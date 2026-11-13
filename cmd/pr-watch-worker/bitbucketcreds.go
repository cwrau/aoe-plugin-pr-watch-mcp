package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

const (
	bitbucketEmailSettingKey = "bitbucket_email"
	bitbucketTokenSettingKey = "bitbucket_token"
)

// bitbucketCredentials caches the Bitbucket email and token settings read from
// the host. Unlike GitHub and GitLab there is no CLI to ask, so the token lives
// in the plugin's settings. get() is called per request and must never block on
// the host; refreshes happen out of band, same shape as snoozeRules.
type bitbucketCredentials struct {
	mu     sync.RWMutex
	email  string
	token  string
	loaded bool
}

func (b *bitbucketCredentials) get() (email, token string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.email, b.token
}

// refresh re-reads both settings. If either read fails the previous values stay
// in force: a transient host hiccup must not silently stop every Bitbucket
// watch.
func (b *bitbucketCredentials) refresh(ctx context.Context, rpc *hostRPC, logger *slog.Logger) {
	email, ok := readStringSetting(ctx, rpc, logger, bitbucketEmailSettingKey)
	if !ok {
		return
	}
	token, ok := readStringSetting(ctx, rpc, logger, bitbucketTokenSettingKey)
	if !ok {
		return
	}
	b.mu.Lock()
	changed := !b.loaded || b.email != email || b.token != token
	b.email, b.token, b.loaded = email, token, true
	b.mu.Unlock()
	if changed {
		// Never the values themselves: a short hash tells which token this is.
		logger.Info("bitbucket credentials loaded", "token_set", token != "", "token_fingerprint", tokenFingerprint(token), "email_set", email != "")
	}
}

// tokenFingerprint is the first 8 hex digits of the token's SHA-256: enough to
// tell two tokens apart in a log, nothing that helps recover one. Empty for no
// token.
func tokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:8]
}

func readStringSetting(ctx context.Context, rpc *hostRPC, logger *slog.Logger, key string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, configGetTimeout)
	defer cancel()
	raw, err := rpc.call(ctx, "config.get", map[string]any{"key": key})
	if err != nil {
		logger.Warn("read setting; keeping the previous value", "key", key, "error", err)
		return "", false
	}
	value, err := parseStringSetting(raw)
	if err != nil {
		logger.Warn("parse setting; keeping the previous value", "key", key, "error", err)
		return "", false
	}
	return value, true
}

// parseStringSetting decodes a config.get reply for a plain string setting:
// {"value": "..."}. A null or absent value is unset (empty).
func parseStringSetting(raw json.RawMessage) (string, error) {
	var reply struct {
		Value *string `json:"value"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", fmt.Errorf("decode config.get reply: %w", err)
	}
	if reply.Value == nil {
		return "", nil
	}
	return strings.TrimSpace(*reply.Value), nil
}
