package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestParseStringSetting(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "value", raw: `{"value": "abc"}`, want: "abc"},
		{name: "trimmed", raw: `{"value": "  abc\n"}`, want: "abc"},
		{name: "null is unset", raw: `{"value": null}`, want: ""},
		{name: "absent is unset", raw: `{}`, want: ""},
	}
	for _, tt := range tests {
		got, err := parseStringSetting(json.RawMessage(tt.raw))
		if err != nil || got != tt.want {
			t.Errorf("%s: parseStringSetting(%s) = %q, %v; want %q", tt.name, tt.raw, got, err, tt.want)
		}
	}
	if _, err := parseStringSetting(json.RawMessage(`{"value": ["not", "a string"]}`)); err == nil {
		t.Error("parseStringSetting() error = nil, want an error for a non-string value")
	}
}

// The worker reads these keys with config.get; a key the manifest does not
// declare could never be set in the host's settings.
func TestManifestDeclaresTheBitbucketSettings(t *testing.T) {
	manifest, err := os.ReadFile("../../aoe-plugin.toml")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, key := range []string{bitbucketEmailSettingKey, bitbucketTokenSettingKey} {
		re := regexp.MustCompile(`(?m)^\[\[settings\]\]\nkey = "` + key + `"\n(?:[^\n]+\n)*?type = "string"\n`)
		if !re.Match(manifest) {
			t.Errorf("manifest declares no string setting %q", key)
		}
	}
}

// The log says which token the worker holds without revealing it, so a token
// that "works in curl" can be compared with the one the worker really loaded.
func TestBitbucketCredentialsLogAFingerprintNotTheToken(t *testing.T) {
	h := newReplyingHost(`{"value": "s3cret-token"}`)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	(&bitbucketCredentials{}).refresh(context.Background(), h.rpc, logger)

	sum := sha256.Sum256([]byte("s3cret-token"))
	want := "token_fingerprint=" + hex.EncodeToString(sum[:])[:8]
	out := buf.String()
	if !strings.Contains(out, want) {
		t.Errorf("log = %q, want %q", out, want)
	}
	if strings.Contains(out, "s3cret-token") {
		t.Errorf("log = %q leaks the token", out)
	}
}

func TestBitbucketCredentialsRefreshReadsBothSettings(t *testing.T) {
	h := newReplyingHost(`{"value": " s3cret "}`)
	c := &bitbucketCredentials{}
	c.refresh(context.Background(), h.rpc, testLogger())

	email, token := c.get()
	if email != "s3cret" || token != "s3cret" {
		t.Errorf("get() = %q, %q; want both trimmed from the (shared) fake reply", email, token)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	joined := strings.Join(h.params, " ")
	for _, key := range []string{`"bitbucket_email"`, `"bitbucket_token"`} {
		if !strings.Contains(joined, key) {
			t.Errorf("config.get requests = %s, want %s asked for", joined, key)
		}
	}
}

// A transient host failure must not wipe the token: that would silently stop
// every Bitbucket watch, the same guarantee the snooze rules have.
func TestBitbucketCredentialsKeepPreviousOnFailureAndFollowAClearedSetting(t *testing.T) {
	h := newReplyingHost(`{"value": "tok"}`)
	c := &bitbucketCredentials{}
	c.refresh(context.Background(), h.rpc, testLogger())

	h.mu.Lock()
	h.replyErr = true
	h.mu.Unlock()
	c.refresh(context.Background(), h.rpc, testLogger())
	if _, token := c.get(); token != "tok" {
		t.Errorf("token after a failed refresh = %q, want the previous one kept", token)
	}

	h.mu.Lock()
	h.replyErr = false
	h.reply = `{"value": null}`
	h.mu.Unlock()
	c.refresh(context.Background(), h.rpc, testLogger())
	if email, token := c.get(); email != "" || token != "" {
		t.Errorf("credentials after clearing the settings = %q, %q; want none", email, token)
	}
}
