package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var errTelegramUnconfigured = errors.New("TELEGRAM_BOT_TOKEN or TELEGRAM_CHAT_ID not set")

// telegramSendURL is overridable so tests can point it at an in-process fake.
var telegramSendURL = "https://api.telegram.org/bot%s/sendMessage"

// sendTelegram posts a message to the configured chat. It returns
// errTelegramUnconfigured when either environment variable is absent.
func (m *monitor) sendTelegram(text string) error {
	envKey := os.Getenv("TELEGRAM_BOT_TOKEN")
	envChat := os.Getenv("TELEGRAM_CHAT_ID")
	if envKey == "" || envChat == "" {
		return errTelegramUnconfigured
	}
	payload := struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}{ChatID: envChat, Text: text}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf(telegramSendURL, envKey), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("telegram API returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !out.OK {
		return errors.New("telegram API returned ok=false")
	}
	return nil
}
