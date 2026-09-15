package cli

import (
	"context"
	"errors"
	"sync"
)

// lazyApp owns an app that is opened on first use. It exists because three rules
// about that lifecycle are easy to get wrong in an ad-hoc closure, and one of them
// was: the MCP server read the opened app once after Serve returned, so an attempt
// that succeeded later was never closed — the pool stayed open and the traces were
// never flushed (docs/research.md 7g).
//
//   - callers that arrive together share one attempt, rather than queueing behind a
//     mutex and paying for an attempt each;
//   - a failure is not remembered, so the next call tries again, which is what a
//     database that is still starting needs;
//   - close waits for an attempt in flight and closes what it returns, however many
//     times it is called.
type lazyApp struct {
	connect func(context.Context) (*app, error)

	mu      sync.Mutex
	opened  *app
	attempt *appAttempt
	closed  bool
}

// appAttempt is one connection attempt, shared by everyone waiting on it.
type appAttempt struct {
	done chan struct{}
	app  *app
	err  error
}

func newLazyApp(connect func(context.Context) (*app, error)) *lazyApp {
	return &lazyApp{connect: connect}
}

// errAppClosed is returned to a caller that arrives after close.
var errAppClosed = errors.New("aidev: the application is shutting down")

// get returns the app, opening it if necessary. The caller's context bounds only
// the wait: an attempt already in flight belongs to every waiter, so one caller
// giving up does not cancel the connection the others are waiting for.
func (l *lazyApp) get(ctx context.Context) (*app, error) {
	l.mu.Lock()
	switch {
	case l.closed:
		l.mu.Unlock()
		return nil, errAppClosed
	case l.opened != nil:
		opened := l.opened
		l.mu.Unlock()
		return opened, nil
	}
	attempt := l.attempt
	if attempt == nil {
		attempt = &appAttempt{done: make(chan struct{})}
		l.attempt = attempt
		go l.open(attempt)
	}
	l.mu.Unlock()

	select {
	case <-attempt.done:
		return attempt.app, attempt.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// open runs one attempt. Its context is not any caller's: the attempt is shared, and
// the connect function applies its own bound.
func (l *lazyApp) open(attempt *appAttempt) {
	a, err := l.connect(context.Background())

	l.mu.Lock()
	attempt.app, attempt.err = a, err
	if err == nil {
		l.opened = a
	}
	// Nothing is remembered about a failure: the next get starts a new attempt.
	l.attempt = nil
	l.mu.Unlock()

	close(attempt.done)
}

// close closes the app once. It waits for an attempt in flight so that an app which
// arrives during shutdown is closed rather than leaked.
func (l *lazyApp) close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	attempt := l.attempt
	opened := l.opened
	l.mu.Unlock()

	if attempt != nil {
		<-attempt.done
		l.mu.Lock()
		if opened == nil {
			opened = l.opened
		}
		l.mu.Unlock()
	}
	if opened != nil && opened.close != nil {
		opened.close()
	}
}
