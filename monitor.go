package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// legState describes the freshness state of one leg after a check.
type legState int

const (
	stateFresh legState = iota
	stateStale
	stateError
)

func (s legState) String() string {
	switch s {
	case stateFresh:
		return "fresh"
	case stateStale:
		return "stale"
	default:
		return "error"
	}
}

// legResult is the outcome of checking a single leg.
type legResult struct {
	leg   Leg
	state legState
	age   float64 // seconds since the newest object (0 when state == stateError)
	err   error
}

// monitor holds the state machine (last known state per leg) plus the most
// recent results served on /metrics.
type monitor struct {
	cfg    Config
	dryRun bool
	http   *http.Client
	notify func(string) error

	mu      sync.Mutex
	last    map[string]legState
	results []legResult
	lastRun float64
}

func newMonitor(cfg Config, dryRun bool) *monitor {
	m := &monitor{
		cfg:    cfg,
		dryRun: dryRun,
		http:   &http.Client{Timeout: 30 * time.Second},
		last:   make(map[string]legState),
	}
	m.notify = m.sendTelegram
	return m
}

// checkAll evaluates every leg at time now, emits any stale/error/recovery
// alerts, and returns the results plus the exit code for `check`
// (0 = fresh, 1 = stale, 2 = error).
func (m *monitor) checkAll(now time.Time) ([]legResult, int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	results := make([]legResult, 0, len(m.cfg.Legs))
	exit := 0
	for _, leg := range m.cfg.Legs {
		res := checkLeg(leg, now, m.http)
		results = append(results, res)
		m.transition(leg.Name, res)
		switch res.state {
		case stateStale:
			if exit == 0 {
				exit = 1
			}
		case stateError:
			exit = 2
		}
	}
	m.results = results
	m.lastRun = float64(now.Unix())
	return results, exit
}

// snapshot returns the latest results for the metrics endpoint.
func (m *monitor) snapshot() ([]legResult, float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.results, m.lastRun
}

// transition advances the per-leg state machine and alerts on meaningful
// changes: into stale, into error, and recovery back to fresh. Alerts fire
// once per transition, never repeatedly per run.
func (m *monitor) transition(name string, res legResult) {
	prev, seen := m.last[name]
	if seen && prev == res.state {
		return
	}
	var msg string
	switch {
	case res.state == stateStale:
		msg = fmt.Sprintf("b2-verify alert: leg %q is STALE - newest object age %.1fh exceeds the limit of %.1fh", res.leg.Name, res.age/3600, res.leg.MaxAgeHours)
	case res.state == stateError:
		msg = fmt.Sprintf("b2-verify alert: leg %q failed to check: %v", res.leg.Name, res.err)
	case res.state == stateFresh && seen && (prev == stateStale || prev == stateError):
		msg = fmt.Sprintf("b2-verify recovery: leg %q is fresh again - newest object age %.1fh", res.leg.Name, res.age/3600)
	}
	if msg == "" {
		m.last[name] = res.state
		return
	}

	if m.dryRun {
		slog.Info("alert suppressed (dry-run)", "message", msg)
		m.last[name] = res.state
		return
	}

	err := m.notify(msg)
	if err == nil {
		slog.Info("alert delivered", "message", msg)
		m.last[name] = res.state
		return
	}
	if errors.Is(err, errTelegramUnconfigured) {
		slog.Warn("alert not sent - TELEGRAM_BOT_TOKEN or TELEGRAM_CHAT_ID not set (log-only mode)", "message", msg)
		m.last[name] = res.state
		return
	}
	slog.Error("alert delivery failed - will retry on the next run", "leg", name, "error", err)
}

// checkLeg evaluates a single leg. A leg is fresh when its newest object is
// no older than max_age_hours.
func checkLeg(leg Leg, now time.Time, hc *http.Client) legResult {
	res := legResult{leg: leg}
	switch leg.Kind {
	case kindB2:
		age, err := checkB2Leg(leg, now, hc)
		if err != nil {
			res.state = stateError
			res.err = err
			return res
		}
		res.age = age
	case kindFile:
		info, err := os.Stat(leg.Path)
		if err != nil {
			res.state = stateError
			res.err = fmt.Errorf("cannot stat path %q: %w", leg.Path, err)
			return res
		}
		res.age = now.Sub(info.ModTime()).Seconds()
	default:
		res.state = stateError
		res.err = fmt.Errorf("unsupported leg kind %q", leg.Kind)
		return res
	}
	if res.age < 0 {
		res.age = 0
	}
	if res.age > leg.MaxAgeHours*3600 {
		res.state = stateStale
	} else {
		res.state = stateFresh
	}
	return res
}

// runCheck performs a single evaluation and returns the process exit code.
func runCheck(cfg Config, dryRun bool) int {
	m := newMonitor(cfg, dryRun)
	results, code := m.checkAll(time.Now())
	for _, r := range results {
		attrs := []any{"leg", r.leg.Name, "kind", r.leg.Kind, "state", r.state.String()}
		if r.state != stateError {
			attrs = append(attrs, "age_hours", round1(r.age/3600))
		}
		if r.err != nil {
			attrs = append(attrs, "error", r.err.Error())
		}
		slog.Info("leg check", attrs...)
	}
	switch code {
	case 0:
		slog.Info("check complete: all legs fresh")
	case 1:
		slog.Warn("check complete: at least one leg is stale")
	default:
		slog.Error("check complete: one or more legs failed to check")
	}
	return code
}

// runMonitor runs the continuous watchdog: an immediate check, then a check
// every interval, with /metrics served for the lifetime of the process.
func runMonitor(cfg Config, dryRun bool) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := newMonitor(cfg, dryRun)

	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", metricsHandler(m, cfg))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("serving metrics", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server stopped unexpectedly", "error", err)
			stop()
		}
	}()

	interval := time.Duration(cfg.IntervalSeconds) * time.Second
	slog.Info("b2-verify starting", "version", version, "legs", len(cfg.Legs), "interval", interval.String(), "dry_run", dryRun)

	check := func() {
		results, _ := m.checkAll(time.Now())
		for _, r := range results {
			slog.Info("leg checked", "leg", r.leg.Name, "kind", r.leg.Kind, "state", r.state.String(), "age_hours", round1(r.age/3600))
		}
	}
	check()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = srv.Shutdown(shCtx)
			cancel()
			return
		case <-ticker.C:
			check()
		}
	}
}

// metricsHandler serves the Prometheus exposition for the given monitor.
func metricsHandler(m *monitor, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		results, lastRun := m.snapshot()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, buildMetrics(results, lastRun, cfg))
	}
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
