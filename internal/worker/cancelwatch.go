package worker

import (
	"context"
	"log/slog"
	"time"

	"aidev/internal/logging"
	"aidev/internal/task"
)

// watchForCancel reads a run's task status every interval and calls stop once
// when it reads CANCELLED. A read error is logged to logger (nil discards) and
// the next poll tries again. It returns when ctx ends or after calling stop.
func watchForCancel(ctx context.Context, every time.Duration, read func(context.Context) (task.Status, error), stop func(), logger *slog.Logger) {
	if logger == nil {
		logger = logging.Discard()
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status, err := read(ctx)
			if err != nil {
				logger.WarnContext(ctx, "could not read the task status while watching for cancel", "error", err.Error())
				continue
			}
			if status == task.StatusCancelled {
				stop()
				return
			}
		}
	}
}
