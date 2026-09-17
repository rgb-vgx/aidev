package worker

import (
	"context"
	"log/slog"
	"time"

	"aidev/internal/task"
)

// watchForCancel reads a run's task status every interval and calls stop once
// when it reads CANCELLED. A read error is logged to logger (nil discards) and
// the next poll tries again. It returns when ctx ends or after calling stop.
//
// Not implemented yet: specified by cancelwatch_test.go.
func watchForCancel(ctx context.Context, every time.Duration, read func(context.Context) (task.Status, error), stop func(), logger *slog.Logger) {
	<-ctx.Done()
}
