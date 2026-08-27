package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain silences the default slog output so test runs stay readable;
// alert delivery is asserted via injected notifiers instead.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// newFileLeg creates a temporary directory with a marker file whose mtime is
// offset from now by the given duration, and returns the leg pointing at it.
func newFileLeg(t *testing.T, name string, maxAgeHours float64, mtimeOffset time.Duration) (Leg, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "marker")
	if err := os.WriteFile(path, []byte("backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-mtimeOffset)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
	return Leg{Name: name, Kind: kindFile, Path: path, MaxAgeHours: maxAgeHours}, path
}

// newTestMonitor builds a monitor whose notifier is a no-op, so transition
// tests can observe alert traffic via their own injected function.
func newTestMonitor(t *testing.T, legs []Leg) *monitor {
	t.Helper()
	m := newMonitor(Config{Legs: legs, Listen: defaultListen, IntervalSeconds: defaultIntervalSeconds}, false)
	m.notify = func(string) error { return nil }
	return m
}

func TestFileLegBoundary(t *testing.T) {
	// Boundary semantics are deterministic when the comparison instant is
	// derived from the file's real mtime (filesystems round timestamps).
	atLimit, path := newFileLeg(t, "at-limit", 1, 25*time.Hour)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mt := info.ModTime()

	// Age exactly equal to max_age_hours: fresh.
	res := checkLeg(atLimit, mt.Add(time.Hour), nil)
	if res.state != stateFresh {
		t.Fatalf("leg at exactly the age limit should be fresh, got %v", res.state)
	}
	if res.age != 3600 {
		t.Fatalf("age should be exactly 3600s, got %v", res.age)
	}

	// One second over the limit: stale.
	res = checkLeg(atLimit, mt.Add(time.Hour+time.Second), nil)
	if res.state != stateStale {
		t.Fatalf("leg one second over the limit should be stale, got %v", res.state)
	}
	if res.age <= 3600 {
		t.Fatalf("age should exceed 3600s, got %v", res.age)
	}

	// Just under the limit: fresh.
	res = checkLeg(atLimit, mt.Add(time.Hour-time.Second), nil)
	if res.state != stateFresh {
		t.Fatalf("leg just under the limit should be fresh, got %v", res.state)
	}

	// Sanity: a genuinely recent file is fresh under a 24h limit.
	recent, _ := newFileLeg(t, "recent", 24, 5*time.Minute)
	res = checkLeg(recent, time.Now(), nil)
	if res.state != stateFresh {
		t.Fatalf("recent leg should be fresh, got %v", res.state)
	}
}

func TestFileLegMissingPath(t *testing.T) {
	leg := Leg{Name: "missing", Kind: kindFile, Path: filepath.Join(t.TempDir(), "nope"), MaxAgeHours: 1}
	res := checkLeg(leg, time.Now(), nil)
	if res.state != stateError {
		t.Fatalf("missing path should error, got %v", res.state)
	}
	if res.err == nil {
		t.Fatal("expected an error detail for the missing path")
	}
}

func TestFileLegFutureMtimeClampedToZero(t *testing.T) {
	leg, _ := newFileLeg(t, "future", 24, -time.Hour) // mtime one hour in the future
	res := checkLeg(leg, time.Now(), nil)
	if res.age != 0 {
		t.Fatalf("future mtime should clamp age to 0, got %v", res.age)
	}
	if res.state != stateFresh {
		t.Fatalf("clamped age should be fresh, got %v", res.state)
	}
}

func TestCheckExitCodes(t *testing.T) {
	fresh, _ := newFileLeg(t, "fresh", 24, time.Minute)
	stale, _ := newFileLeg(t, "stale", 1, 2*time.Hour)
	missing := Leg{Name: "missing", Kind: kindFile, Path: filepath.Join(t.TempDir(), "nope"), MaxAgeHours: 1}

	m := newTestMonitor(t, []Leg{fresh})
	if _, code := m.checkAll(time.Now()); code != 0 {
		t.Fatalf("all fresh should exit 0, got %d", code)
	}

	m = newTestMonitor(t, []Leg{fresh, stale})
	if _, code := m.checkAll(time.Now()); code != 1 {
		t.Fatalf("stale leg should exit 1, got %d", code)
	}

	m = newTestMonitor(t, []Leg{stale, missing})
	if _, code := m.checkAll(time.Now()); code != 2 {
		t.Fatalf("error leg should take precedence with exit 2, got %d", code)
	}
}

func TestStaleAlertOnceThenRecovery(t *testing.T) {
	var sent []string
	leg, path := newFileLeg(t, "stale-leg", 1, 2*time.Hour)
	m := newTestMonitor(t, []Leg{leg})
	m.notify = func(text string) error { sent = append(sent, text); return nil }

	// First run: already stale on first sight -> alert once.
	if _, code := m.checkAll(time.Now()); code != 1 {
		t.Fatalf("expected exit 1 for stale leg, got %d", code)
	}
	if len(sent) != 1 {
		t.Fatalf("expected 1 alert on first sight, got %d: %v", len(sent), sent)
	}
	if !strings.Contains(sent[0], "STALE") {
		t.Fatalf("alert should mention staleness: %q", sent[0])
	}

	// Second run: still stale -> no re-alert (no spam).
	if _, code := m.checkAll(time.Now()); code != 1 {
		t.Fatalf("expected exit 1 again, got %d", code)
	}
	if len(sent) != 1 {
		t.Fatalf("no re-alert while still stale, got %d alerts", len(sent))
	}

	// Recovery: mtime now fresh -> recovery alert.
	mt := time.Now()
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
	if _, code := m.checkAll(time.Now()); code != 0 {
		t.Fatalf("expected exit 0 after recovery, got %d", code)
	}
	if len(sent) != 2 {
		t.Fatalf("expected a recovery alert, got %d: %v", len(sent), sent)
	}
	if !strings.Contains(sent[1], "recovery") {
		t.Fatalf("second alert should be a recovery message: %q", sent[1])
	}
}

func TestErrorThenRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "marker")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	leg := Leg{Name: "err-leg", Kind: kindFile, Path: path, MaxAgeHours: 1}
	m := newTestMonitor(t, []Leg{leg})
	var sent []string
	m.notify = func(text string) error { sent = append(sent, text); return nil }

	// Missing path -> error alert.
	m.cfg.Legs[0].Path = filepath.Join(dir, "gone")
	if _, code := m.checkAll(time.Now()); code != 2 {
		t.Fatalf("expected exit 2 for error leg, got %d", code)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "failed to check") {
		t.Fatalf("expected an error alert, got %d: %v", len(sent), sent)
	}

	// Path restored and fresh -> recovery alert.
	m.cfg.Legs[0].Path = path
	if _, code := m.checkAll(time.Now()); code != 0 {
		t.Fatalf("expected exit 0 after recovery, got %d", code)
	}
	if len(sent) != 2 || !strings.Contains(sent[1], "fresh again") {
		t.Fatalf("expected a recovery alert, got %d: %v", len(sent), sent)
	}
}

