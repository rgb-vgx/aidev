package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aidev/internal/task"
)

// The watcher stops a run only on a status it actually read. A database that is
// briefly unreachable must not kill every run in progress: an error is logged
// and the next poll tries again.
func TestWatchForCancelIgnoresReadErrors(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	read := func(context.Context) (task.Status, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		switch {
		case calls <= 3:
			return "", errors.New("connection refused")
		case calls <= 6:
			return task.StatusRunning, nil
		default:
			return task.StatusCancelled, nil
		}
	}
	var stopped atomic.Int32
	stop := func() { stopped.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		watchForCancel(ctx, 5*time.Millisecond, read, stop, nil)
		close(finished)
	}()

	deadline := time.After(5 * time.Second)
	for stopped.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the watcher never stopped the run after reading CANCELLED")
		case <-time.After(5 * time.Millisecond):
		}
	}
	mu.Lock()
	if calls < 7 {
		t.Errorf("stopped after %d reads; the read errors and RUNNING must not stop the run", calls)
	}
	mu.Unlock()

	// Having stopped the run, the watcher has nothing left to do.
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("the watcher kept running after it stopped the run")
	}
	if n := stopped.Load(); n != 1 {
		t.Errorf("stop called %d times, want once", n)
	}
}

// The watcher ends with the run it watches, without stopping it.
func TestWatchForCancelEndsWithItsContext(t *testing.T) {
	read := func(context.Context) (task.Status, error) { return task.StatusRunning, nil }
	var stopped atomic.Int32

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		watchForCancel(ctx, time.Millisecond, read, func() { stopped.Add(1) }, nil)
		close(finished)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("the watcher outlived its context")
	}
	if stopped.Load() != 0 {
		t.Error("the watcher stopped a run that was never cancelled")
	}
}
