package providers

import (
	"os"
	"strings"
	"sync"
)

// RuntimeSettings holds the mutable, process-wide switches that can be flipped
// from the web console without editing .env or restarting the service.
//
// Rationale: GEMINI_INCOGNITO lives in .env and is read once at startup, which
// means changing privacy mode requires editing the file and restarting. The
// console toggle is meant to be *the same switch* — one flip affects every
// subsequent request — so it writes here instead. The .env value stays the
// startup default; a console change overrides it until the next restart.
//
// Values are read on the request path from several goroutines while the admin
// endpoint writes them, so every access goes through the mutex (same lesson as
// SetLogger: shared state without a lock is a guaranteed data race).
type RuntimeSettings struct {
	mu sync.RWMutex
	// override is non-nil only after the console explicitly set a value. It is
	// deliberately separate from the cached env default so IncognitoSource can
	// tell the two apart.
	override *bool
	// envCached/envValue hold the lazily-read GEMINI_INCOGNITO default. The env
	// lookup cannot happen at init() time because .env is loaded by
	// godotenv.Overload() during configs.New, which runs after package init.
	envCached bool
	envValue  bool
}

var runtimeSettings = &RuntimeSettings{}

// RuntimeSettingsStore returns the process-wide settings instance.
func RuntimeSettingsStore() *RuntimeSettings {
	return runtimeSettings
}

// IncognitoEnabled reports the effective privacy-mode value: the console
// override when one was set, otherwise the GEMINI_INCOGNITO env default.
func (s *RuntimeSettings) IncognitoEnabled() bool {
	s.mu.RLock()
	override := s.override
	if override != nil {
		value := *override
		s.mu.RUnlock()
		return value
	}
	cached, cachedValue := s.envCached, s.envValue
	s.mu.RUnlock()

	if cached {
		return cachedValue
	}

	// Env is read outside the write lock: os.Getenv is cheap and this keeps the
	// critical section free of syscalls. Double-seeding is harmless because both
	// readers compute the same value from the same process environment.
	resolved := IncognitoFromEnv()

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.envCached {
		s.envCached = true
		s.envValue = resolved
	}
	return s.envValue
}

// SetIncognito applies a console override and returns the effective value.
func (s *RuntimeSettings) SetIncognito(enabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := enabled
	s.override = &value
	return value
}

// IncognitoSource reports where the current value comes from: "runtime" once
// the console has set it, "env" while it still tracks GEMINI_INCOGNITO.
func (s *RuntimeSettings) IncognitoSource() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.override == nil {
		return "env"
	}
	return "runtime"
}

// IncognitoEnabled is the package-level convenience reader used on the request
// path by the protocol adapters.
func IncognitoEnabled() bool {
	return runtimeSettings.IncognitoEnabled()
}

// SetIncognito flips the process-wide privacy-mode switch.
func SetIncognito(enabled bool) bool {
	return runtimeSettings.SetIncognito(enabled)
}

// IncognitoSource reports whether the active value came from the console or
// from the .env default.
func IncognitoSource() string {
	return runtimeSettings.IncognitoSource()
}

// IncognitoFromEnv reports whether the GEMINI_INCOGNITO env switch is on. It is
// the single reader of that key, so every protocol adapter agrees on the
// startup default. Values are the usual truthy set; anything else is off.
func IncognitoFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GEMINI_INCOGNITO"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
