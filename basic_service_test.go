package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type serv struct {
	BasicService

	conf           servConf
	startCalled    bool
	runCalled      bool
	shutdownCalled bool
}

type servConf struct {
	startSleep            time.Duration
	startErrOnContextDone bool
	startRetVal           error

	runSleep            time.Duration
	runErrOnContextDone bool
	runRetVal           error

	stopRetVal error
}

func newServ(conf servConf) *serv {
	s := &serv{
		conf: conf,
	}
	s.InitBasicService(s.startUp, s.run, s.shutDown)
	return s
}

func (s *serv) startUp(ctx context.Context) error {
	s.startCalled = true
	select {
	case <-time.After(s.conf.startSleep):
	case <-ctx.Done():
		if s.conf.startErrOnContextDone {
			return ctx.Err()
		}
	}
	return s.conf.startRetVal
}

func (s *serv) run(ctx context.Context) error {
	s.runCalled = true
	select {
	case <-time.After(s.conf.runSleep):
	case <-ctx.Done():
		if s.conf.runErrOnContextDone {
			return ctx.Err()
		}
	}
	return s.conf.runRetVal
}

func (s *serv) shutDown() error {
	s.shutdownCalled = true
	return s.conf.stopRetVal
}

type testCase struct {
	startErrOnContext bool
	startRetVal       error

	runErrOnContext bool
	runRetVal       error

	stopRetVal error

	cancelAfterStartAsync bool
	stopAfterStartAsync   bool

	cancelAfterAwaitRunning bool
	stopAfterAwaitRunning   bool

	// Expected values
	startCalled, runCalled, stopCalled bool
	awaitRunningError                  bool
	awaitTerminatedError               bool
	failureCase                        error
}

func TestStopInNew(t *testing.T) {
	s := newServ(servConf{})

	require.Equal(t, New, s.State())
	s.StopAsync()
	require.Error(t, s.AwaitRunning(context.Background()))
	require.NoError(t, s.AwaitTerminated(context.Background()))
	require.Equal(t, Terminated, s.State())
}

func TestAllFunctionality(t *testing.T) {
	errStartFailed := errors.New("start failed")
	errRunFailed := errors.New("run failed")
	errStopFailed := errors.New("stop failed")

	testCases := map[string]testCase{
		"normal flow": {
			startCalled: true,
			runCalled:   true,
			stopCalled:  true,
		},

		"start returns error": {
			startRetVal:          errStartFailed,
			awaitRunningError:    true,
			awaitTerminatedError: true, // Failed in start
			startCalled:          true,
			failureCase:          errStartFailed,
		},

		"start is canceled via context and returns cancelation error": {
			cancelAfterStartAsync: true,
			startErrOnContext:     true,
			awaitRunningError:     true,
			awaitTerminatedError:  true, // Failed in start
			startCalled:           true,
			failureCase:           context.Canceled,
		},

		"start is canceled via context, doesn't return error. Run shouldn't run, since context is canceled now.": {
			cancelAfterStartAsync: true,
			startErrOnContext:     false,
			awaitRunningError:     true,  // will never be Running
			awaitTerminatedError:  false, // but still terminates correctly, since Start or Run didn't return error
			startCalled:           true,
			runCalled:             false,
			stopCalled:            true,
			failureCase:           nil, // start didn't return error, service stopped without calling run
		},

		"start is canceled via StopAsync, but start doesn't return error": {
			startErrOnContext:    false, // don't return error on cancellation, just stop early
			stopAfterStartAsync:  true,
			awaitRunningError:    true,
			awaitTerminatedError: false, // stopped while starting, but no error. Should be in Terminated state.
			startCalled:          true,
			stopCalled:           true,
			failureCase:          nil, // start didn't return error, service stopped without calling run
		},

		"run returns error": {
			runRetVal:            errRunFailed,
			awaitTerminatedError: true, // service will get into Failed state, since run failed
			startCalled:          true,
			runCalled:            true,
			stopCalled:           true,
			failureCase:          errRunFailed,
		},

		"run returns error from context cancelation": {
			runErrOnContext:         true,
			cancelAfterAwaitRunning: true,
			awaitTerminatedError:    true, // service will get into Failed state, since run failed
			startCalled:             true,
			runCalled:               true,
			stopCalled:              true,
			failureCase:             context.Canceled,
		},

		"run and stop both return error, only one is reported": {
			runRetVal:            errRunFailed,
			stopRetVal:           errStopFailed,
			awaitTerminatedError: true, // service will get into Failed state, since run failed
			startCalled:          true,
			runCalled:            true,
			stopCalled:           true,
			failureCase:          errRunFailed, // run fails first, its error is returned
		},

		"stop returns error": {
			runRetVal:            nil,
			stopRetVal:           errStopFailed,
			awaitTerminatedError: true, // service will get into Failed state, since stop fails
			startCalled:          true,
			runCalled:            true,
			stopCalled:           true,
			failureCase:          errStopFailed,
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			runTestCase(t, tc)
		})
	}
}

func runTestCase(t *testing.T, tc testCase) {
	t.Parallel()

	s := newServ(servConf{
		startSleep:            time.Second,
		startRetVal:           tc.startRetVal,
		startErrOnContextDone: tc.startErrOnContext,
		runSleep:              time.Second,
		runRetVal:             tc.runRetVal,
		runErrOnContextDone:   tc.runErrOnContext,
		stopRetVal:            tc.stopRetVal,
	})

	require.Equal(t, New, s.State())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // make sure to call cancel at least once

	require.NoError(t, s.StartAsync(ctx), "StartAsync")
	require.Error(t, s.StartAsync(ctx), "second StartAsync") // must always return error
	if tc.cancelAfterStartAsync {
		cancel()
	}
	if tc.stopAfterStartAsync {
		s.StopAsync()
	}

	if tc.awaitRunningError {
		require.Error(t, s.AwaitRunning(context.Background()), "AwaitRunning")
	} else {
		require.NoError(t, s.AwaitRunning(context.Background()), "AwaitRunning")
	}

	if tc.cancelAfterAwaitRunning {
		cancel()
	}
	if tc.stopAfterAwaitRunning {
		s.StopAsync()
	}

	if tc.awaitTerminatedError {
		require.Error(t, s.AwaitTerminated(context.Background()), "AwaitTerminated")
	} else {
		require.NoError(t, s.AwaitTerminated(context.Background()), "AwaitTerminated")
	}

	require.Equal(t, tc.startCalled, s.startCalled, "startCalled")
	require.Equal(t, tc.runCalled, s.runCalled, "runCalled")
	require.Equal(t, tc.stopCalled, s.shutdownCalled, "stopCalled")
	require.Equal(t, tc.failureCase, s.FailureCase(), "FailureCase")
}
