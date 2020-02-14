package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNoServices(t *testing.T) {
	_, err := NewManager(nil)
	require.Error(t, err)
}

func TestBasicManagerTransitions(t *testing.T) {
	s1 := serviceThatDoesntDoAnything()
	s2 := serviceThatDoesntDoAnything()
	s3 := serviceThatDoesntDoAnything()

	m, err := NewManager([]Service{s1, s2, s3})
	require.NoError(t, err)

	states := m.ServicesByState()
	for s, ss := range states {
		require.Equal(t, New, s)
		require.Len(t, ss, 3)
	}

	require.False(t, m.IsHealthy())
	require.False(t, m.IsStopped())
	require.NoError(t, m.StartAsync(context.Background()))
	require.NoError(t, m.AwaitHealthy(context.Background()))
	require.True(t, m.IsHealthy())
	require.False(t, m.IsStopped())

	states = m.ServicesByState()
	for s, ss := range states {
		require.Equal(t, Running, s)
		require.Len(t, ss, 3)
	}

	m.StopAsync()
	require.NoError(t, m.AwaitStopped(context.Background()))
	require.False(t, m.IsHealthy())
	require.True(t, m.IsStopped())

	states = m.ServicesByState()
	for s, ss := range states {
		require.Equal(t, Terminated, s)
		require.Len(t, ss, 3)
	}
}

func TestManagerRequiresServicesToBeInNewState(t *testing.T) {
	s1 := serviceThatDoesntDoAnything()
	s2 := serviceThatDoesntDoAnything()
	s3 := serviceThatDoesntDoAnything()

	require.NoError(t, s1.StartAsync(context.Background()))

	_, err := NewManager([]Service{s1, s2, s3})
	require.Error(t, err) // s1 is not New anymore
}

func TestManagerReactsOnExternalStateChanges(t *testing.T) {
	s1 := serviceThatDoesntDoAnything()
	s2 := serviceThatDoesntDoAnything()
	s3 := serviceThatDoesntDoAnything()

	m, err := NewManager([]Service{s1, s2, s3})
	require.NoError(t, err)

	require.NoError(t, s1.StartAsync(context.Background()))
	require.NoError(t, s2.StartAsync(context.Background()))
	s3.StopAsync()
	require.Error(t, m.AwaitHealthy(context.Background())) // must return error as soon as s3's transition to Terminated is observed

	states := m.ServicesByState()
	// s3 is Stopping or Terminated
	require.ElementsMatch(t, mergeStates(states, Stopping, Terminated), []Service{s3})

	// s1, s2 are New (if change to Starting wasn't observed yet), Starting or Running
	require.ElementsMatch(t, mergeStates(states, New, Starting, Running), []Service{s1, s2})

	m.StopAsync()
	require.NoError(t, m.AwaitStopped(context.Background()))

	states = m.ServicesByState()
	require.ElementsMatch(t, states[Terminated], []Service{s1, s2, s3})
}

func TestManagerGoesToStoppedStateBeforeBeingHealthyOrEvenStarting(t *testing.T) {
	// by using single service, as soon as it is terminated, manager will be done.
	// we test that healthy waiters are notified
	s1 := serviceThatDoesntDoAnything()

	m, err := NewManager([]Service{s1})
	require.NoError(t, err)
	require.False(t, m.IsStopped())

	s1.StopAsync()

	// will never get healthy
	require.Error(t, m.AwaitHealthy(context.Background()))
	// all services are stopped, so we need to reach Stopped state
	require.NoError(t, m.AwaitStopped(context.Background()))
	require.True(t, m.IsStopped())

	states := m.ServicesByState()
	require.ElementsMatch(t, states[Terminated], []Service{s1})
}

func TestManagerCannotStartIfAllServicesArentNew(t *testing.T) {
	s1 := serviceThatDoesntDoAnything()
	s2 := serviceThatDoesntDoAnything()
	s3 := serviceThatDoesntDoAnything()

	m, err := NewManager([]Service{s1, s2, s3})
	require.NoError(t, err)

	require.NoError(t, s3.StartAsync(context.Background()))
	require.Error(t, m.StartAsync(context.Background())) // cannot start now

	m.StopAsync()
	require.NoError(t, m.AwaitStopped(context.Background()))
}

