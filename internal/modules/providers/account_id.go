package providers

import (
	"context"
	"sync"
)

// AccountIDCtxKey is the context key for the account ID holder.
type AccountIDCtxKey struct{}

// AccountIDHolder is a thread-safe container for propagating the
// selected account ID from the provider layer back to the caller
// (e.g. the OpenAI controller) via context.
type AccountIDHolder struct {
	mu sync.RWMutex
	id string
}

// NewAccountIDHolder creates a new empty holder.
func NewAccountIDHolder() *AccountIDHolder {
	return &AccountIDHolder{}
}

// Set updates the stored account ID.
func (h *AccountIDHolder) Set(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.id = id
}

// Get returns the stored account ID.
func (h *AccountIDHolder) Get() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.id
}

// ContextWithAccountID returns a new context that carries an AccountIDHolder.
func ContextWithAccountID(ctx context.Context) (context.Context, *AccountIDHolder) {
	holder := NewAccountIDHolder()
	return context.WithValue(ctx, AccountIDCtxKey{}, holder), holder
}

// AccountIDFromContext returns the holder stored in ctx, or nil.
func AccountIDFromContext(ctx context.Context) *AccountIDHolder {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(AccountIDCtxKey{}).(*AccountIDHolder)
	return h
}
