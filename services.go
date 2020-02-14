package services

import (
	"context"
	"time"
)

func NewIdleService(up StartingFn, down StoppingFn) Service {
	bs := &BasicService{}
	InitIdleService(bs, up, down)
	return bs
}

// Initializes basic service as an "idle" service -- it doesn't do anything in its Running state,
// but still supports all state transitions.
func InitIdleService(bs *BasicService, start StartingFn, stop StoppingFn) {
	InitBasicService(bs, start, func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}, stop)
}

// One iteration of the timer service. Called repeatedly until service is stopped, or this function returns error
// in which case, service will fail.
type OneIteration func(ctx context.Context) error

func NewTimerService(interval time.Duration, up StartingFn, down StoppingFn, iter OneIteration) Service {
	bs := &BasicService{}
	InitTimerService(bs, interval, up, down, iter)
	return bs
}

// Runs iteration function on every interval tick. When iteration returns error, service fails.
func InitTimerService(bs *BasicService, interval time.Duration, up StartingFn, down StoppingFn, iter OneIteration) {
	InitBasicService(bs, up, func(ctx context.Context) error {
		t := time.NewTicker(interval)

		for {
			select {
			case <-t.C:
				err := iter(ctx)
				if err != nil {
					return err
				}

			case <-ctx.Done():
				return nil
			}
		}
	}, down)
}
