package providers

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	AccountErrorNotFound            = "account_not_found"
	AccountErrorNotInitialized      = "account_not_initialized"
	AccountErrorCookiePairMismatch  = "cookie_pair_mismatch"
	AccountErrorCookieExpired       = "cookie_expired"
	AccountErrorRefreshInProgress   = "refresh_in_progress"
	AccountErrorProxyUnreachable    = "proxy_unreachable"
	AccountErrorUpstreamChallenge   = "upstream_challenge"
	AccountErrorUpstreamTimeout     = "upstream_timeout"
	AccountErrorGenerationProbeFail = "generation_probe_failed"
)

// AccountOperationError keeps admin-facing account failures stable and
// actionable without discarding the underlying diagnostic error.
type AccountOperationError struct {
	Code      string
	State     string
	Message   string
	Retryable bool
	Action    string
	Err       error
}

func (e *AccountOperationError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Code
}

func (e *AccountOperationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (c *Client) sessionInitialized() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	initialized := strings.TrimSpace(c.at) != ""
	c.mu.RUnlock()
	return initialized
}

func accountNotFoundError(accountID string) *AccountOperationError {
	return &AccountOperationError{
		Code:    AccountErrorNotFound,
		Message: fmt.Sprintf("Gemini account %q was not found", accountID),
		Action:  "Refresh the account list and select an existing account.",
	}
}

func refreshInProgressError(state string) *AccountOperationError {
	return &AccountOperationError{
		Code:      AccountErrorRefreshInProgress,
		State:     state,
		Message:   "The Gemini account is currently refreshing.",
		Retryable: true,
		Action:    "Wait for the current refresh to finish, then test again.",
	}
}

func accountOperationError(err error, state, fallbackCode string) *AccountOperationError {
	var existing *AccountOperationError
	if errors.As(err, &existing) {
		return existing
	}
	if err == nil {
		err = errors.New("Gemini account operation failed")
	}
	message := strings.ToLower(err.Error())
	result := &AccountOperationError{Code: fallbackCode, State: state, Err: err}

	switch {
	case strings.Contains(message, "cookie pair rejected"),
		strings.Contains(message, "rejected the cookie pair"),
		strings.Contains(message, "must be paired"),
		strings.Contains(message, "must match"):
		result.Code = AccountErrorCookiePairMismatch
		result.State = AccountStateNeedsManualLogin
		result.Message = "Gemini rejected the cookie pair: __Secure-1PSID and __Secure-1PSIDTS must come from the same logged-in browser session."
		result.Action = "Update both cookies together from the same authenticated Gemini browser session."
	case strings.Contains(message, "cookies invalid"),
		strings.Contains(message, "status 401"),
		strings.Contains(message, "status 403"),
		strings.Contains(message, "sign in"):
		result.Code = AccountErrorCookieExpired
		result.State = AccountStateNeedsManualLogin
		result.Message = "The Gemini login cookies are expired or no longer accepted."
		result.Action = "Sign in to Gemini again and update both account cookies."
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(message, "timeout"), strings.Contains(message, "deadline exceeded"):
		result.Code = AccountErrorUpstreamTimeout
		result.Retryable = true
		result.Message = "The Gemini account operation timed out while contacting Google."
		result.Action = "Check the account proxy and network, then retry."
	case strings.Contains(message, "proxy"), strings.Contains(message, "dial tcp"), strings.Contains(message, "tls handshake"):
		result.Code = AccountErrorProxyUnreachable
		result.Retryable = true
		result.Message = "The configured account proxy could not reach Gemini."
		result.Action = "Check or replace the account proxy, then retry."
	case strings.Contains(message, "challenge"), strings.Contains(message, "/sorry"):
		result.Code = AccountErrorUpstreamChallenge
		result.Retryable = true
		result.Message = "Google returned a login or anti-abuse challenge for this account session."
		result.Action = "Resolve the challenge in the browser or use a valid session and proxy."
	case strings.Contains(message, "client not initialized"), strings.Contains(message, "snlm0e not found"):
		result.Code = AccountErrorNotInitialized
		result.Message = "The Gemini account session is not initialized."
		result.Action = "Refresh the account session; if that fails, update the matching cookie pair."
	default:
		if result.Code == "" {
			result.Code = AccountErrorGenerationProbeFail
		}
		result.Message = err.Error()
		if result.Code == AccountErrorGenerationProbeFail {
			result.Retryable = true
			result.Action = "Check the nested upstream error, account proxy, and cookie status."
		}
	}
	return result
}

func accountStatusError(client *Client, fallbackCode string) *AccountOperationError {
	if client == nil {
		return accountOperationError(errors.New("Gemini account is unavailable"), AccountStateUninitialized, fallbackCode)
	}
	status := client.AccountStatus()
	state := status.State
	if state == "" {
		state = AccountStateUninitialized
	}
	if strings.TrimSpace(status.LastError) == "" {
		return &AccountOperationError{
			Code:    fallbackCode,
			State:   state,
			Message: "The Gemini account session is not initialized.",
			Action:  "Refresh the account session; if that fails, update the matching cookie pair.",
		}
	}
	return accountOperationError(errors.New(status.LastError), state, fallbackCode)
}

func accountFailureState(err *AccountOperationError) string {
	if err != nil && (err.Code == AccountErrorCookiePairMismatch || err.Code == AccountErrorCookieExpired) {
		return AccountStateNeedsManualLogin
	}
	return AccountStateExpired
}
