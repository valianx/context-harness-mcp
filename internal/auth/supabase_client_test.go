package auth

// supabase_client_test.go verifies that GetUser emits the three structured log
// events (external_supabase_request / external_supabase_response /
// external_supabase_failed) defined by the external call observability pattern.
//
// Coverage:
//   - TestGetUser_SuccessPath_EmitsRequestAndResponse  (AC-ES.1, AC-ES.2)
//   - TestGetUser_NetworkError_EmitsRequestAndFailed   (AC-ES.3)
//   - TestGetUser_Non2xx_EmitsRequestAndFailed         (AC-ES.4)
//   - TestGetUser_RequestLogBeforeHTTPCall             (AC-ES.1 ordering)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// installAuthTestLogger replaces slog.Default with a JSON handler writing to buf.
// Returns a restore function.
func installAuthTestLogger(buf *bytes.Buffer) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(prev) }
}

// decodeAuthLogLines parses newline-delimited JSON from buf.
func decodeAuthLogLines(buf *bytes.Buffer) []map[string]any {
	var records []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err == nil {
			records = append(records, m)
		}
	}
	return records
}

// findAuthLog returns the first record whose "msg" field equals name.
func findAuthLog(records []map[string]any, name string) (map[string]any, bool) {
	for _, r := range records {
		if r["msg"] == name {
			return r, true
		}
	}
	return nil, false
}

// ── roundTripFunc lets tests inject arbitrary HTTP responses ─────────────────

// roundTripFunc implements http.RoundTripper via a plain function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// makeClient builds a supabaseHTTPClient backed by the provided RoundTripper.
func makeClient(rt http.RoundTripper) *supabaseHTTPClient {
	return &supabaseHTTPClient{
		projectURL: "https://example.supabase.co",
		anonKey:    "anon-key",
		client:     &http.Client{Transport: rt},
	}
}

// ── AC-ES.1 + AC-ES.2: success path ─────────────────────────────────────────

// TestGetUser_SuccessPath_EmitsRequestAndResponse verifies that a 200 response
// produces both external_supabase_request (INFO) and external_supabase_response
// (INFO) with the expected fields.
func TestGetUser_SuccessPath_EmitsRequestAndResponse(t *testing.T) {
	var buf bytes.Buffer
	restore := installAuthTestLogger(&buf)
	defer restore()

	body := `{"id":"abc-123","email":"foo@bar.com","email_confirmed_at":"2024-01-01T00:00:00Z"}`
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})

	user, err := makeClient(rt).GetUser(context.Background(), "tok-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user.ID != "abc-123" {
		t.Errorf("user.ID = %q, want abc-123", user.ID)
	}

	records := decodeAuthLogLines(&buf)

	// AC-ES.1: request log present with mandatory fields.
	reqLog, ok := findAuthLog(records, "external_supabase_request")
	if !ok {
		t.Fatal("expected 'external_supabase_request' log line")
	}
	if reqLog["service"] != "supabase" {
		t.Errorf("external_supabase_request service = %v, want supabase", reqLog["service"])
	}
	if reqLog["method"] != "GET" {
		t.Errorf("external_supabase_request method = %v, want GET", reqLog["method"])
	}
	if reqLog["endpoint"] != supabaseEndpoint {
		t.Errorf("external_supabase_request endpoint = %v, want %s", reqLog["endpoint"], supabaseEndpoint)
	}
	if reqLog["body"] != "GET "+supabaseEndpoint {
		t.Errorf("external_supabase_request body = %v, want 'GET %s'", reqLog["body"], supabaseEndpoint)
	}
	if reqLog["level"] != "INFO" {
		t.Errorf("external_supabase_request level = %v, want INFO", reqLog["level"])
	}

	// AC-ES.2: response log present with mandatory fields including the raw body.
	respLog, ok := findAuthLog(records, "external_supabase_response")
	if !ok {
		t.Fatal("expected 'external_supabase_response' log line")
	}
	if respLog["service"] != "supabase" {
		t.Errorf("external_supabase_response service = %v, want supabase", respLog["service"])
	}
	if respLog["endpoint"] != supabaseEndpoint {
		t.Errorf("external_supabase_response endpoint = %v, want %s", respLog["endpoint"], supabaseEndpoint)
	}
	statusCode, _ := respLog["status_code"].(float64) // JSON numbers unmarshal as float64.
	if int(statusCode) != http.StatusOK {
		t.Errorf("external_supabase_response status_code = %v, want 200", respLog["status_code"])
	}
	if _, hasDuration := respLog["duration_ms"]; !hasDuration {
		t.Error("external_supabase_response must carry duration_ms field")
	}
	if respLog["body"] != body {
		t.Errorf("external_supabase_response body = %v, want %s", respLog["body"], body)
	}
	if respLog["level"] != "INFO" {
		t.Errorf("external_supabase_response level = %v, want INFO", respLog["level"])
	}

	// No failed log must appear.
	if _, hasFailed := findAuthLog(records, "external_supabase_failed"); hasFailed {
		t.Error("successful call must NOT emit 'external_supabase_failed'")
	}
}

// ── AC-ES.3: network error path ──────────────────────────────────────────────

