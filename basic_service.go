package services

import (
	"context"
	"fmt"
	"sync"
)

// StartingFn is called when service enters Starting state. If StartingFn returns
// error, service goes into Failed state. If StartingFn returns without error, service transitions into
// Running state (unless context has been canceled).
//
// serviceContext is a context that is finished at latest when service enters Stopping state, but can also be finished
// earlier when StopAsync is called on the service. This context is derived from context passed to StartAsync method.
type StartingFn func(serviceContext context.Context) error

// RunningFn function is called when service enters Running state. When it returns, service will move to Stopping state.
// If RunningFn or Stopping return error, Service will end in Failed state, otherwise if both functions return without
// error, service will end in Terminated state.
type RunningFn func(serviceContext context.Context) error

// StoppingFn function is called when service enters Stopping state. When it returns, service moves to Terminated or Failed state,
// depending on whether there was any error returned from previous RunningFn (if it was called) and this StoppingFn function. If both return error,
// RunningFn's error will be saved as failure case for Failed state.
type StoppingFn func() error

// BasicService implements contract of Service interface, using three supplied functions: StartingFn, RunningFn and StoppingFn.
// Even though these functions are called on different gouroutines, they are called sequentially and they don't need to synchronize
// access on the state. (In other words: StartingFn happens-before RunningFn, RunningFn happens-before StoppingFn).
//
// All three functions are called at most once. If they are nil, they are not called and service transitions to the next state.
//
// Context passed to start and run function is canceled when StopAsync() is called, or service enters Stopping state.
//
// Possible orders of how functions are called:
//
// * 1. StartingFn – if StartingFn returns error, no other functions are called.
//
// * 1. StartingFn, 2. StoppingFn – StartingFn doesn't return error, but StopAsync is called while running
// StartingFn, or context is canceled from outside while StartingFn still runs.
//
// * 1. StartingFn, 2. RunningFn, 3. StoppingFn – this is most common, when StartingFn doesn't return error,
// service is not stopped and context isn't stopped externally while running StartingFn.
type BasicService struct {
	// functions only run, if they are not nil. If functions are nil, service will effectively do nothing
	// in given state, and go to the next one without any error.
	startUp  StartingFn
	run      RunningFn
	shutDown StoppingFn

	// everything below is protected by this mutex
	stateMu       sync.RWMutex
	state         State
	stopRequested bool
	failureCase   error
	listeners     []chan func(l Listener)

	// closed when state reaches Running, Terminated or Failed state
	runningWaitersCh chan struct{}
	// closed when state reaches Terminated or Failed state
	terminatedWaitersCh chan struct{}

	serviceContext context.Context
	serviceCancel  context.CancelFunc
}

var invalidServiceState = "invalid service state: %v, expected %v"

// Returns service built from three functions (using BasicService).
func NewService(startUp StartingFn, run RunningFn, shutDown StoppingFn) Service {
	bs := &BasicService{}
	InitBasicService(bs, startUp, run, shutDown)
	return bs
}

// Initializes basic service. Should only be called once. This method is useful when
// BasicService is embedded as part of bigger service structure.
func InitBasicService(b *BasicService, startUp StartingFn, run RunningFn, shutDown StoppingFn) {
	*b = BasicService{
		startUp:             startUp,
		run:                 run,
		shutDown:            shutDown,
		state:               New,
		runningWaitersCh:    make(chan struct{}),
		terminatedWaitersCh: make(chan struct{}),
	}
}

func (b *BasicService) StartAsync(parentContext context.Context) error {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()

	if b.state != New {
		return fmt.Errorf(invalidServiceState, b.state, New)
	}

	b.state = Starting
	b.serviceContext, b.serviceCancel = context.WithCancel(parentContext)
	b.notifyListeners(func(l Listener) { l.Starting() }, false)
	go b.doStartService()

	return nil
}

// Called in Starting state (without the lock), this method invokes 'startUp' function, and handles the result.
func (b *BasicService) doStartService() {
	var err error = nil
	if b.startUp != nil {
		err = b.startUp(b.serviceContext)
	}

	b.stateMu.Lock()
	defer b.stateMu.Unlock()

	if err != nil {
		b.serviceCancel()

		// pass current state, so that transition knows which channels to close
		b.transitionToFailed(err)
	} else if b.stopRequested || b.serviceContext.Err() != nil {
		// if parent context is done, we don't start RunningFn, since RunningFn is supposed to stop on finished context anyway.
		b.transitionToStopping(nil)
	} else {
		b.transitionToRunning()
	}
}

