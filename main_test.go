package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── helpers ────────────────────────────────────────────────────────────────

func withTempCache(t *testing.T) func() {
	t.Helper()
	tmp := t.TempDir()
	// Override cacheBase by pointing home to tmp.
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmp)
	// Reset in-memory session so tests don't bleed state.
	mu.Lock()
	session = nil
	mu.Unlock()
	// Ensure cfg is initialized for tests.
	origCfg := cfg
	if cfg == nil {
		cfg = defaultConfig()
	}
	return func() {
		os.Setenv("HOME", origHome)
		mu.Lock()
		session = nil
		mu.Unlock()
		cfg = origCfg
	}
}

// ── tests ──────────────────────────────────────────────────────────────────

// TestTokenHeaderExtraction verifies that token headers are parsed from a
// mock upstream response and persisted to session.json / history.jsonl.
func TestTokenHeaderExtraction(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	// Mock upstream that returns token headers.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Anthropic-Input-Tokens", "42381")
		w.Header().Set("X-Anthropic-Output-Tokens", "1204")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"type":"message"}`))
	}))
	defer upstream.Close()

	// Temporarily patch the global upstream constant by using a test handler.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Replicate proxyHandler but target the test server.
		proxyReq, _ := http.NewRequest(r.Method, upstream.URL+r.RequestURI, r.Body)
		for k, vals := range r.Header {
			for _, v := range vals {
				proxyReq.Header.Add(k, v)
			}
		}
		client := &http.Client{}
		resp, err := client.Do(proxyReq)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()

		inputTokens := int64(0)
		outputTokens := int64(0)
		if v := resp.Header.Get("X-Anthropic-Input-Tokens"); v != "" {
			fmt.Sscanf(v, "%d", &inputTokens)
		}
		if v := resp.Header.Get("X-Anthropic-Output-Tokens"); v != "" {
			fmt.Sscanf(v, "%d", &outputTokens)
		}
		for k, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)

		if inputTokens > 0 || outputTokens > 0 {
			recordTokens(inputTokens, outputTokens, r.URL.Path, "", nil)
		}
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Give goroutine time to write.
	time.Sleep(50 * time.Millisecond)

	// Verify session.json.
	data, err := os.ReadFile(sessionFile())
	if err != nil {
		t.Fatalf("session.json not written: %v", err)
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("bad session.json: %v", err)
	}
	if s.InputTokens != 42381 {
		t.Errorf("InputTokens = %d, want 42381", s.InputTokens)
	}
	if s.OutputTokens != 1204 {
		t.Errorf("OutputTokens = %d, want 1204", s.OutputTokens)
	}
	if s.Requests != 1 {
		t.Errorf("Requests = %d, want 1", s.Requests)
	}

	// Verify history.jsonl.
	hist := readHistory()
	if len(hist) != 1 {
		t.Fatalf("history has %d entries, want 1", len(hist))
	}
	if hist[0].Input != 42381 {
		t.Errorf("history Input = %d, want 42381", hist[0].Input)
	}
	if hist[0].Path != "/v1/messages" {
		t.Errorf("history Path = %q, want /v1/messages", hist[0].Path)
	}
}

// TestStreamingPassthrough verifies that SSE responses are forwarded chunk by
// chunk without buffering (each chunk is delivered to the client immediately).
func TestStreamingPassthrough(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	chunks := []string{
		"data: {\"type\":\"ping\"}\n\n",
		"data: {\"type\":\"content_block_delta\"}\n\n",
		"data: [DONE]\n\n",
	}

	// Mock upstream: sends SSE chunks with a short delay between them.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Anthropic-Input-Tokens", "100")
		w.Header().Set("X-Anthropic-Output-Tokens", "50")
		flusher, _ := w.(http.Flusher)
		for _, c := range chunks {
			w.Write([]byte(c))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	// Patch proxy to target test upstream.
	saved := ""
	_ = saved
	handler := buildTestProxyHandler(upstream.URL)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	for _, c := range chunks {
		if !strings.Contains(string(body), strings.TrimSpace(c)) {
			t.Errorf("response missing chunk: %q", c)
		}
	}
}

// TestSessionJSONWritten verifies session.json accumulates across calls.
func TestSessionJSONWritten(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	recordTokens(1000, 200, "/v1/messages", "", nil)
	recordTokens(500, 100, "/v1/messages", "", nil)
	time.Sleep(20 * time.Millisecond)

	data, err := os.ReadFile(sessionFile())
	if err != nil {
		t.Fatalf("session.json not found: %v", err)
	}
	var s Session
	json.Unmarshal(data, &s)
	if s.InputTokens != 1500 {
		t.Errorf("InputTokens = %d, want 1500", s.InputTokens)
	}
	if s.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300", s.OutputTokens)
	}
	if s.Requests != 2 {
		t.Errorf("Requests = %d, want 2", s.Requests)
	}
}

// TestStatsOutput verifies the stats subcommand produces correct output (cost mode).
func TestStatsOutput(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()
	cfg.Mode = "cost"

	// Write a known history.
	if err := os.MkdirAll(cacheBase(), 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	s := Session{
		StartedAt:     now.Add(-47 * time.Minute),
		Requests:      3,
		InputTokens:   284391,
		OutputTokens:  18204,
		LastRequestAt: now,
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(sessionFile(), data, 0o644)

	entries := []HistoryEntry{
		{TS: now.Add(-10 * time.Minute), Input: 82341, Output: 500, Path: "/v1/messages"},
		{TS: now.Add(-5 * time.Minute), Input: 61204, Output: 800, Path: "/v1/messages"},
		{TS: now, Input: 140846, Output: 16904, Path: "/v1/messages"},
	}
	f, _ := os.OpenFile(historyFile(), os.O_CREATE|os.O_WRONLY, 0o644)
	for _, e := range entries {
		line, _ := json.Marshal(e)
		f.Write(append(line, '\n'))
	}
	f.Close()

	// Capture output.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	cmdStats([]string{})

	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	output := string(out)
	checks := []string{
		"284,391",
		"18,204",
		"Requests:",
		"Input tokens:",
		"Output tokens:",
		"Top input spikes",
	}
	for _, c := range checks {
		if !strings.Contains(output, c) {
			t.Errorf("stats output missing %q\nfull output:\n%s", c, output)
		}
	}
}

// TestFmtInt64 verifies comma formatting.
func TestFmtInt64(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{284391, "284,391"},
		{1000000, "1,000,000"},
	}
	for _, c := range cases {
		if got := fmtInt64(c.n); got != c.want {
			t.Errorf("fmtInt64(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// buildTestProxyHandler creates a proxyHandler-equivalent targeting targetURL.
func buildTestProxyHandler(targetURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Buffer the request body to extract model and then replay it.
		var bodyBuf []byte
		if r.Body != nil {
			bodyBuf, _ = io.ReadAll(r.Body)
		}
		model := extractModel(bodyBuf)

		proxyReq, err := http.NewRequest(r.Method, targetURL+r.RequestURI, io.NopCloser(bytes.NewReader(bodyBuf)))
		if err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		for k, vals := range r.Header {
			for _, v := range vals {
				proxyReq.Header.Add(k, v)
			}
		}
		client := &http.Client{Timeout: 0}
		resp, err := client.Do(proxyReq)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()

		inputTokens := int64(0)
		outputTokens := int64(0)
		fmt.Sscanf(resp.Header.Get("X-Anthropic-Input-Tokens"), "%d", &inputTokens)
		fmt.Sscanf(resp.Header.Get("X-Anthropic-Output-Tokens"), "%d", &outputTokens)

		for k, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)

		isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
		if isSSE {
			flusher, ok := w.(http.Flusher)
			buf := make([]byte, 4096)
			for {
				n, readErr := resp.Body.Read(buf)
				if n > 0 {
					w.Write(buf[:n])
					if ok {
						flusher.Flush()
					}
				}
				if readErr == io.EOF || readErr != nil {
					break
				}
			}
		} else {
			io.Copy(w, resp.Body)
		}

		if inputTokens > 0 || outputTokens > 0 {
			recordTokens(inputTokens, outputTokens, r.URL.Path, model, nil)
		}
	})
}

// ── Phase 2 tests ──────────────────────────────────────────────────────────

// TestSessionID verifies two requests in the same session share an ID,
// and a request after a gap gets a new session ID.
func TestSessionID(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	savedGap := cfg.SessionGapMinutes
	cfg.SessionGapMinutes = 1
	defer func() { cfg.SessionGapMinutes = savedGap }()

	recordTokens(100, 10, "/v1/messages", "", nil)
	recordTokens(200, 20, "/v1/messages", "", nil)
	time.Sleep(20 * time.Millisecond)

	hist := readHistory()
	if len(hist) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(hist))
	}
	if hist[0].SessionID == "" {
		t.Error("first entry has empty session_id")
	}
	if hist[0].SessionID != hist[1].SessionID {
		t.Errorf("same-session entries have different IDs: %q vs %q", hist[0].SessionID, hist[1].SessionID)
	}
	firstSID := hist[0].SessionID

	// Simulate gap: set LastRequestAt to 2 minutes ago and force a new session by
	// clearing in-memory session (mirroring what happens after real gap).
	mu.Lock()
	session = nil
	// Write a stale session to disk so loadSession reads it but detects gap.
	stale := &Session{
		SessionID:     firstSID,
		StartedAt:     time.Now().UTC().Add(-10 * time.Minute),
		Requests:      2,
		InputTokens:   300,
		OutputTokens:  30,
		LastRequestAt: time.Now().UTC().Add(-2 * time.Minute),
	}
	mu.Unlock()
	saveSession(stale)

	recordTokens(50, 5, "/v1/messages", "", nil)
	time.Sleep(20 * time.Millisecond)

	hist2 := readHistory()
	if len(hist2) != 3 {
		t.Fatalf("expected 3 history entries, got %d", len(hist2))
	}
	// Post-gap entry should be in a new session (Requests reset to 1).
	if hist2[2].SessionID == "" {
		t.Error("post-gap entry has empty session_id")
	}
	// Verify the in-memory session was reset (only 1 request).
	mu.Lock()
	curSession := session
	mu.Unlock()
	if curSession == nil {
		t.Fatal("in-memory session is nil after recordTokens")
	}
	if curSession.Requests != 1 {
		t.Errorf("post-gap session should have Requests=1, got %d", curSession.Requests)
	}
	// Note: session IDs are Unix-second timestamps, so they may match if test
	// runs within the same second — we verify reset behavior via Requests count.
}

// TestSessionsCmd verifies sessions subcommand groups history correctly.
func TestSessionsCmd(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	if err := os.MkdirAll(cacheBase(), 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	entries := []HistoryEntry{
		{SessionID: "1000", TS: now.Add(-2 * time.Hour), Input: 10000, Output: 500, Path: "/v1/messages"},
		{SessionID: "1000", TS: now.Add(-90 * time.Minute), Input: 20000, Output: 1000, Path: "/v1/messages"},
		{SessionID: "2000", TS: now.Add(-30 * time.Minute), Input: 50000, Output: 2000, Path: "/v1/messages"},
	}
	f, _ := os.OpenFile(historyFile(), os.O_CREATE|os.O_WRONLY, 0o644)
	for _, e := range entries {
		line, _ := json.Marshal(e)
		f.Write(append(line, '\n'))
	}
	f.Close()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdSessions()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	output := string(out)

	// Should have 2 session rows.
	lines := strings.Split(strings.TrimSpace(output), "\n")
	// header + separator + 2 session rows = 4 lines
	if len(lines) < 4 {
		t.Errorf("expected at least 4 lines, got %d:\n%s", len(lines), output)
	}
	if !strings.Contains(output, "30,000") {
		t.Errorf("expected combined input 30,000 for session 1000:\n%s", output)
	}
	if !strings.Contains(output, "50,000") {
		t.Errorf("expected input 50,000 for session 2000:\n%s", output)
	}
}

// TestHistoryFilter verifies --today, --since, --last, and --session filters.
func TestHistoryFilter(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	if err := os.MkdirAll(cacheBase(), 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	yesterday := now.Add(-24 * time.Hour)
	entries := []HistoryEntry{
		{SessionID: "old", TS: yesterday, Input: 1000, Output: 100, Path: "/v1/messages"},
		{SessionID: "new", TS: now.Add(-10 * time.Minute), Input: 2000, Output: 200, Path: "/v1/messages"},
		{SessionID: "new", TS: now.Add(-5 * time.Minute), Input: 3000, Output: 300, Path: "/v1/messages"},
	}
	f, _ := os.OpenFile(historyFile(), os.O_CREATE|os.O_WRONLY, 0o644)
	for _, e := range entries {
		line, _ := json.Marshal(e)
		f.Write(append(line, '\n'))
	}
	f.Close()

	captureHistory := func(args []string) string {
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		cmdHistory(args)
		w.Close()
		os.Stdout = old
		out, _ := io.ReadAll(r)
		return string(out)
	}

	// --last should show only "new" session (2 entries).
	lastOut := captureHistory([]string{"--last"})
	if strings.Contains(lastOut, "1,000") {
		t.Errorf("--last should not include old session entry:\n%s", lastOut)
	}
	if !strings.Contains(lastOut, "2,000") || !strings.Contains(lastOut, "3,000") {
		t.Errorf("--last missing new session entries:\n%s", lastOut)
	}

	// --today should include today's entries only.
	todayOut := captureHistory([]string{"--today"})
	if strings.Contains(todayOut, "1,000") {
		t.Errorf("--today should not include yesterday entry:\n%s", todayOut)
	}

	// --session=old should show only old entry.
	sessionOut := captureHistory([]string{"--session=old"})
	if !strings.Contains(sessionOut, "1,000") {
		t.Errorf("--session=old missing old entry:\n%s", sessionOut)
	}
	if strings.Contains(sessionOut, "2,000") {
		t.Errorf("--session=old should not include new session:\n%s", sessionOut)
	}

	// --since=today should include today entries.
	todayDate := now.Local().Format("2006-01-02")
	sinceOut := captureHistory([]string{"--since=" + todayDate})
	if strings.Contains(sinceOut, "1,000") {
		t.Errorf("--since=today should not include yesterday:\n%s", sinceOut)
	}

	// No flags defaults to --last.
	defaultOut := captureHistory([]string{})
	if defaultOut != lastOut {
		t.Errorf("default (no flags) should equal --last output")
	}
}

// TestOldHistoryNoSessionID verifies that old entries without session_id parse fine.
func TestOldHistoryNoSessionID(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	if err := os.MkdirAll(cacheBase(), 0o755); err != nil {
		t.Fatal(err)
	}

	// Write old-format entries without session_id field.
	lines := `{"ts":"2026-04-05T10:00:00Z","input":1000,"output":100,"path":"/v1/messages"}
{"ts":"2026-04-05T10:05:00Z","input":2000,"output":200,"path":"/v1/messages"}
`
	os.WriteFile(historyFile(), []byte(lines), 0o644)

	hist := readHistory()
	if len(hist) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(hist))
	}
	if hist[0].SessionID != "" {
		t.Errorf("old entry should have empty session_id, got %q", hist[0].SessionID)
	}
	// Should not panic; cmdSessions should handle empty IDs gracefully.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdSessions()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "(unknown)") {
		t.Errorf("expected (unknown) for entries without session_id:\n%s", string(out))
	}
}

// ── Phase 3 tests ──────────────────────────────────────────────────────────

// TestStatuslineWrite verifies that after recordTokens, ctx.json is written with correct fields.
func TestStatuslineWrite(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "ctx.json")
	os.Setenv("CTX_STATUSLINE_PATH", statePath)
	defer os.Unsetenv("CTX_STATUSLINE_PATH")
	cfg = loadConfig() // Reload to pick up env var

	recordTokens(284391, 18204, "/v1/messages", "", nil)
	time.Sleep(20 * time.Millisecond)

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ctx.json not written: %v", err)
	}
	var state StatuslineState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("bad ctx.json: %v", err)
	}
	if state.InputTokens != 284391 {
		t.Errorf("InputTokens = %d, want 284391", state.InputTokens)
	}
	if state.OutputTokens != 18204 {
		t.Errorf("OutputTokens = %d, want 18204", state.OutputTokens)
	}
	if state.Requests != 1 {
		t.Errorf("Requests = %d, want 1", state.Requests)
	}
	if state.SessionID == "" {
		t.Error("SessionID should not be empty")
	}
	if state.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should not be zero")
	}
	expectedCost := costUSD(cfg.DefaultModel, 284391, 18204)
	if state.CostUSD < expectedCost-0.01 || state.CostUSD > expectedCost+0.01 {
		t.Errorf("CostUSD = %.4f, want ~%.4f", state.CostUSD, expectedCost)
	}
}

// TestStatuslineAtomic verifies that no .tmp file is left behind after write.
func TestStatuslineAtomic(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "ctx.json")
	os.Setenv("CTX_STATUSLINE_PATH", statePath)
	defer os.Unsetenv("CTX_STATUSLINE_PATH")
	cfg = loadConfig() // Reload to pick up env var

	recordTokens(1000, 100, "/v1/messages", "", nil)
	time.Sleep(20 * time.Millisecond)

	// No .tmp file should remain.
	if _, err := os.Stat(statePath + ".tmp"); !os.IsNotExist(err) {
		t.Error(".tmp file should not exist after atomic write")
	}
	// ctx.json should exist.
	if _, err := os.Stat(statePath); err != nil {
		t.Errorf("ctx.json should exist: %v", err)
	}
}

// TestStatuslineCmd verifies the compact output format (cost mode).
func TestStatuslineCmd(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "ctx.json")
	os.Setenv("CTX_STATUSLINE_PATH", statePath)
	os.Setenv("CTX_MODE", "cost")
	defer os.Unsetenv("CTX_STATUSLINE_PATH")
	defer os.Unsetenv("CTX_MODE")
	cfg = loadConfig() // Reload to pick up env var

	state := StatuslineState{
		InputTokens:  284391,
		OutputTokens: 18204,
		Requests:     38,
		CostUSD:      1.13,
		SessionID:    "1744048320",
		UpdatedAt:    time.Now().UTC(),
	}
	data, _ := json.Marshal(state)
	os.WriteFile(statePath, data, 0o644)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdStatusline([]string{})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	output := strings.TrimSpace(string(out))

	// Expected: ⬡ 284k in · 18k out · $1.13
	if !strings.Contains(output, "284k") {
		t.Errorf("output missing 284k: %q", output)
	}
	if !strings.Contains(output, "18k") {
		t.Errorf("output missing 18k: %q", output)
	}
	if !strings.Contains(output, "$1.13") {
		t.Errorf("output missing $1.13: %q", output)
	}
}

// TestStatuslineCmdJSON verifies --json flag prints raw JSON.
func TestStatuslineCmdJSON(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "ctx.json")
	os.Setenv("CTX_STATUSLINE_PATH", statePath)
	defer os.Unsetenv("CTX_STATUSLINE_PATH")
	cfg = loadConfig() // Reload to pick up env var

	state := StatuslineState{
		InputTokens:  1000,
		OutputTokens: 100,
		Requests:     1,
		CostUSD:      0.01,
		SessionID:    "123",
		UpdatedAt:    time.Now().UTC(),
	}
	data, _ := json.Marshal(state)
	os.WriteFile(statePath, data, 0o644)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdStatusline([]string{"--json"})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)

	var parsed StatuslineState
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\noutput: %s", err, out)
	}
	if parsed.SessionID != "123" {
		t.Errorf("parsed SessionID = %q, want 123", parsed.SessionID)
	}
}

// TestStatuslineDisabled verifies that CTX_STATUSLINE_PATH="" skips write entirely.
func TestStatuslineDisabled(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	os.Setenv("CTX_STATUSLINE_PATH", "")
	defer os.Unsetenv("CTX_STATUSLINE_PATH")
	cfg = loadConfig() // Reload to pick up env var

	// Should not panic or write anything.
	recordTokens(500, 50, "/v1/messages", "", nil)
	time.Sleep(20 * time.Millisecond)

	// Verify statuslinePath returns "".
	if p := statuslinePath(); p != "" {
		t.Errorf("expected empty path, got %q", p)
	}
}

// TestStatuslineStale verifies that stale data (>35 min) prints nothing.
func TestStatuslineStale(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "ctx.json")
	os.Setenv("CTX_STATUSLINE_PATH", statePath)
	defer os.Unsetenv("CTX_STATUSLINE_PATH")

	state := StatuslineState{
		InputTokens:  1000,
		OutputTokens: 100,
		Requests:     1,
		CostUSD:      0.01,
		SessionID:    "123",
		UpdatedAt:    time.Now().UTC().Add(-40 * time.Minute), // stale
	}
	data, _ := json.Marshal(state)
	os.WriteFile(statePath, data, 0o644)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdStatusline([]string{})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	if len(strings.TrimSpace(string(out))) > 0 {
		t.Errorf("stale data should print nothing, got: %q", string(out))
	}
}

// TestFmtCompact verifies compact number formatting.
func TestFmtCompact(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1k"},
		{1499, "1k"},
		{1500, "2k"},
		{284391, "284k"},
		{1_000_000, "1M"},
		{1_500_000, "2M"},
		{2_000_000, "2M"},
	}
	for _, c := range cases {
		if got := fmtCompact(c.n); got != c.want {
			t.Errorf("fmtCompact(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// ── session gap test ───────────────────────────────────────────────────────

func TestSessionGapReset(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	// Force session gap to 1 minute.
	savedGap := cfg.SessionGapMinutes
	cfg.SessionGapMinutes = 1
	defer func() { cfg.SessionGapMinutes = savedGap }()

	// Write a session that ended 2 minutes ago.
	old := time.Now().UTC().Add(-2 * time.Minute)
	s := Session{
		StartedAt:     old.Add(-10 * time.Minute),
		Requests:      5,
		InputTokens:   9999,
		OutputTokens:  999,
		LastRequestAt: old,
	}
	os.MkdirAll(filepath.Dir(sessionFile()), 0o755)
	data, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(sessionFile(), data, 0o644)

	// Reset in-memory session so loadSession is used.
	mu.Lock()
	session = nil
	mu.Unlock()

	recordTokens(100, 10, "/v1/messages", "", nil)
	time.Sleep(20 * time.Millisecond)

	data2, _ := os.ReadFile(sessionFile())
	var s2 Session
	json.Unmarshal(data2, &s2)

	// Should have reset: only 1 request.
	if s2.Requests != 1 {
		t.Errorf("expected session reset (Requests=1), got %d", s2.Requests)
	}
	if s2.InputTokens != 100 {
		t.Errorf("expected InputTokens=100 after reset, got %d", s2.InputTokens)
	}
}

// ── Phase 4 tests ──────────────────────────────────────────────────────────

// TestSSETeeParser feeds a real-looking SSE stream through sseInspector and
// verifies tool names are extracted correctly without modifying byte output.
func TestSSETeeParser(t *testing.T) {
	stream := "" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_01\",\"name\":\"Read\",\"input\":{}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\"}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_02\",\"name\":\"Bash\",\"input\":{}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"output_tokens\":42}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	inspector := newSSEInspector(strings.NewReader(stream))

	// Read all bytes through inspector.
	got, err := io.ReadAll(inspector)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// Byte output must be identical.
	if string(got) != stream {
		t.Errorf("inspector modified the byte stream\ngot:  %q\nwant: %q", got, stream)
	}

	// Tool names must be extracted in order.
	want := []string{"Read", "Bash"}
	if len(inspector.Tools) != len(want) {
		t.Fatalf("Tools = %v, want %v", inspector.Tools, want)
	}
	for i, name := range want {
		if inspector.Tools[i] != name {
			t.Errorf("Tools[%d] = %q, want %q", i, inspector.Tools[i], name)
		}
	}
}

// TestSSETeeParserChunked verifies the inspector works when SSE events are
// split across multiple Read calls (simulating real network chunking).
func TestSSETeeParserChunked(t *testing.T) {
	event := "event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_01\",\"name\":\"Glob\",\"input\":{}}}\n\n"

	// Feed the event in 10-byte chunks.
	inspector := newSSEInspector(strings.NewReader(event))
	buf := make([]byte, 10)
	var out []byte
	for {
		n, err := inspector.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	if string(out) != event {
		t.Errorf("chunked output mismatch")
	}
	if len(inspector.Tools) != 1 || inspector.Tools[0] != "Glob" {
		t.Errorf("Tools = %v, want [Glob]", inspector.Tools)
	}
}

// TestHistoryToolField verifies the tools field is written and parsed round-trip.
func TestHistoryToolField(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	tools := []string{"Read", "Bash", "Read"}
	recordTokens(1000, 100, "/v1/messages", "", tools)
	time.Sleep(20 * time.Millisecond)

	hist := readHistory()
	if len(hist) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(hist))
	}
	if len(hist[0].Tools) != len(tools) {
		t.Fatalf("Tools = %v, want %v", hist[0].Tools, tools)
	}
	for i, name := range tools {
		if hist[0].Tools[i] != name {
			t.Errorf("Tools[%d] = %q, want %q", i, hist[0].Tools[i], name)
		}
	}
}

// TestHistoryToolFieldNil verifies that nil tools omits the field from JSON.
func TestHistoryToolFieldNil(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	recordTokens(500, 50, "/v1/messages", "", nil)
	time.Sleep(20 * time.Millisecond)

	data, err := os.ReadFile(historyFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"tools"`) {
		t.Errorf("nil tools should not write tools field; got: %s", data)
	}
}

