package services

import (
	"context"
	"fmt"
	"sync"
)

// Start function of the service. Called on transition from New to Starting state. If start function returns
// error, service goes into Failed state. If startUp returns without error, service transitions into
// Running state (unless context has been canceled), and Run function is called.
//
// serviceContext is a context that is finished at latest when service enters Stopping state, but can also be finished
// earlier when StopAsync is called on the service.
type StartUp func(serviceContext context.Context) error

// Run function is called on transition to Running state. When it returns, service will move to Stopping state.
// If Run or Stopping return error, Service will end in Failed state, otherwise if both functions return without
// error, service will end in Terminated state.
type Run func(serviceContext context.Context) error

// ShutDown function is called on transition to Stopping state. When it returns, service moves to Terminated or Failed state,
// depending on whether there was any error returned from previous Run and this ShutDown function. If both return error,
// Run's error will be saved as failure case for Failed state.
type ShutDown func() error

// BasicService implements contract of Service interface, using three supplied functions: startUp, run and shutDown.
// Even though these functions are called on different gouroutines, they are called sequentially. StartUp is called
// first, then run and shutDown is called last.
//
// All three functions are called at most once.
//
// Context passed to start and run function is cancelled when StopAsync() is called, or service enters Stopping state by
// returning from Run function.
//
// Possible orders of how functions are called:
// 1. startUp, 2. run, 3. shutDown -- this is most common, when startUp doesn't return error, and service is not stopped in startUp.
// 1. startUp, 2. shutDown -- startUp doesn't return error, but StopAsync is called while running startUp, or context is canceled from outside while StartUp still runs.
// 1. startUp -- if startUp returns error, no other functions are called.

type BasicService struct {
	startUp  StartUp
	run      Run
	shutDown ShutDown

	stateMu       sync.Mutex
	state         State
	stopRequested bool

	// closed when state reaches Running, Terminated or Failed state
	runningWaitersCh chan struct{}
	// closed when state reaches Terminated or Failed state
	terminatedWaitersCh chan struct{}

	failureCase error

	serviceContext context.Context
	serviceCancel  context.CancelFunc
}

var invalidServiceState = "invalid service state: %v, expected %v"

func (b *BasicService) NewService(startUp StartUp, run Run, shutDown ShutDown) Service {
	bs := &BasicService{}
	bs.InitBasicService(startUp, run, shutDown)
	return bs
}

// Initializes basic service. Should only be called once. This method is useful when BasicService is embedded as part
// of bigger service structure.
func (b *BasicService) InitBasicService(startUp StartUp, run Run, shutDown ShutDown) {
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
		// if parent context is done, we don't start Run, since Run is supposed to stop on finished context anyway.
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

	// run on new goroutine, so that we can unlock the lock via defer
	go b.doRunService()
}

// Called in Running state (without the lock), this method invokes 'run' function, and handles the result.
func (b *BasicService) doRunService() {
	var err error
	if b.run != nil {
		err = b.run(b.serviceContext)
	} else {
		err = fmt.Errorf("service has no run function")
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
	case Failed: // in failed we can still invoke shutDown
		// don't close anything, it was closed before
	}
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
	b.stateMu.Lock()
	defer b.stateMu.Unlock()

	return b.failureCase
}

func (b *BasicService) State() State {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return b.state
}

var _ Service = &BasicService{}
