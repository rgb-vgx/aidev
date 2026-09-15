package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"aidev/internal/logging"
	"aidev/internal/store"
	"aidev/internal/worker"
)

// An OpenCode reviewer reading TASK-000032 found two things about the deferred
// connection that its own tests did not (docs/research.md 7g), both reproduced
// against the built binary:
//
//   - every handler connects before looking at its input, so while the database is
//     down a malformed request is answered with "the database is not reachable"
//     instead of what is wrong with the request;
//   - concurrent first calls do not share one attempt. The mutex serialises them,
//     so N callers pay N connection timeouts one after another, and a caller whose
//     context is already cancelled still waits for the attempt in flight.

// failingOpener counts its calls, blocks for delay, and always fails.
func failingOpener(delay time.Duration, calls *atomic.Int32) Opener {
	return func(ctx context.Context) (*worker.Orchestrator, *store.Store, error) {
		calls.Add(1)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return nil, nil, errors.New("database is not reachable: simulated")
	}
}

// session starts the server over an in-memory transport and returns a client session.
func session(t *testing.T, server *Server) *sdk.ClientSession {
	t.Helper()
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.ServeTransport(ctx, serverTransport) }()

	client, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).
		Connect(context.Background(), clientTransport, nil)
	if err != nil {
		cancel()
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		select {
		case <-served:
		case <-time.After(30 * time.Second):
			t.Error("the server did not shut down")
		}
	})
	return client
}

func callError(t *testing.T, s *sdk.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := s.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("%s succeeded, want an error: %+v", name, res.StructuredContent)
	}
	var parts []string
	for _, c := range res.Content {
		if text, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, " ")
}

// A request that could never work does not wait on the database, and says what is
// wrong with the request.
func TestInputIsCheckedBeforeConnecting(t *testing.T) {
	cases := []struct {
		name, tool string
		args       map[string]any
		wants      string
	}{
		{"no verification command", ToolCreateTask,
			map[string]any{"repo_path": t.TempDir(), "title": "x", "verification": []string{}}, "verification"},
		{"unknown status filter", ToolListTasks,
			map[string]any{"statuses": []string{"NONSENSE"}}, "NONSENSE"},
		{"negative wait", ToolRunTask,
			map[string]any{"task": "TASK-000001", "wait_seconds": -5}, "wait_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			s := session(t, NewDeferred(failingOpener(0, &calls), "test", logging.Discard()))

			msg := callError(t, s, tc.tool, tc.args)
			if !strings.Contains(msg, tc.wants) {
				t.Errorf("error = %q, want it to mention %q rather than the database", msg, tc.wants)
			}
			if strings.Contains(msg, "not reachable") {
				t.Errorf("error = %q: the database was consulted for a request that cannot work", msg)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("the database was opened %d time(s) for invalid input; want 0", n)
			}
		})
	}
}

// Claude Code issues tool calls in parallel. Five first calls while the database is
// down must cost one attempt, not five in a queue.
func TestConcurrentFirstCallsShareOneAttempt(t *testing.T) {
	var calls atomic.Int32
	const attempt = 300 * time.Millisecond
	s := session(t, NewDeferred(failingOpener(attempt, &calls), "test", logging.Discard()))

	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]string, 5)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = callError(t, s, ToolListTasks, map[string]any{"limit": 1})
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	if n := calls.Load(); n != 1 {
		t.Errorf("the database was opened %d times for 5 concurrent calls, want 1 shared attempt", n)
	}
	if elapsed > 3*attempt {
		t.Errorf("5 concurrent calls took %s, about %s each: they queued instead of sharing", elapsed, elapsed/5)
	}
	for i, msg := range errs {
		if !strings.Contains(msg, "not reachable") {
			t.Errorf("call %d error = %q, want the opener's reason", i, msg)
		}
	}
	// A later call still retries: a failure is never cached.
	_ = callError(t, s, ToolListTasks, map[string]any{"limit": 1})
	if n := calls.Load(); n != 2 {
		t.Errorf("after a later call the database was opened %d times, want 2", n)
	}
}

// A caller that gives up must not be held by an attempt someone else started.
func TestACancelledCallDoesNotWaitForTheAttempt(t *testing.T) {
	var calls atomic.Int32
	s := session(t, NewDeferred(failingOpener(5*time.Second, &calls), "test", logging.Discard()))

	go func() { // holds the attempt open
		_, _ = s.CallTool(context.Background(), &sdk.CallToolParams{
			Name: ToolListTasks, Arguments: map[string]any{"limit": 1},
		})
	}()
	time.Sleep(200 * time.Millisecond) // let the attempt start

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.CallTool(ctx, &sdk.CallToolParams{Name: ToolListTasks, Arguments: map[string]any{"limit": 1}})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a cancelled call returned no error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("a cancelled call returned after %s: it waited for the attempt in flight", elapsed)
	}
}