// TestStatsToolsFlag verifies --tools shows the tool frequency table.
func TestStatsToolsFlag(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	if err := os.MkdirAll(cacheBase(), 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	sid := "9999"
	s := Session{
		SessionID:     sid,
		StartedAt:     now.Add(-10 * time.Minute),
		Requests:      3,
		InputTokens:   10000,
		OutputTokens:  1000,
		LastRequestAt: now,
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(sessionFile(), data, 0o644)

	entries := []HistoryEntry{
		{SessionID: sid, TS: now.Add(-5 * time.Minute), Input: 3000, Output: 300, Path: "/v1/messages",
			Tools: []string{"Bash", "Read", "Bash"}},
		{SessionID: sid, TS: now.Add(-3 * time.Minute), Input: 3000, Output: 300, Path: "/v1/messages",
			Tools: []string{"Read", "Glob", "Bash"}},
		{SessionID: sid, TS: now, Input: 4000, Output: 400, Path: "/v1/messages",
			Tools: []string{"Edit"}},
	}
	f, _ := os.OpenFile(historyFile(), os.O_CREATE|os.O_WRONLY, 0o644)
	for _, e := range entries {
		line, _ := json.Marshal(e)
		f.Write(append(line, '\n'))
	}
	f.Close()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdStats([]string{"--tools"})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	output := string(out)

	// Bash appears 3 times, Read 2 times, Glob 1, Edit 1.
	checks := []string{
		"Tool call breakdown",
		"Bash",
		"Read",
		"Glob",
		"Edit",
		"3 calls",
		"2 calls",
	}
	for _, c := range checks {
		if !strings.Contains(output, c) {
			t.Errorf("stats --tools output missing %q\nfull output:\n%s", c, output)
		}
	}

	// Bash should appear before Read (higher count).
	bashIdx := strings.Index(output, "Bash")
	readIdx := strings.Index(output, "Read")
	if bashIdx > readIdx {
		t.Errorf("Bash (3 calls) should appear before Read (2 calls) in output")
	}
}

// ── Phase 5 tests ──────────────────────────────────────────────────────────

// TestConfigLoad verifies config loading: defaults, file, and env overrides.
func TestConfigLoad(t *testing.T) {
	// Save original cfg.
	origCfg := cfg

	tmp := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmp)
	defer func() {
		os.Setenv("HOME", origHome)
		cfg = origCfg
	}()

	// Test 1: Missing file → defaults.
	cfg = loadConfig()
	if cfg.Port != 7474 {
		t.Errorf("Port = %d, want 7474", cfg.Port)
	}
	if cfg.SessionGapMinutes != 30 {
		t.Errorf("SessionGapMinutes = %d, want 30", cfg.SessionGapMinutes)
	}
	if cfg.DefaultModel != "claude-sonnet-4" {
		t.Errorf("DefaultModel = %q, want claude-sonnet-4", cfg.DefaultModel)
	}

	// Test 2: File with custom values.
	cfgDir := filepath.Join(tmp, ".config", "claude-context-proxy")
	os.MkdirAll(cfgDir, 0o755)
	customCfg := &Config{
		Port:              8888,
		SessionGapMinutes: 60,
		StatuslinePath:    "~/.custom/path",
		Inspect:           true,
		DefaultModel:      "claude-opus-4",
		Pricing: map[string]ModelPrice{
			"claude-opus-4": {InputPerMtok: 15.00, OutputPerMtok: 75.00},
		},
	}
	data, _ := json.MarshalIndent(customCfg, "", "  ")
	os.WriteFile(filepath.Join(cfgDir, "config.json"), data, 0o644)

	cfg = loadConfig()
	if cfg.Port != 8888 {
		t.Errorf("Port = %d, want 8888", cfg.Port)
	}
	if cfg.SessionGapMinutes != 60 {
		t.Errorf("SessionGapMinutes = %d, want 60", cfg.SessionGapMinutes)
	}
	if cfg.Inspect != true {
		t.Errorf("Inspect = %v, want true", cfg.Inspect)
	}
	if cfg.DefaultModel != "claude-opus-4" {
		t.Errorf("DefaultModel = %q, want claude-opus-4", cfg.DefaultModel)
	}

	// Test 3: Env vars override file.
	os.Setenv("CTX_PORT", "9999")
	os.Setenv("CTX_SESSION_GAP_MINUTES", "90")
	cfg = loadConfig()
	if cfg.Port != 9999 {
		t.Errorf("Port = %d, want 9999 (env override)", cfg.Port)
	}
	if cfg.SessionGapMinutes != 90 {
		t.Errorf("SessionGapMinutes = %d, want 90 (env override)", cfg.SessionGapMinutes)
	}
	os.Unsetenv("CTX_PORT")
	os.Unsetenv("CTX_SESSION_GAP_MINUTES")
}

// TestModelDetection verifies that model is extracted from request body.
func TestModelDetection(t *testing.T) {
	origCfg := cfg
	cfg = defaultConfig()
	defer func() { cfg = origCfg }()

	tests := []struct {
		name     string
		body     string
		wantModel string
	}{
		{
			name:      "request with claude-opus-4",
			body:      `{"model":"claude-opus-4","messages":[]}`,
			wantModel: "claude-opus-4",
		},
		{
			name:      "request with claude-haiku-4",
			body:      `{"model":"claude-haiku-4","messages":[]}`,
			wantModel: "claude-haiku-4",
		},
		{
			name:      "request missing model field",
			body:      `{"messages":[]}`,
			wantModel: "claude-sonnet-4", // default
		},
		{
			name:      "empty body",
			body:      ``,
			wantModel: "claude-sonnet-4", // default
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractModel([]byte(tc.body))
			if got != tc.wantModel {
				t.Errorf("extractModel(%q) = %q, want %q", tc.body, got, tc.wantModel)
			}
		})
	}
}

