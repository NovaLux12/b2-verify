package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	// maxB2Pages bounds b2_list_file_names pagination so one pathological
	// bucket cannot stall the watchdog indefinitely.
	maxB2Pages = 5
)

// b2AuthorizeURL is the Backblaze B2 API endpoint for account authorisation.
// It is a variable (not a constant) so tests can redirect it to a fake.
var b2AuthorizeURL = "https://api.backblazeb2.com/b2api/v3/b2_authorize_account"

// b2File is one object listing entry from b2_list_file_names.
type b2File struct {
	FileName        string `json:"fileName"`
	UploadTimestamp int64  `json:"uploadTimestamp"`
}

// b2API is a minimal client for the Backblaze B2 native API, covering
// b2_authorize_account, b2_list_buckets and b2_list_file_names.
type b2API struct {
	apiURL    string
	auth      string
	accountID string
	http      *http.Client
}

// checkB2Leg resolves the newest object's age in seconds for a b2 leg.
func checkB2Leg(leg Leg, now time.Time, hc *http.Client) (float64, error) {
	keyID := os.Getenv(leg.KeyIDEnv)
	if keyID == "" {
		return 0, fmt.Errorf("environment variable %s (key_id_env) is not set", leg.KeyIDEnv)
	}
	appKey := os.Getenv(leg.KeyEnv)
	if appKey == "" {
		return 0, fmt.Errorf("environment variable %s (key_env) is not set", leg.KeyEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	api, err := authorizeB2(ctx, keyID, appKey, hc)
	if err != nil {
		return 0, err
	}
	bucketID, err := api.bucketID(ctx, leg.Bucket)
	if err != nil {
		return 0, err
	}
	files, err := api.listFileNames(ctx, bucketID, leg.Prefix)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("no objects found under prefix %q in bucket %q", leg.Prefix, leg.Bucket)
	}
	var latest int64
	for _, f := range files {
		if f.UploadTimestamp > latest {
			latest = f.UploadTimestamp
		}
	}
	return float64(now.UnixMilli()-latest) / 1000, nil
}

// authorizeB2 exchanges an application key ID and key for an API URL and an
// authorisation token.
func authorizeB2(ctx context.Context, keyID, appKey string, hc *http.Client) (*b2API, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b2AuthorizeURL, nil)
	if err != nil {
		return nil, fmt.Errorf("authorise: build request: %w", err)
	}
	req.SetBasicAuth(keyID, appKey)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authorise: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authorise: unexpected HTTP status %d", resp.StatusCode)
	}
	var out struct {
		AccountID string `json:"accountId"`
		APIURL    string `json:"apiUrl"`
		Auth      string `json:"authorizationToken"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("authorise: decode response: %w", err)
	}
	if out.APIURL == "" || out.Auth == "" {
		return nil, fmt.Errorf("authorise: response missing apiUrl or authorizationToken")
	}
	api := &b2API{apiURL: out.APIURL, accountID: out.AccountID, http: hc}
	api.auth = out.Auth
	return api, nil
}

// bucketID resolves a bucket name to its ID via b2_list_buckets.
func (a *b2API) bucketID(ctx context.Context, name string) (string, error) {
	var out struct {
		Buckets []struct {
			BucketID   string `json:"bucketId"`
			BucketName string `json:"bucketName"`
		} `json:"buckets"`
	}
	if err := a.post(ctx, "/b2api/v3/b2_list_buckets", map[string]any{"accountId": a.accountID}, &out); err != nil {
		return "", err
	}
	for _, b := range out.Buckets {
		if b.BucketName == name {
			return b.BucketID, nil
		}
	}
	return "", fmt.Errorf("bucket %q not found in account", name)
}

// listFileNames returns the objects under prefix, walking at most maxB2Pages
// pages of 1,000 objects each.
func (a *b2API) listFileNames(ctx context.Context, bucketID, prefix string) ([]b2File, error) {
	var all []b2File
	start := ""
	for page := 0; page < maxB2Pages; page++ {
		payload := map[string]any{"bucketId": bucketID, "maxFileCount": 1000}
		if prefix != "" {
			payload["prefix"] = prefix
		}
		if start != "" {
			payload["startFileName"] = start
		}
		var out struct {
			Files        []b2File `json:"files"`
			NextFileName string   `json:"nextFileName"`
		}
		if err := a.post(ctx, "/b2api/v3/b2_list_file_names", payload, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Files...)
		if out.NextFileName == "" {
			return all, nil
		}
		start = out.NextFileName
	}
	return all, nil
}

func (a *b2API) post(ctx context.Context, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%s: marshal request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", path, err)
	}
	req.Header.Set("Authorization", a.auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: unexpected HTTP status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("%s: decode response: %w", path, err)
	}
	return nil
}
