package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuildMetricsFamilies(t *testing.T) {
	cfg := Config{Listen: defaultListen, IntervalSeconds: defaultIntervalSeconds, Legs: []Leg{
		{Name: "leg-a", Kind: kindFile, MaxAgeHours: 1},
		{Name: "leg-b", Kind: kindFile, MaxAgeHours: 1},
	}}
	results := []legResult{
		{leg: cfg.Legs[0], state: stateFresh, age: 3600},
		{leg: cfg.Legs[1], state: stateStale, age: 7200},
	}
	out := buildMetrics(results, 1234567890, cfg)

	for _, family := range []struct{ name, help, typ string }{
		{"b2_verify_leg_age_seconds", "Age in seconds of the newest object on each monitored leg.", "gauge"},
		{"b2_verify_leg_fresh", "Whether the leg is fresh: 1 = fresh, 0 = stale or error.", "gauge"},
		{"b2_verify_last_run_timestamp_seconds", "Unix time in seconds of the last completed check run.", "gauge"},
		{"b2_verify_scrape", "Information about this scrape: version and number of configured legs.", "gauge"},
	} {
		if !strings.Contains(out, "# HELP "+family.name+" "+family.help+"\n") {
			t.Errorf("missing # HELP for %s", family.name)
		}
		if !strings.Contains(out, "# TYPE "+family.name+" "+family.typ+"\n") {
			t.Errorf("missing # TYPE for %s", family.name)
		}
	}

	for _, want := range []string{
		`b2_verify_leg_age_seconds{leg="leg-a"} 3600.000`,
		`b2_verify_leg_age_seconds{leg="leg-b"} 7200.000`,
		`b2_verify_leg_fresh{leg="leg-a"} 1`,
		`b2_verify_leg_fresh{leg="leg-b"} 0`,
		"b2_verify_last_run_timestamp_seconds 1234567890",
		`b2_verify_scrape{legs="2",version="v0.1.0-dev"} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing expected line %q", want)
		}
	}
}

func TestBuildMetricsEscapesLabelValues(t *testing.T) {
	cfg := Config{Listen: defaultListen, IntervalSeconds: defaultIntervalSeconds}
	weirdName := "we\"ird\\na\nme"
	results := []legResult{{leg: Leg{Name: weirdName}, state: stateFresh, age: 1}}
	out := buildMetrics(results, 0, cfg)

	wantFresh := "b2_verify_leg_fresh{leg=\"we\\\"ird\\\\na\\nme\"} 1"
	wantAge := "b2_verify_leg_age_seconds{leg=\"we\\\"ird\\\\na\\nme\"} 1.000"
	if !strings.Contains(out, wantFresh) {
		t.Errorf("fresh series should escape label value, got:\n%s", out)
	}
	if !strings.Contains(out, wantAge) {
		t.Errorf("age series should escape label value, got:\n%s", out)
	}
}

func TestMetricsHandlerServesExposition(t *testing.T) {
	leg, _ := newFileLeg(t, "http-leg", 24, time.Hour)
	m := newTestMonitor(t, []Leg{leg})
	if _, code := m.checkAll(time.Now()); code != 0 {
		t.Fatalf("expected fresh legs, got exit %d", code)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsHandler(m, m.cfg)(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("unexpected content type %q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "b2_verify_leg_fresh{leg=\"http-leg\"} 1") {
		t.Fatalf("metrics body missing leg series:\n%s", body)
	}
	if !strings.Contains(body, "b2_verify_last_run_timestamp_seconds") {
		t.Fatalf("metrics body missing last run series:\n%s", body)
	}
}

func TestSendTelegramPostsMessage(t *testing.T) {
	received := make(chan map[string]string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		received <- body
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	oldURL := telegramSendURL
	telegramSendURL = srv.URL + "/bot%s/sendMessage"
	defer func() { telegramSendURL = oldURL }()

	t.Setenv("TELEGRAM_BOT_TOKEN", "test-bot-value")
	t.Setenv("TELEGRAM_CHAT_ID", "test-chat")

	m := newMonitor(Config{Legs: nil, Listen: defaultListen, IntervalSeconds: defaultIntervalSeconds}, false)
	m.http = srv.Client()
	if err := m.sendTelegram("hello from the watchdog"); err != nil {
		t.Fatalf("sendTelegram: %v", err)
	}
	body := <-received
	if body["chat_id"] != "test-chat" {
		t.Fatalf("unexpected chat_id %q", body["chat_id"])
	}
	if body["text"] != "hello from the watchdog" {
		t.Fatalf("unexpected text %q", body["text"])
	}
}

func TestSendTelegramUnconfigured(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("TELEGRAM_CHAT_ID", "chat")
	m := newMonitor(Config{}, false)
	if err := m.sendTelegram("x"); err != errTelegramUnconfigured {
		t.Fatalf("expected errTelegramUnconfigured, got %v", err)
	}
}
