package storage

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

// ProgressRows is how many rows a corpus load writes between progress lines.
const ProgressRows = 50_000

// ProgressInterval is how often a stage with no rows to count reports that it is still running.
const ProgressInterval = 10 * time.Second

// WatchStage logs every ProgressInterval until stop is called; progress, when given, adds what the engine reports.
func WatchStage(ctx context.Context, attrs []any, progress func(context.Context) []any) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(ProgressInterval)
		defer ticker.Stop()
		start := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				line := append(slices.Clone(attrs), "elapsed", time.Since(start).Round(time.Second))
				if progress != nil {
					line = append(line, progress(ctx)...)
				}
				slog.Info("corpus load stage running", line...)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