func TestManagerThatFailsToStart(t *testing.T) {
	s1 := serviceThatDoesntDoAnything()
	s2 := serviceThatDoesntDoAnything()
	s3 := serviceThatFailsToStart()

	m, err := NewManager([]Service{s1, s2, s3})
	require.NoError(t, err)

	states := m.ServicesByState()
	require.Equal(t, states, map[State][]Service{New: {s1, s2, s3}})

	require.NoError(t, m.StartAsync(context.Background()))
	require.Error(t, m.AwaitHealthy(context.Background())) // will never get healthy, since one service fails to start

	states = m.ServicesByState()
	// check that failed state contains s3.
	require.ElementsMatch(t, states[Failed], []Service{s3})

	// s1 and s2 can be in New (if Starting state transition wasn't observed yet), Starting or Running state.
	require.ElementsMatch(t, mergeStates(states, New, Starting, Running), []Service{s1, s2})

	// stop remaining services
	m.StopAsync()
	require.NoError(t, m.AwaitStopped(context.Background()))

	states = m.ServicesByState()
	equalStatesMap(t, states, map[State][]Service{
		Terminated: {s1, s2},
		Failed:     {s3},
	})
}

func TestManagerEntersStopStateEventually(t *testing.T) {
	s1 := serviceThatStopsOnItsOwnAfterTimeout(200 * time.Millisecond)
	s2 := serviceThatStopsOnItsOwnAfterTimeout(300 * time.Millisecond)

	m, err := NewManager([]Service{s1, s2})
	require.NoError(t, err)

	// start all services
	require.NoError(t, m.StartAsync(context.Background()))

	require.NoError(t, m.AwaitHealthy(context.Background()))

	// both services stop after short time, so this manager will become stopped
	require.NoError(t, m.AwaitStopped(context.Background()))
}

func TestManagerAwaitWithContextCancellation(t *testing.T) {
	s1 := serviceThatStopsOnItsOwnAfterTimeout(200 * time.Millisecond)
	s2 := serviceThatStopsOnItsOwnAfterTimeout(300 * time.Millisecond)

	m, err := NewManager([]Service{s1, s2})
	require.NoError(t, err)

	// test context cancellation
	stoppedContext, cancel := context.WithCancel(context.Background())
	cancel()

	require.Equal(t, context.Canceled, m.AwaitHealthy(stoppedContext)) // since no services have started yet, and context is stopped, must return error quickly

	// start all services
	require.NoError(t, m.StartAsync(context.Background()))

	shortContext, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	require.Equal(t, context.DeadlineExceeded, m.AwaitStopped(shortContext))
	require.NoError(t, m.AwaitStopped(context.Background()))
}

func mergeStates(m map[State][]Service, states ...State) []Service {
	result := []Service(nil)
	for _, s := range states {
		result = append(result, m[s]...)
	}
	return result
}

func equalStatesMap(t *testing.T, m1, m2 map[State][]Service) {
	t.Helper()

	states := []State{New, Starting, Running, Stopping, Terminated, Failed}

	for _, s := range states {
		require.ElementsMatch(t, m1[s], m2[s], s)
	}
}

func serviceThatFailsToStart() Service {
	return NewService(func(serviceContext context.Context) error {
		return errors.New("failed to start")
	}, nil, nil)
}

func serviceThatStopsOnItsOwnAfterTimeout(timeout time.Duration) Service {
	return NewService(nil, func(serviceContext context.Context) error {
		select {
		case <-serviceContext.Done():
			return nil
		case <-time.After(timeout):
			return nil
		}
	}, nil)
}

func serviceThatDoesntDoAnything() Service {
	// but respects StopAsync and stops.
	return NewIdleService(nil, nil)
}
