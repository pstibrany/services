package services

import (
	"context"
	"time"
)

func NewIdleService(up StartUp, down ShutDown) Service {
	bs := &BasicService{}
	InitIdleService(bs, up, down)
	return bs
}

// Initializes basic service as an "idle" service -- it doesn't do anything in its Running state,
// but still supports all state transitions.
func InitIdleService(bs *BasicService, up StartUp, down ShutDown) {
	bs.InitBasicService(up, func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}, down)
}

// One iteration of the timer service. Called repeatedly until service is stopped, or this function returns error
// in which case, service will fail.
type OneIteration func(ctx context.Context) error

func NewTimerService(interval time.Duration, up StartUp, down ShutDown, iter OneIteration) Service {
	bs := &BasicService{}
	InitTimerService(bs, interval, up, down, iter)
	return bs
}

// Runs iteration function on every interval tick. When iteration returns error, service fails.
func InitTimerService(bs *BasicService, interval time.Duration, up StartUp, down ShutDown, iter OneIteration) {
	bs.InitBasicService(up, func(ctx context.Context) error {
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