func TestDryRunSuppressesAlerts(t *testing.T) {
	leg, _ := newFileLeg(t, "dry", 1, 2*time.Hour)
	m := newMonitor(Config{Legs: []Leg{leg}, Listen: defaultListen, IntervalSeconds: defaultIntervalSeconds}, true)
	called := false
	m.notify = func(string) error { called = true; return nil }

	if _, code := m.checkAll(time.Now()); code != 1 {
		t.Fatalf("dry-run should still report exit 1 for stale, got %d", code)
	}
	if called {
		t.Fatal("dry-run must not call the notifier")
	}
}

func TestUnconfiguredTelegramIsLogOnly(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_CHAT_ID", "")
	leg, _ := newFileLeg(t, "quiet", 1, 2*time.Hour)
	m := newMonitor(Config{Legs: []Leg{leg}, Listen: defaultListen, IntervalSeconds: defaultIntervalSeconds}, false)
	// Default notifier is sendTelegram, which returns errTelegramUnconfigured.

	if _, code := m.checkAll(time.Now()); code != 1 {
		t.Fatalf("expected exit 1 for stale leg, got %d", code)
	}
	// The state must still be recorded so the watchdog does not re-log the
	// same alert on every run.
	if got := m.last["quiet"]; got != stateStale {
		t.Fatalf("state should be recorded after a log-only alert, got %v", got)
	}
	// A second run must not attempt delivery again (observed via last map:
	// no transitions pending).
	if _, code := m.checkAll(time.Now()); code != 1 {
		t.Fatalf("expected exit 1 on second run, got %d", code)
	}
}

func TestFailedDeliveryRetriesNextRun(t *testing.T) {
	leg, _ := newFileLeg(t, "retry-leg", 1, 2*time.Hour)
	m := newTestMonitor(t, []Leg{leg})
	attempts := 0
	m.notify = func(string) error {
		attempts++
		return os.ErrPermission // delivery failure
	}
	if _, code := m.checkAll(time.Now()); code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if attempts != 1 {
		t.Fatalf("expected 1 delivery attempt, got %d", attempts)
	}
	// State must NOT be recorded: next run retries.
	if _, code := m.checkAll(time.Now()); code != 1 {
		t.Fatalf("expected exit 1 on second run, got %d", code)
	}
	if attempts != 2 {
		t.Fatalf("failed delivery should retry on the next run, got %d attempts", attempts)
	}
}
