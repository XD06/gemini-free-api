package providers

import (
	"context"
	"fmt"
	"sync/atomic"
)

type retryBudgetKey struct{}
type RetryBudget struct{ remaining atomic.Int32 }

func ContextWithRetryBudget(ctx context.Context, attempts int) context.Context {
	if attempts <= 0 {
		attempts = 4
	}
	budget := &RetryBudget{}
	budget.remaining.Store(int32(attempts))
	return context.WithValue(ctx, retryBudgetKey{}, budget)
}
func ConsumeRetryBudget(ctx context.Context) error {
	budget, _ := ctx.Value(retryBudgetKey{}).(*RetryBudget)
	if budget == nil {
		return nil
	}
	for {
		current := budget.remaining.Load()
		if current <= 0 {
			return fmt.Errorf("upstream retry budget exhausted")
		}
		if budget.remaining.CompareAndSwap(current, current-1) {
			return nil
		}
	}
}
