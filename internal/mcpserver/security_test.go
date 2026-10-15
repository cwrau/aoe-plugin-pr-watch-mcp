package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

func post(t *testing.T, srv *httptest.Server, headers map[string]string, host string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(initializeBody))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestRejectsBrowserCrossOriginRequests(t *testing.T) {
	srv := newTestServer(t)
	for name, headers := range map[string]map[string]string{
		"foreign origin":        {"Origin": "https://evil.example"},
		"cross-site fetch meta": {"Sec-Fetch-Site": "cross-site"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := post(t, srv, headers, "").StatusCode; got != http.StatusForbidden {
				t.Errorf("status = %d, want 403", got)
			}
		})
	}
}

func TestRejectsRebindingHost(t *testing.T) {
	srv := newTestServer(t)
	if got := post(t, srv, nil, "evil.example").StatusCode; got != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a non-loopback Host", got)
	}
}

func TestAcceptsLocalRequests(t *testing.T) {
	srv := newTestServer(t)
	for name, headers := range map[string]map[string]string{
		"no browser headers": nil,
		"same origin":        {"Origin": srv.URL},
	} {
		t.Run(name, func(t *testing.T) {
			if got := post(t, srv, headers, "").StatusCode; got != http.StatusOK {
				t.Errorf("status = %d, want 200", got)
			}
		})
	}
}