// TestGetUser_NetworkError_EmitsRequestAndFailed verifies that when Do returns
// a network error the method logs external_supabase_request then
// external_supabase_failed (ERROR) with the error message in the body field.
func TestGetUser_NetworkError_EmitsRequestAndFailed(t *testing.T) {
	var buf bytes.Buffer
	restore := installAuthTestLogger(&buf)
	defer restore()

	networkErr := errors.New("connection refused")
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, networkErr
	})

	_, err := makeClient(rt).GetUser(context.Background(), "tok-xyz")
	if err == nil {
		t.Fatal("expected error from GetUser on network failure")
	}

	records := decodeAuthLogLines(&buf)

	// AC-ES.1: request log must be emitted even on failure.
	if _, ok := findAuthLog(records, "external_supabase_request"); !ok {
		t.Fatal("expected 'external_supabase_request' before network error")
	}

	// AC-ES.3: failed log with the error message.
	failedLog, ok := findAuthLog(records, "external_supabase_failed")
	if !ok {
		t.Fatal("expected 'external_supabase_failed' on network error")
	}
	if failedLog["service"] != "supabase" {
		t.Errorf("external_supabase_failed service = %v, want supabase", failedLog["service"])
	}
	if failedLog["endpoint"] != supabaseEndpoint {
		t.Errorf("external_supabase_failed endpoint = %v, want %s", failedLog["endpoint"], supabaseEndpoint)
	}
	if _, hasDuration := failedLog["duration_ms"]; !hasDuration {
		t.Error("external_supabase_failed must carry duration_ms field")
	}
	bodyField, _ := failedLog["body"].(string)
	if !strings.Contains(bodyField, "connection refused") {
		t.Errorf("external_supabase_failed body = %q — expected to contain 'connection refused'", bodyField)
	}
	if failedLog["level"] != "ERROR" {
		t.Errorf("external_supabase_failed level = %v, want ERROR", failedLog["level"])
	}

	// No response log must appear.
	if _, hasResp := findAuthLog(records, "external_supabase_response"); hasResp {
		t.Error("network error must NOT emit 'external_supabase_response'")
	}
}

// ── AC-ES.4: non-2xx status path ─────────────────────────────────────────────

// TestGetUser_Non2xx_EmitsRequestAndFailed verifies that a 500 response emits
// external_supabase_failed (ERROR) with status_code=500 and the response body.
func TestGetUser_Non2xx_EmitsRequestAndFailed(t *testing.T) {
	var buf bytes.Buffer
	restore := installAuthTestLogger(&buf)
	defer restore()

	errBody := `{"error":"internal"}`
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader(errBody)),
			Header:     make(http.Header),
		}, nil
	})

	_, err := makeClient(rt).GetUser(context.Background(), "tok-500")
	if err == nil {
		t.Fatal("expected error from GetUser on 500 response")
	}

	records := decodeAuthLogLines(&buf)

	// AC-ES.1: request log always emitted first.
	if _, ok := findAuthLog(records, "external_supabase_request"); !ok {
		t.Fatal("expected 'external_supabase_request'")
	}

	// AC-ES.4: failed log carries status_code and Supabase error body.
	failedLog, ok := findAuthLog(records, "external_supabase_failed")
	if !ok {
		t.Fatal("expected 'external_supabase_failed' on 500 response")
	}
	statusCode, _ := failedLog["status_code"].(float64)
	if int(statusCode) != http.StatusInternalServerError {
		t.Errorf("external_supabase_failed status_code = %v, want 500", failedLog["status_code"])
	}
	if _, hasDuration := failedLog["duration_ms"]; !hasDuration {
		t.Error("external_supabase_failed must carry duration_ms field")
	}
	bodyField, _ := failedLog["body"].(string)
	if !strings.Contains(bodyField, `"error"`) {
		t.Errorf("external_supabase_failed body = %q — expected Supabase error JSON", bodyField)
	}
	if failedLog["level"] != "ERROR" {
		t.Errorf("external_supabase_failed level = %v, want ERROR", failedLog["level"])
	}
}

// ── AC-ES.1 log ordering guarantee ───────────────────────────────────────────

// TestGetUser_RequestLogBeforeHTTPCall verifies that external_supabase_request
// appears before external_supabase_response in the log output. Because both
// events share the same goroutine and the log is captured into a sequential
// buffer, position in the slice already proves ordering.
func TestGetUser_RequestLogBeforeHTTPCall(t *testing.T) {
	var buf bytes.Buffer
	restore := installAuthTestLogger(&buf)
	defer restore()

	body := `{"id":"ord-check","email":"order@test.com"}`
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})

	if _, err := makeClient(rt).GetUser(context.Background(), "tok-order"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	records := decodeAuthLogLines(&buf)

	var reqIdx, respIdx int = -1, -1
	for i, r := range records {
		switch r["msg"] {
		case "external_supabase_request":
			reqIdx = i
		case "external_supabase_response":
			respIdx = i
		}
	}

	if reqIdx == -1 {
		t.Fatal("external_supabase_request not found")
	}
	if respIdx == -1 {
		t.Fatal("external_supabase_response not found")
	}
	if reqIdx >= respIdx {
		t.Errorf("external_supabase_request (idx %d) must appear BEFORE external_supabase_response (idx %d)", reqIdx, respIdx)
	}
}
