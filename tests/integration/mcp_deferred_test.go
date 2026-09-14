package integration

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"aidev/internal/logging"
	aidevmcp "aidev/internal/mcp"
	"aidev/internal/store"
	"aidev/internal/worker"
)

// Once the database comes up, the session that is already connected must start
// working: Claude Code does not relaunch a server it is connected to, so recovery
// has to happen inside the server. And once connected, aidev must keep the
// connection rather than open a new one on every call.
func TestMCPServerRecoversWhenTheDatabaseComesUp(t *testing.T) {
	h := newHarness(t, nil)

	var opens atomic.Int32
	open := func(ctx context.Context) (*worker.Orchestrator, *store.Store, error) {
		if n := opens.Add(1); n <= 2 {
			return nil, nil, errors.New("database is not reachable: connection refused (simulated)\n\n" +
				"Is PostgreSQL running? `make db-up` starts it, and `aidev migrate` applies the schema")
		}
		return h.orchestrator, h.store, nil
	}
	server := aidevmcp.NewDeferred(open, "test", logging.Discard())

	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	serveCtx, cancelServe := context.WithCancel(h.ctx)
	served := make(chan error, 1)
	go func() { served <- server.ServeTransport(serveCtx, serverTransport) }()

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(h.ctx, clientTransport, nil)
	if err != nil {
		cancelServe()
		t.Fatalf("connect to the mcp server: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		cancelServe()
		select {
		case <-served:
		case <-time.After(30 * time.Second):
			t.Error("the mcp server did not shut down")
		}
	})

	if _, err := session.ListTools(h.ctx, nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if n := opens.Load(); n != 0 {
		t.Errorf("listing tools opened the database %d time(s): it must not need it", n)
	}

	listTasks := func(call int) *sdk.CallToolResult {
		t.Helper()
		res, err := session.CallTool(h.ctx, &sdk.CallToolParams{
			Name: "aidev_list_tasks", Arguments: map[string]any{"limit": 1},
		})
		if err != nil {
			t.Fatalf("call %d: transport error: %v", call, err)
		}
		return res
	}

	for call := 1; call <= 2; call++ {
		res := listTasks(call)
		if !res.IsError {
			t.Fatalf("call %d succeeded while the database was down", call)
		}
		if text := toolErrorText(res); !strings.Contains(text, "make db-up") {
			t.Errorf("call %d: error = %q, want the reason and the remedy passed through", call, text)
		}
	}
	for call := 3; call <= 4; call++ {
		if res := listTasks(call); res.IsError {
			t.Fatalf("call %d failed after the database came up: %s", call, toolErrorText(res))
		}
	}
	if n := opens.Load(); n != 3 {
		t.Errorf("the database was opened %d times over four calls, want 3: two failures, then one connection that is kept", n)
	}
}