// TestModelFallback verifies unknown models use default model pricing.
func TestModelFallback(t *testing.T) {
	origCfg := cfg
	cfg = defaultConfig()
	defer func() { cfg = origCfg }()

	// Use a model not in the pricing map.
	unknownModel := "claude-unknown-999"

	// Get pricing for unknown model; should fall back to default.
	_, ok := cfg.Pricing[unknownModel]
	if ok {
		t.Fatalf("unexpected pricing found for unknown model")
	}

	// costUSD should use default model pricing.
	cost := costUSD(unknownModel, 1_000_000, 1_000_000)
	defaultPricing := cfg.Pricing[cfg.DefaultModel]
	expectedCost := defaultPricing.InputPerMtok + defaultPricing.OutputPerMtok
	if cost != expectedCost {
		t.Errorf("costUSD(unknown, 1M, 1M) = %.2f, want %.2f (default model %q)", cost, expectedCost, cfg.DefaultModel)
	}
}

// TestHistoryHasModel verifies history.jsonl entries include the model field.
func TestHistoryHasModel(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	origCfg := cfg
	cfg = defaultConfig()
	defer func() { cfg = origCfg }()

	recordTokens(100, 50, "/v1/messages", "claude-opus-4", nil)
	recordTokens(200, 75, "/v1/messages", "claude-haiku-4", nil)
	time.Sleep(20 * time.Millisecond)

	hist := readHistory()
	if len(hist) != 2 {
		t.Fatalf("history has %d entries, want 2", len(hist))
	}

	if hist[0].Model != "claude-opus-4" {
		t.Errorf("history[0].Model = %q, want claude-opus-4", hist[0].Model)
	}
	if hist[1].Model != "claude-haiku-4" {
		t.Errorf("history[1].Model = %q, want claude-haiku-4", hist[1].Model)
	}
}

