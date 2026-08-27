package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeB2Server is an in-process fake of the Backblaze B2 API surface used by
// the tool: b2_authorize_account, b2_list_buckets and b2_list_file_names.
// When paginate is true the listing is split across two pages.
func fakeB2Server(t *testing.T, keyID, appKey string, files []b2File, paginate bool) *httptest.Server {
	t.Helper()
	var (
		mu        sync.Mutex
		listCalls int
		srvURL    string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/b2_authorize_account"):
			gotID, gotKey, ok := r.BasicAuth()
			if !ok || gotID != keyID || gotKey != appKey {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":"bad_auth_credentials"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"accountId":          "fake-account",
				"apiUrl":             srvURL,
				"authorizationToken": "fake-auth-value",
			})
		case strings.HasSuffix(r.URL.Path, "/b2_list_buckets"):
			if r.Header.Get("Authorization") != "fake-auth-value" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"buckets": []map[string]string{
					{"bucketId": "fake-bucket", "bucketName": "YOUR_BUCKET"},
					{"bucketId": "other-bucket", "bucketName": "unrelated"},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/b2_list_file_names"):
			mu.Lock()
			listCalls++
			call := listCalls
			mu.Unlock()
			var req struct {
				BucketID string `json:"bucketId"`
				Prefix   string `json:"prefix"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.BucketID != "fake-bucket" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if paginate && call == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"files":        files[:1],
					"nextFileName": "page-two-start",
				})
				return
			}
			if paginate {
				_ = json.NewEncoder(w).Encode(map[string]any{"files": files[1:], "nextFileName": ""})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": files, "nextFileName": ""})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	srvURL = srv.URL
	return srv
}

func b2Leg(maxAgeHours float64) Leg {
	return Leg{
		Name:        "fake-b2",
		Kind:        kindB2,
		Bucket:      "YOUR_BUCKET",
		Prefix:      "dumps/",
		KeyIDEnv:    "B2_KEY_ID",
		KeyEnv:      "B2_APPLICATION_KEY",
		MaxAgeHours: maxAgeHours,
	}
}

func msAgo(now time.Time, d time.Duration) int64 { return now.Add(-d).UnixMilli() }

// withFakeAuthorize redirects the account-authorisation endpoint at the fake
// server so the whole B2 call chain runs against it.
func withFakeAuthorize(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := b2AuthorizeURL
	b2AuthorizeURL = srv.URL + "/b2api/v3/b2_authorize_account"
	t.Cleanup(func() { b2AuthorizeURL = old })
}

func TestCheckB2LegFreshAndStale(t *testing.T) {
	now := time.Now()
	files := []b2File{{FileName: "dump.sql.gz", UploadTimestamp: msAgo(now, time.Hour)}}
	srv := fakeB2Server(t, "key-id", "app-key", files, false)
	defer srv.Close()
	withFakeAuthorize(t, srv)

	t.Setenv("B2_KEY_ID", "key-id")
	t.Setenv("B2_APPLICATION_KEY", "app-key")

	// 1h old object, 30h limit: fresh.
	res := checkLeg(b2Leg(30), now, srv.Client())
	if res.state != stateFresh {
		t.Fatalf("1h old object with 30h limit should be fresh, got %v (%v)", res.state, res.err)
	}
	if res.age < 3500 || res.age > 3700 {
		t.Fatalf("age should be ~3600s, got %v", res.age)
	}

	// Same object, 0.5h limit: stale.
	res = checkLeg(b2Leg(0.5), now, srv.Client())
	if res.state != stateStale {
		t.Fatalf("1h old object with 0.5h limit should be stale, got %v", res.state)
	}
}

func TestCheckB2LegTakesNewestObject(t *testing.T) {
	now := time.Now()
	files := []b2File{
		{FileName: "a/old", UploadTimestamp: msAgo(now, 48*time.Hour)},
		{FileName: "b/new", UploadTimestamp: msAgo(now, 30*time.Minute)},
	}
	srv := fakeB2Server(t, "key-id", "app-key", files, false)
	defer srv.Close()
	withFakeAuthorize(t, srv)
	t.Setenv("B2_KEY_ID", "key-id")
	t.Setenv("B2_APPLICATION_KEY", "app-key")

	// Newest object is 30m old: fresh under a 1h limit even though one
	// object is 48h old.
	res := checkLeg(b2Leg(1), now, srv.Client())
	if res.state != stateFresh {
		t.Fatalf("newest object is 30m old, should be fresh under 1h limit, got %v (%v)", res.state, res.err)
	}
	if res.age < 1500 || res.age > 2100 {
		t.Fatalf("age should reflect the newest object (~1800s), got %v", res.age)
	}
}

func TestCheckB2LegBadCredentials(t *testing.T) {
	now := time.Now()
	srv := fakeB2Server(t, "key-id", "app-key", nil, false)
	defer srv.Close()
	withFakeAuthorize(t, srv)
	t.Setenv("B2_KEY_ID", "key-id")
	t.Setenv("B2_APPLICATION_KEY", "WRONG")

	res := checkLeg(b2Leg(1), now, srv.Client())
	if res.state != stateError || res.err == nil {
		t.Fatalf("bad credentials should error, got %v (%v)", res.state, res.err)
	}
}

func TestCheckB2LegMissingEnv(t *testing.T) {
	now := time.Now()
	t.Setenv("B2_KEY_ID", "")
	t.Setenv("B2_APPLICATION_KEY", "x")

	res := checkLeg(b2Leg(1), now, &http.Client{})
	if res.state != stateError {
		t.Fatalf("missing key env should error, got %v", res.state)
	}
	if !strings.Contains(res.err.Error(), "key_id_env") {
		t.Fatalf("error should name the missing variable, got %v", res.err)
	}
}

func TestCheckB2LegEmptyPrefixIsError(t *testing.T) {
	now := time.Now()
	srv := fakeB2Server(t, "key-id", "app-key", nil, false)
	defer srv.Close()
	withFakeAuthorize(t, srv)
	t.Setenv("B2_KEY_ID", "key-id")
	t.Setenv("B2_APPLICATION_KEY", "app-key")

	res := checkLeg(b2Leg(1), now, srv.Client())
	if !strings.Contains(res.err.Error(), "no objects") {
		t.Fatalf("error should explain the empty listing, got %v", res.err)
	}
}

func TestCheckB2LegPaginates(t *testing.T) {
	now := time.Now()
	files := []b2File{
		{FileName: "p/1", UploadTimestamp: msAgo(now, 10*time.Minute)},
		{FileName: "p/2", UploadTimestamp: msAgo(now, 25*time.Hour)},
	}
	srv := fakeB2Server(t, "key-id", "app-key", files, true)
	defer srv.Close()
	withFakeAuthorize(t, srv)
	t.Setenv("B2_KEY_ID", "key-id")
	t.Setenv("B2_APPLICATION_KEY", "app-key")

	// The newest object lives on the second page; the first page only has
	// the 25h-old object with a continuation marker.
	res := checkLeg(b2Leg(30), now, srv.Client())
	if res.state != stateFresh {
		t.Fatalf("newest object across pages is 10m old, should be fresh, got %v (%v)", res.state, res.err)
	}
	if res.age < 300 || res.age > 1200 {
		t.Fatalf("age should reflect the newest object found across pages (~600s), got %v", res.age)
	}
}

func TestCheckB2LegWrongBucketIsError(t *testing.T) {
	now := time.Now()
	srv := fakeB2Server(t, "key-id", "app-key", nil, false)
	defer srv.Close()
	withFakeAuthorize(t, srv)
	t.Setenv("B2_KEY_ID", "key-id")
	t.Setenv("B2_APPLICATION_KEY", "app-key")

	leg := b2Leg(1)
	leg.Bucket = "bucket-that-does-not-exist"
	res := checkLeg(leg, now, srv.Client())
	if res.state != stateError {
		t.Fatalf("unknown bucket should error, got %v", res.state)
	}
	if !strings.Contains(res.err.Error(), "not found") {
		t.Fatalf("error should mention the missing bucket, got %v", res.err)
	}
}
