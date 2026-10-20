package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUpdateIdleNeverForcesUnknownOrBusyRuntime(t *testing.T) {
	err := waitUpdateIdle(context.Background(), 30*time.Millisecond, time.Millisecond, 3*time.Millisecond,
		func(context.Context) (bool, error) { return false, nil },
		func() error { t.Fatal("busy runtime was updated"); return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected waiting result: %v", err)
	}
}

func TestUpdateIdleRechecksAfterNewWorkAndStopsOnRealError(t *testing.T) {
	probes, applies := 0, 0
	failure := errors.New("backup unavailable")
	err := waitUpdateIdle(context.Background(), time.Second, time.Millisecond, 3*time.Millisecond,
		func(context.Context) (bool, error) { probes++; return probes != 2, nil },
		func() error {
			applies++
			if probes < 4 {
				t.Fatal("did not wait for continuous idle")
			}
			if applies == 1 {
				return errUpdateBusy
			}
			return failure
		})
	if !errors.Is(err, failure) || applies != 2 {
		t.Fatalf("unexpected update retry: applies=%d err=%v", applies, err)
	}
}

func TestUpdateIdleCancellationDuringProbeDoesNotApply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := waitUpdateIdle(ctx, time.Second, time.Millisecond, 0,
		func(context.Context) (bool, error) { cancel(); return true, nil },
		func() error { t.Fatal("cancelled update applied"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestUpdateIdleChangedDeploymentStopsWaiting(t *testing.T) {
	failure := errors.New("deployment removed")
	err := waitUpdateIdle(context.Background(), time.Second, time.Millisecond, 0,
		func(context.Context) (bool, error) { return false, failure },
		func() error { t.Fatal("invalid deployment updated"); return nil })
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
}