// called with lock
func (b *BasicService) transitionToRunning() {
	b.state = Running

	// unblock waiters waiting for Running state
	close(b.runningWaitersCh)

	b.notifyListeners(func(l Listener) { l.Running() }, false)

	// run on new goroutine, so that we can unlock the lock via defer
	go b.doRunService()
}

// Called in Running state (without the lock), this method invokes 'run' function, and handles the result.
func (b *BasicService) doRunService() {
	var err error
	if b.run != nil {
		err = b.run(b.serviceContext)
	}

	b.stateMu.Lock()
	defer b.stateMu.Unlock()

	b.transitionToStopping(err)
}

// called with lock
func (b *BasicService) transitionToStopping(failure error) {
	from := b.state
	if from == Starting {
		// we cannot reach Running, so we can unblock waiters
		close(b.runningWaitersCh)
	}

	b.state = Stopping
	b.serviceCancel()
	b.notifyListeners(func(l Listener) { l.Stopping(from) }, false)

	go b.doStopService(failure)
}

// Called in Stopping or Failed state, after run has finished. Called without the lock, this method invokes shutDown function and handles the result.
func (b *BasicService) doStopService(failure error) {
	var err error
	if b.shutDown != nil {
		err = b.shutDown()
	}
	if failure != nil {
		// ignore error return by shutDown
		err = failure
	}

	b.stateMu.Lock()
	defer b.stateMu.Unlock()

	if err != nil {
		b.transitionToFailed(err)
	} else {
		b.transitionToTerminated()
	}
}

// called with lock
func (b *BasicService) transitionToFailed(err error) {
	from := b.state

	b.state = Failed
	if b.failureCase == nil {
		// if we have multiple failures (eg. both run and stop return failure), we only keep the first one
		b.failureCase = err
	}

	switch from {
	case Starting:
		close(b.runningWaitersCh)
		close(b.terminatedWaitersCh)
	case Running, Stopping:
		// cannot close runningWaitersCh, as it has already been closed when
		// transitioning to Running or Stopping state
		close(b.terminatedWaitersCh)
	}

	b.notifyListeners(func(l Listener) { l.Failed(from, err) }, true)
}

// called with lock
func (b *BasicService) transitionToTerminated() {
	from := b.state

	b.state = Terminated
	if from == New {
		// we will not reach Running state now, so we can unblock waiters.
		close(b.runningWaitersCh)
	}

	close(b.terminatedWaitersCh)

	b.notifyListeners(func(l Listener) { l.Terminated(from) }, true)
}

func (b *BasicService) StopAsync() {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()

	b.stopRequested = true
	if b.serviceCancel != nil {
		b.serviceCancel()
	}

	if b.state == New {
		b.transitionToTerminated()
	}
}

func (b *BasicService) AwaitRunning(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.runningWaitersCh:
	}

	s := b.State()
	if s == Running {
		return nil
	}

	return fmt.Errorf(invalidServiceState, s, Running)
}

func (b *BasicService) AwaitTerminated(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.terminatedWaitersCh:
	}

	s := b.State()
	if s == Terminated {
		return nil
	}

	return fmt.Errorf(invalidServiceState, s, Terminated)
}

func (b *BasicService) FailureCase() error {
	b.stateMu.RLock()
	defer b.stateMu.RUnlock()

	return b.failureCase
}

func (b *BasicService) State() State {
	b.stateMu.RLock()
	defer b.stateMu.RUnlock()
	return b.state
}

func (b *BasicService) AddListener(listener Listener) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()

	if b.state == Terminated || b.state == Failed {
		// no more state transitions will be done, and channel wouldn't get closed
		return
	}

	// There are max 4 state transitions. We use buffer to avoid blocking the sender,
	// which holds service lock.
	ch := make(chan func(l Listener), 4)
	b.listeners = append(b.listeners, ch)

	// each listener has its own goroutine, processing events.
	go func() {
		for lfn := range ch {
			lfn(listener)
		}
	}()
}

// lock must be held here. Read lock would be good enough, but since
// this is called from transition methods, full lock is used.
func (b *BasicService) notifyListeners(lfn func(l Listener), closeChan bool) {
	for _, ch := range b.listeners {
		ch <- lfn
		if closeChan {
			close(ch)
		}
	}
}

var _ Service = &BasicService{}
