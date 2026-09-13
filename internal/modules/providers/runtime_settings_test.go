package providers

import (
	"sync"
	"testing"
)

// TestRuntimeSettingsDefaultsToEnv pins the precedence rule: until the console
// sets an override, the effective value tracks GEMINI_INCOGNITO.
func TestRuntimeSettingsDefaultsToEnv(t *testing.T) {
	t.Setenv("GEMINI_INCOGNITO", "true")

	settings := &RuntimeSettings{}
	if !settings.IncognitoEnabled() {
		t.Fatal("expected env default GEMINI_INCOGNITO=true to be honored")
	}
	if got := settings.IncognitoSource(); got != "env" {
		t.Fatalf("source = %q, want env", got)
	}
}

func TestRuntimeSettingsEnvDefaultOff(t *testing.T) {
	t.Setenv("GEMINI_INCOGNITO", "")

	settings := &RuntimeSettings{}
	if settings.IncognitoEnabled() {
		t.Fatal("expected privacy mode off when GEMINI_INCOGNITO is unset")
	}
}

// TestRuntimeSettingsOverrideBeatsEnv is the console-toggle contract: flipping
// the switch changes the effective value for every later read, in both
// directions, and reports "runtime" as the source.
func TestRuntimeSettingsOverrideBeatsEnv(t *testing.T) {
	t.Setenv("GEMINI_INCOGNITO", "false")

	settings := &RuntimeSettings{}

	if got := settings.SetIncognito(true); !got {
		t.Fatal("SetIncognito(true) should return true")
	}
	if !settings.IncognitoEnabled() {
		t.Fatal("override=true must beat env=false")
	}
	if got := settings.IncognitoSource(); got != "runtime" {
		t.Fatalf("source = %q, want runtime", got)
	}

	if got := settings.SetIncognito(false); got {
		t.Fatal("SetIncognito(false) should return false")
	}
	if settings.IncognitoEnabled() {
		t.Fatal("override=false must beat env=false")
	}
}

// TestRuntimeSettingsOverrideCanEnableWhenEnvOff covers the reverse direction,
// where the console is the only thing turning privacy mode on.
func TestRuntimeSettingsOverrideCanEnableWhenEnvOff(t *testing.T) {
	t.Setenv("GEMINI_INCOGNITO", "")

	settings := &RuntimeSettings{}
	settings.SetIncognito(true)
	if !settings.IncognitoEnabled() {
		t.Fatal("console override should enable privacy mode with env unset")
	}
}

// TestRuntimeSettingsConcurrentAccess exercises the mutex: readers on the
// request path race writers from the admin endpoint. Run with -race to make
// this meaningful.
func TestRuntimeSettingsConcurrentAccess(t *testing.T) {
	t.Setenv("GEMINI_INCOGNITO", "true")

	settings := &RuntimeSettings{}
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(flip bool) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				settings.SetIncognito(flip)
			}
		}(i%2 == 0)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = settings.IncognitoEnabled()
				_ = settings.IncognitoSource()
			}
		}()
	}
	wg.Wait()
}

func TestIncognitoFromEnvTruthyValues(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes", "on", " On "} {
		t.Setenv("GEMINI_INCOGNITO", value)
		if !IncognitoFromEnv() {
			t.Fatalf("GEMINI_INCOGNITO=%q should be truthy", value)
		}
	}
	for _, value := range []string{"", "0", "false", "no", "off", "nope"} {
		t.Setenv("GEMINI_INCOGNITO", value)
		if IncognitoFromEnv() {
			t.Fatalf("GEMINI_INCOGNITO=%q should be falsy", value)
		}
	}
}
