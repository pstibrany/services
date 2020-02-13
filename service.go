package services

import (
	"context"
	"fmt"
)

// State of the service. Service starts in New state, and (optionally) goes through Starting, Running, Stopping to either Terminated or Failed.
type State int

const (
	New        State = iota // Service is new, not running yet. Initial state.
	Starting                // Service is starting. If starting succeeds, service enters Running state.
	Running                 // Service is fully running now. When service stops running, it enters Stopping state.
	Stopping                // Service is shutting down
	Terminated              // Service has stopped successfully. Final state.
	Failed                  // Service has failed in Starting, Running or Stopping state. Final state.
)

func (s State) String() string {
	switch s {
	case New:
		return "New"
	case Starting:
		return "Starting"
	case Running:
		return "Running"
	case Stopping:
		return "Stopping"
	case Terminated:
		return "Terminated"
	case Failed:
		return "Failed"
	default:
		return fmt.Sprintf("Unknown state: %d", s)
	}
}

// Service defines interface for controlling a service.
type Service interface {
	// Starts Service asynchronously. Service must be in New State, otherwise error is returned.
	// Context is used as a parent context for service own context.
	StartAsync(ctx context.Context) error

	// Waits until service gets into Running state. If service is already in Running state, returns immediately.
	// If service is in a state, from which it cannot get into Running state, error is returned immediately.
	// This method is blocking, if service is in New or Starting state.
	AwaitRunning(ctx context.Context) error

	// If Service is New, it is Terminated without having been started nor stopped.
	// If Service is in Starting or Running state, this initiates shutdown and returns immediately.
	// If Service has already been stopped, this method returns immediately, without taking action.
	StopAsync()

	// Waits for the service to reach Terminated state. If service enters this state, this method returns nil.
	// If service enters Failed state (or context is finished before), error is returned.
	AwaitTerminated(ctx context.Context) error

	// If Service is in Failed state, this method returns the reason. It returns nil, if Service is not in Failed state.
	FailureCase() error

	// Returns current state of the service.
	State() State

	// Adds listener to this service. Listener will be notified on subsequent state transitions. Previous state
	// transitions are not replayed.
	AddListener(listener Listener)
}

type Listener interface {
	// Called when the service transitions from NEW to STARTING.
	Starting()

	// Called when the service transitions from STARTING to RUNNING.
	Running()

	// Called when the service transitions to the STOPPING state.
	Stopping(from State)

	// Called when the service transitions to the TERMINATED state.
	Terminated(from State)

	// Called when the service transitions to the FAILED state.
	Failed(from State, failure error)
}
