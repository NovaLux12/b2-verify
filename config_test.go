package main

import (
	"strings"
	"testing"
)

func TestParseConfigBareArray(t *testing.T) {
	cfg, err := parseConfig([]byte(`[
		{"name":"b2-leg","kind":"b2","bucket":"YOUR_BUCKET","prefix":"backup/","key_id_env":"B2_KEY_ID","key_env":"B2_APPLICATION_KEY","max_age_hours":30},
		{"name":"file-leg","kind":"file","path":"/var/backups/markers","max_age_hours":2}
	]`))
	if err != nil {
		t.Fatalf("parse bare array: %v", err)
	}
	if len(cfg.Legs) != 2 {
		t.Fatalf("expected 2 legs, got %d", len(cfg.Legs))
	}
	if cfg.Legs[0].Kind != kindB2 || cfg.Legs[0].Bucket != "YOUR_BUCKET" {
		t.Fatalf("b2 leg not parsed correctly: %+v", cfg.Legs[0])
	}
	if cfg.Legs[1].Kind != kindFile || cfg.Legs[1].Path != "/var/backups/markers" {
		t.Fatalf("file leg not parsed correctly: %+v", cfg.Legs[1])
	}
	// Defaults applied.
	if cfg.Listen != defaultListen {
		t.Fatalf("default listen should be %q, got %q", defaultListen, cfg.Listen)
	}
	if cfg.IntervalSeconds != defaultIntervalSeconds {
		t.Fatalf("default interval should be %d, got %d", defaultIntervalSeconds, cfg.IntervalSeconds)
	}
}

func TestParseConfigObjectWithOverrides(t *testing.T) {
	cfg, err := parseConfig([]byte(`{
		"listen": ":9999",
		"interval_seconds": 60,
		"legs": [{"name":"a","kind":"file","path":"/tmp/x","max_age_hours":1}]
	}`))
	if err != nil {
		t.Fatalf("parse object config: %v", err)
	}
	if cfg.Listen != ":9999" {
		t.Fatalf("listen override not honoured: %q", cfg.Listen)
	}
	if cfg.IntervalSeconds != 60 {
		t.Fatalf("interval override not honoured: %d", cfg.IntervalSeconds)
	}
	if len(cfg.Legs) != 1 {
		t.Fatalf("expected 1 leg, got %d", len(cfg.Legs))
	}
}

func TestParseConfigErrors(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantSub string
	}{
		{"empty legs", `{"legs":[]}`, "at least one leg"},
		{"bad json", `{`, "parse config"},
		{"bare array bad json", `[`, "parse legs array"},
		{"unknown kind", `[{"name":"a","kind":"s3","max_age_hours":1}]`, "kind must be"},
		{"b2 missing bucket", `[{"name":"a","kind":"b2","key_id_env":"K","key_env":"E","max_age_hours":1}]`, "requires bucket"},
		{"b2 missing key env names", `[{"name":"a","kind":"b2","bucket":"b","max_age_hours":1}]`, "key_id_env"},
		{"file missing path", `[{"name":"a","kind":"file","max_age_hours":1}]`, "requires path"},
		{"zero max age", `[{"name":"a","kind":"file","path":"/tmp","max_age_hours":0}]`, "positive"},
		{"negative max age", `[{"name":"a","kind":"file","path":"/tmp","max_age_hours":-1}]`, "positive"},
		{"empty name", `[{"name":"","kind":"file","path":"/tmp","max_age_hours":1}]`, "name must not be empty"},
		{"duplicate names", `[{"name":"a","kind":"file","path":"/x","max_age_hours":1},{"name":"a","kind":"file","path":"/y","max_age_hours":1}]`, "duplicate leg name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfig([]byte(tc.input))
			if err == nil {
				t.Fatalf("expected an error for %q, got none", tc.input)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestParseConfigRejectsInvalidJSONTypes(t *testing.T) {
	// max_age_hours as a string must fail rather than silently default.
	_, err := parseConfig([]byte(`[{"name":"a","kind":"file","path":"/tmp","max_age_hours":"soon"}]`))
	if err == nil {
		t.Fatal("expected a type error for string max_age_hours")
	}
}
