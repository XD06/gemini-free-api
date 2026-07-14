package providers

import (
	"context"
	"sync"
	"time"
)

type streamTimingsContextKey struct{}

// StreamTimingSnapshot captures client-visible milestones for one streaming request.
type StreamTimingSnapshot struct {
	UpstreamTTFBMs   int64
	FirstReasoningMs int64
	FirstContentMs   int64
	TailCloseMs      int64
	CompletionSource string
	RetryCount       int
}

type StreamTimings struct {
	mu               sync.Mutex
	startedAt        time.Time
	upstreamTTFBMs   int64
	firstReasoningMs int64
	firstContentMs   int64
	tailCloseMs      int64
	completionSource string
	retryCount       int
}

func ContextWithStreamTimings(ctx context.Context) (context.Context, *StreamTimings) {
	timings := &StreamTimings{
		startedAt:        time.Now(),
		upstreamTTFBMs:   -1,
		firstReasoningMs: -1,
		firstContentMs:   -1,
		tailCloseMs:      -1,
	}
	return context.WithValue(ctx, streamTimingsContextKey{}, timings), timings
}

func streamTimingsFromContext(ctx context.Context) *StreamTimings {
	if ctx == nil {
		return nil
	}
	timings, _ := ctx.Value(streamTimingsContextKey{}).(*StreamTimings)
	return timings
}

func (t *StreamTimings) markFirst(target *int64) {
	if t == nil || *target >= 0 {
		return
	}
	*target = time.Since(t.startedAt).Milliseconds()
}

func markStreamUpstreamBytes(ctx context.Context) {
	if timings := streamTimingsFromContext(ctx); timings != nil {
		timings.mu.Lock()
		timings.markFirst(&timings.upstreamTTFBMs)
		timings.mu.Unlock()
	}
}

func markStreamReasoning(ctx context.Context) {
	if timings := streamTimingsFromContext(ctx); timings != nil {
		timings.mu.Lock()
		timings.markFirst(&timings.firstReasoningMs)
		timings.mu.Unlock()
	}
}

func markStreamContent(ctx context.Context) {
	if timings := streamTimingsFromContext(ctx); timings != nil {
		timings.mu.Lock()
		timings.markFirst(&timings.firstContentMs)
		timings.mu.Unlock()
	}
}

func markStreamCompletion(ctx context.Context, source string, lastContentAt time.Time) {
	if timings := streamTimingsFromContext(ctx); timings != nil {
		timings.mu.Lock()
		if timings.completionSource == "" {
			timings.completionSource = source
			if !lastContentAt.IsZero() {
				timings.tailCloseMs = time.Since(lastContentAt).Milliseconds()
			}
		}
		timings.mu.Unlock()
	}
}

func markStreamRetry(ctx context.Context) {
	if timings := streamTimingsFromContext(ctx); timings != nil {
		timings.mu.Lock()
		timings.retryCount++
		timings.mu.Unlock()
	}
}

func (t *StreamTimings) Snapshot() StreamTimingSnapshot {
	if t == nil {
		return StreamTimingSnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return StreamTimingSnapshot{
		UpstreamTTFBMs:   t.upstreamTTFBMs,
		FirstReasoningMs: t.firstReasoningMs,
		FirstContentMs:   t.firstContentMs,
		TailCloseMs:      t.tailCloseMs,
		CompletionSource: t.completionSource,
		RetryCount:       t.retryCount,
	}
}
