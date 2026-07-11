package providers

import (
	"context"
	"testing"
)

func TestRetryBudgetExhausts(t *testing.T) {
	ctx := ContextWithRetryBudget(context.Background(), 2)
	if err := ConsumeRetryBudget(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ConsumeRetryBudget(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ConsumeRetryBudget(ctx); err == nil {
		t.Fatal("expected exhausted budget")
	}
}