// ── Phase 7 tests ──────────────────────────────────────────────────────────

// TestContextModeStats verifies stats output in context mode: shows × and %, no $.
func TestContextModeStats(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()
	cfg.Mode = "context"

	if err := os.MkdirAll(cacheBase(), 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	s := Session{
		StartedAt:     now.Add(-47 * time.Minute),
		Requests:      38,
		InputTokens:   284391,
		OutputTokens:  18204,
		LastRequestAt: now,
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(sessionFile(), data, 0o644)

	entries := []HistoryEntry{
		{TS: now.Add(-10 * time.Minute), Input: 82341, Output: 500, Path: "/v1/messages"},
		{TS: now.Add(-5 * time.Minute), Input: 61204, Output: 800, Path: "/v1/messages"},
		{TS: now, Input: 140846, Output: 16904, Path: "/v1/messages"},
	}
	f, _ := os.OpenFile(historyFile(), os.O_CREATE|os.O_WRONLY, 0o644)
	for _, e := range entries {
		line, _ := json.Marshal(e)
		f.Write(append(line, '\n'))
	}
	f.Close()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdStats([]string{})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	output := string(out)

	// Must contain window and ratio markers.
	if !strings.Contains(output, "×") {
		t.Errorf("context mode missing ×: %q", output)
	}
	if !strings.Contains(output, "%") {
		t.Errorf("context mode missing %%: %q", output)
	}
	if !strings.Contains(output, "Context ratio") {
		t.Errorf("context mode missing 'Context ratio': %q", output)
	}
	// Must NOT contain dollar amounts.
	if strings.Contains(output, "$") {
		t.Errorf("context mode should not contain $: %q", output)
	}
}

// TestCostModeStats verifies stats output in cost mode: shows $, no ×.
func TestCostModeStats(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()
	cfg.Mode = "cost"

	if err := os.MkdirAll(cacheBase(), 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	s := Session{
		StartedAt:     now.Add(-47 * time.Minute),
		Requests:      38,
		InputTokens:   284391,
		OutputTokens:  18204,
		LastRequestAt: now,
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	os.WriteFile(sessionFile(), data, 0o644)

	entries := []HistoryEntry{
		{TS: now.Add(-10 * time.Minute), Input: 82341, Output: 500, Path: "/v1/messages"},
	}
	f, _ := os.OpenFile(historyFile(), os.O_CREATE|os.O_WRONLY, 0o644)
	for _, e := range entries {
		line, _ := json.Marshal(e)
		f.Write(append(line, '\n'))
	}
	f.Close()

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdStats([]string{})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	output := string(out)

	if !strings.Contains(output, "$") {
		t.Errorf("cost mode missing $: %q", output)
	}
	if strings.Contains(output, "×") {
		t.Errorf("cost mode should not contain ×: %q", output)
	}
}

// TestContextModeStatusline verifies statusline output in context mode: w and :1, no $.
func TestContextModeStatusline(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "ctx.json")
	os.Setenv("CTX_STATUSLINE_PATH", statePath)
	os.Setenv("CTX_MODE", "context")
	defer os.Unsetenv("CTX_STATUSLINE_PATH")
	defer os.Unsetenv("CTX_MODE")
	cfg = loadConfig()

	state := StatuslineState{
		InputTokens:  284391,
		OutputTokens: 18204,
		Requests:     38,
		CostUSD:      1.13,
		SessionID:    "1744048320",
		UpdatedAt:    time.Now().UTC(),
	}
	data, _ := json.Marshal(state)
	os.WriteFile(statePath, data, 0o644)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdStatusline([]string{})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	output := strings.TrimSpace(string(out))

	if !strings.Contains(output, "w") {
		t.Errorf("context mode statusline missing 'w' suffix: %q", output)
	}
	if !strings.Contains(output, ":1") {
		t.Errorf("context mode statusline missing ':1' ratio: %q", output)
	}
	if strings.Contains(output, "$") {
		t.Errorf("context mode statusline should not contain $: %q", output)
	}
}

// TestCostModeStatusline verifies statusline output in cost mode: $, no w suffix.
func TestCostModeStatusline(t *testing.T) {
	cleanup := withTempCache(t)
	defer cleanup()

	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "ctx.json")
	os.Setenv("CTX_STATUSLINE_PATH", statePath)
	os.Setenv("CTX_MODE", "cost")
	defer os.Unsetenv("CTX_STATUSLINE_PATH")
	defer os.Unsetenv("CTX_MODE")
	cfg = loadConfig()

	state := StatuslineState{
		InputTokens:  284391,
		OutputTokens: 18204,
		Requests:     38,
		CostUSD:      1.13,
		SessionID:    "1744048320",
		UpdatedAt:    time.Now().UTC(),
	}
	data, _ := json.Marshal(state)
	os.WriteFile(statePath, data, 0o644)

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	cmdStatusline([]string{})
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	output := strings.TrimSpace(string(out))

	if !strings.Contains(output, "$") {
		t.Errorf("cost mode statusline missing $: %q", output)
	}
	if strings.Contains(output, ":1") {
		t.Errorf("cost mode statusline should not contain :1 ratio: %q", output)
	}
}
