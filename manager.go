package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type managerState int

const (
	unknown managerState = iota // initial state
	healthy                     // all services running
	stopped                     // all services stopped (failed or terminated)
)

// Service Manager is initialized with a collection of services. They all must be in New state.
// Service manager can start them, and observe their state as a group.
// Once all services are running, Manager is said to be Healthy. It is possible for manager to never reach the Healthy state, if some services fail to start.
// When all services are stopped (Terminated or Failed), manager is Stopped.
type Manager struct {
	services []Service

	healthyCh chan struct{}
	stoppedCh chan struct{}

	mu            sync.Mutex
	state         managerState
	byState       map[State][]Service // Services sorted by state
	healthyClosed bool                // was healthyCh closed already?
}

// NewManager creates new service manager. It needs at least one service, and all services must be in New state.
func NewManager(services []Service) (*Manager, error) {
	if len(services) == 0 {
		return nil, errors.New("no services")
	}

	m := &Manager{
		services:  services,
		byState:   map[State][]Service{},
		healthyCh: make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}

	for _, s := range services {
		st := s.State()
		if st != New {
			return nil, fmt.Errorf("unexpected service %v state: %v", s, st)
		}

		m.byState[st] = append(m.byState[st], s)
	}

	for _, s := range services {
		s.AddListener(newManagerServiceListener(m, s))
	}
	return m, nil
}

// Initiates service startup on all the services being managed. It is only valid to call this method if all of the services are New.
func (m *Manager) StartAsync(ctx context.Context) error {
	for _, s := range m.services {
		err := s.StartAsync(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

// Initiates service shutdown if necessary on all the services being managed.
func (m *Manager) StopAsync() {
	for _, s := range m.services {
		s.StopAsync()
	}
}

// Returns true if all services are currently in the Running state.
func (m *Manager) IsHealthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.state == healthy
}

// Waits for the ServiceManager to become healthy. Returns nil, if manager is healthy, error otherwise.
func (m *Manager) AwaitHealthy(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.healthyCh:
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == healthy {
		return nil
	} else {
		// must have been healthy before, since healthyCh is closed
		return errors.New("not healthy anymore")
	}
}

// Returns true if all services are in terminal state (Terminated or Failed)
func (m *Manager) IsStopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.state == stopped
}

// Waits for the ServiceManager to become stopped. Returns nil, if manager is stopped, error when context finishes earlier.
func (m *Manager) AwaitStopped(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.stoppedCh:
		return nil
	}
}

// Provides a snapshot of the current state of all the services under management.
func (m *Manager) ServicesByState() map[State][]Service {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := map[State][]Service{}
	for st, ss := range m.byState {
		result[st] = append([]Service(nil), ss...) // make a copy
	}
	return result
}

func (m *Manager) serviceStateChanged(s Service, from State, to State) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fs := m.byState[from]
	for ix, ss := range fs {
		if s == ss {
			fs = append(fs[:ix], fs[ix+1:]...)
			break
		}
	}
	if len(fs) == 0 {
		delete(m.byState, from)
	} else {
		m.byState[from] = fs
	}

	m.byState[to] = append(m.byState[to], s)

	running := len(m.byState[Running])
	stopping := len(m.byState[Stopping])
	done := len(m.byState[Terminated]) + len(m.byState[Failed])

	all := len(m.services)

	switch {
	case running == all:
		close(m.healthyCh)
		m.state = healthy
		m.healthyClosed = true

	case done == all:
		if !m.healthyClosed {
			// healthy cannot be reached anymore
			close(m.healthyCh)
			m.healthyClosed = true
		}
		close(m.stoppedCh) // happens at most once
		m.state = stopped

	default:
		if !m.healthyClosed && (done > 0 || stopping > 0) {
			// healthy cannot be reached anymore
			close(m.healthyCh)
			m.healthyClosed = true
		}

		m.state = unknown
	}
}

func newManagerServiceListener(m *Manager, s Service) *managerServiceListener {
	return &managerServiceListener{m: m, s: s}
}

// managerServiceListener is a service listener, that updates Service state in the Manager
type managerServiceListener struct {
	m *Manager
	s Service
}

func (l managerServiceListener) Starting() {
	l.m.serviceStateChanged(l.s, New, Starting)
}

func (l managerServiceListener) Running() {
	l.m.serviceStateChanged(l.s, Starting, Running)
}

func (l managerServiceListener) Stopping(from State) {
	l.m.serviceStateChanged(l.s, from, Stopping)
}

func (l managerServiceListener) Terminated(from State) {
	l.m.serviceStateChanged(l.s, from, Terminated)
}

func (l managerServiceListener) Failed(from State, failure error) {
	l.m.serviceStateChanged(l.s, from, Failed)
}
