package cli

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The MCP server connects to the database on the first tool call, and the command
// closes what was opened when the server stops. An OpenCode reviewer reading
// TASK-000032 found the seam between those two: the close reads the opened app once
// after Serve returns, so an attempt that succeeds later is never closed — the pool
// stays open and the traces are never flushed (docs/research.md 7g). The production
// binary exits anyway; a test or a future in-process server does not.
//
// These tests describe the small holder that owns that lifecycle: lazyApp.

func TestLazyAppOpensOnceAndClosesOnce(t *testing.T) {
	var opens, closes atomic.Int32
	lazy := newLazyApp(func(ctx context.Context) (*app, error) {
		opens.Add(1)
		return &app{close: func() { closes.Add(1) }}, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := lazy.get(context.Background()); err != nil {
				t.Errorf("get: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := opens.Load(); n != 1 {
		t.Errorf("opened %d times for 5 concurrent callers, want 1", n)
	}

	lazy.close()
	lazy.close() // closing twice must not close the app twice
	if n := closes.Load(); n != 1 {
		t.Errorf("closed %d times, want exactly 1", n)
	}
}

// close must wait for an attempt already in flight, or the app it returns is never
// closed at all.
func TestLazyAppCloseWaitsForAnAttemptInFlight(t *testing.T) {
	var closes atomic.Int32
	started := make(chan struct{})
	lazy := newLazyApp(func(ctx context.Context) (*app, error) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		return &app{close: func() { closes.Add(1) }}, nil
	})

	go func() { _, _ = lazy.get(context.Background()) }()
	<-started
	lazy.close()

	if n := closes.Load(); n != 1 {
		t.Errorf("closed %d times after close raced an attempt in flight, want 1: the app was left open", n)
	}
}

// A failure is not remembered: the next call tries again, as the MCP server needs
// when a database is still starting.
func TestLazyAppRetriesAfterAFailure(t *testing.T) {
	var opens atomic.Int32
	lazy := newLazyApp(func(ctx context.Context) (*app, error) {
		if opens.Add(1) == 1 {
			return nil, context.DeadlineExceeded
		}
		return &app{close: func() {}}, nil
	})

	if _, err := lazy.get(context.Background()); err == nil {
		t.Fatal("the first get returned no error")
	}
	if _, err := lazy.get(context.Background()); err != nil {
		t.Fatalf("the second get: %v", err)
	}
	if n := opens.Load(); n != 2 {
		t.Errorf("opened %d times, want 2: a failure must not be cached and a success must be", n)
	}
	lazy.close()
}
