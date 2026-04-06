package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	upstream        = "https://api.anthropic.com"
	listenAddr      = ":7474"
	cacheDir        = ".cache/claude-context-proxy"
	inputPriceMtok  = 3.00  // $ per million input tokens
	outputPriceMtok = 15.00 // $ per million output tokens
)

// Session holds per-session accumulated stats.
type Session struct {
	SessionID     string    `json:"session_id"`
	StartedAt     time.Time `json:"started_at"`
	Requests      int       `json:"requests"`
	InputTokens   int64     `json:"input_tokens"`
	OutputTokens  int64     `json:"output_tokens"`
	LastRequestAt time.Time `json:"last_request_at"`
}

// HistoryEntry is one line in history.jsonl.
type HistoryEntry struct {
	SessionID string    `json:"session_id,omitempty"`
	TS        time.Time `json:"ts"`
	Input     int64     `json:"input"`
	Output    int64     `json:"output"`
	Path      string    `json:"path"`
}

var (
	mu      sync.Mutex
	session *Session

	sessionGapMinutes int64 = 30
)

func cacheBase() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, cacheDir)
}

func sessionFile() string  { return filepath.Join(cacheBase(), "session.json") }
func historyFile() string  { return filepath.Join(cacheBase(), "history.jsonl") }

func loadSession() *Session {
	data, err := os.ReadFile(sessionFile())
	if err != nil {
		return nil
	}
	var s Session
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	return &s
}

func saveSession(s *Session) {
	if err := os.MkdirAll(cacheBase(), 0o755); err != nil {
		return
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile(sessionFile(), data, 0o644)
}

func appendHistory(e HistoryEntry) {
	if err := os.MkdirAll(cacheBase(), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(historyFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line, _ := json.Marshal(e)
	_, _ = f.Write(append(line, '\n'))
}

func recordTokens(input, output int64, path string) {
	mu.Lock()
	defer mu.Unlock()

	now := time.Now().UTC()
	gap := time.Duration(sessionGapMinutes) * time.Minute

	if session == nil || (session.LastRequestAt != time.Time{} && now.Sub(session.LastRequestAt) > gap) {
		session = &Session{
			SessionID: fmt.Sprintf("%d", now.Unix()),
			StartedAt: now,
		}
	}

	session.Requests++
	session.InputTokens += input
	session.OutputTokens += output
	session.LastRequestAt = now

	saveSession(session)
	appendHistory(HistoryEntry{SessionID: session.SessionID, TS: now, Input: input, Output: output, Path: path})
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	target := upstream + r.RequestURI

	proxyReq, err := http.NewRequest(r.Method, target, r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Copy all request headers verbatim.
	for k, vals := range r.Header {
		for _, v := range vals {
			proxyReq.Header.Add(k, v)
		}
	}

	client := &http.Client{Timeout: 0} // no timeout — streaming can be long
	resp, err := client.Do(proxyReq)
	if err != nil {
		http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Extract token counts from response headers.
	inputTokens, _ := strconv.ParseInt(resp.Header.Get("X-Anthropic-Input-Tokens"), 10, 64)
	outputTokens, _ := strconv.ParseInt(resp.Header.Get("X-Anthropic-Output-Tokens"), 10, 64)

	// Copy response headers.
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream or buffer depending on content type.
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	if isSSE {
		flusher, ok := w.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if ok {
					flusher.Flush()
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				break
			}
		}
	} else {
		_, _ = io.Copy(w, resp.Body)
	}

	if inputTokens > 0 || outputTokens > 0 {
		go recordTokens(inputTokens, outputTokens, r.URL.Path)
	}
}

// ── Stats CLI ──────────────────────────────────────────────────────────────

func cmdStats() {
	s := loadSession()
	if s == nil {
		fmt.Println("No session data found. Start the proxy and make some requests.")
		return
	}

	dur := time.Since(s.StartedAt).Round(time.Minute)
	minutes := int(dur.Minutes())
	hours := minutes / 60
	durStr := fmt.Sprintf("%dm", minutes%60)
	if hours > 0 {
		durStr = fmt.Sprintf("%dh%dm", hours, minutes%60)
	}

	inputCost := float64(s.InputTokens) / 1_000_000 * inputPriceMtok
	outputCost := float64(s.OutputTokens) / 1_000_000 * outputPriceMtok

	sep := "─────────────────────────────────────"
	fmt.Printf("Session: %s (%s)\n", s.StartedAt.Local().Format("2006-01-02 15:04"), durStr)
	fmt.Println(sep)
	fmt.Printf("Requests:       %s\n", fmtInt(s.Requests))
	fmt.Printf("Input tokens:   %s  (~$%.2f)\n", fmtInt64(s.InputTokens), inputCost)
	fmt.Printf("Output tokens:  %s  (~$%.2f)\n", fmtInt64(s.OutputTokens), outputCost)
	fmt.Printf("Total cost:     ~$%.2f\n", inputCost+outputCost)
	fmt.Println(sep)

	// Top input spikes from last 10 history entries.
	entries := readHistory()
	n := len(entries)
	start := n - 10
	if start < 0 {
		start = 0
	}
	recent := entries[start:]
	if len(recent) == 0 {
		return
	}

	// Sort a copy by input descending.
	type indexed struct {
		idx   int
		entry HistoryEntry
	}
	indexed2 := make([]indexed, len(recent))
	for i, e := range recent {
		indexed2[i] = indexed{start + i + 1, e}
	}
	sort.Slice(indexed2, func(i, j int) bool {
		return indexed2[i].entry.Input > indexed2[j].entry.Input
	})

	fmt.Println("Top input spikes (last 10 req):")
	shown := 0
	for _, x := range indexed2 {
		if x.entry.Input == 0 {
			continue
		}
		fmt.Printf("  req #%-4d %s tokens\n", x.idx, fmtInt64(x.entry.Input))
		shown++
		if shown >= 5 {
			break
		}
	}
	if shown == 0 {
		fmt.Println("  (none)")
	}
}

// ── Sessions CLI ───────────────────────────────────────────────────────────

func cmdSessions() {
	entries := readHistory()
	if len(entries) == 0 {
		fmt.Println("No history found.")
		return
	}

	type sessionRow struct {
		id       string
		startedAt time.Time
		requests int
		input    int64
		output   int64
	}

	// Group by session_id. Preserve insertion order via a slice of IDs.
	order := []string{}
	rows := map[string]*sessionRow{}
	for _, e := range entries {
		sid := e.SessionID
		if sid == "" {
			sid = "(unknown)"
		}
		if _, ok := rows[sid]; !ok {
			order = append(order, sid)
			rows[sid] = &sessionRow{id: sid, startedAt: e.TS}
		}
		r := rows[sid]
		r.requests++
		r.input += e.Input
		r.output += e.Output
		if e.TS.Before(r.startedAt) {
			r.startedAt = e.TS
		}
	}

	// Sort newest-first by startedAt.
	sort.Slice(order, func(i, j int) bool {
		return rows[order[i]].startedAt.After(rows[order[j]].startedAt)
	})

	fmt.Printf("%-20s  %-9s  %-12s  %-12s  %s\n", "Session", "Requests", "Input", "Output", "Cost")
	fmt.Println(strings.Repeat("─", 72))
	for _, sid := range order {
		r := rows[sid]
		cost := float64(r.input)/1_000_000*inputPriceMtok + float64(r.output)/1_000_000*outputPriceMtok
		label := sid
		if sid != "(unknown)" {
			label = r.startedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Printf("%-20s  %-9s  %-12s  %-12s  $%.2f\n",
			label, fmtInt(r.requests), fmtInt64(r.input), fmtInt64(r.output), cost)
	}
}

// ── History CLI ────────────────────────────────────────────────────────────

func cmdHistory(args []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	today := fs.Bool("today", false, "entries from today")
	since := fs.String("since", "", "entries on or after YYYY-MM-DD")
	sessionID := fs.String("session", "", "entries for specific session_id")
	last := fs.Bool("last", false, "entries from the most recent session")
	fs.Parse(args)

	entries := readHistory()
	if len(entries) == 0 {
		fmt.Println("No history found.")
		return
	}

	// Determine filter mode. Default to --last if no filter given.
	noFilter := !*today && *since == "" && *sessionID == "" && !*last
	if noFilter {
		*last = true
	}

	// Find most-recent session ID if needed.
	lastSID := ""
	if *last {
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i].SessionID != "" {
				lastSID = entries[i].SessionID
				break
			}
		}
	}

	now := time.Now()
	todayStr := now.Local().Format("2006-01-02")

	var sinceTime time.Time
	if *since != "" {
		t, err := time.ParseInLocation("2006-01-02", *since, time.Local)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid --since date: %v\n", err)
			os.Exit(1)
		}
		sinceTime = t
	}

	var filtered []HistoryEntry
	for _, e := range entries {
		if *today && e.TS.Local().Format("2006-01-02") != todayStr {
			continue
		}
		if *since != "" && e.TS.Local().Before(sinceTime) {
			continue
		}
		if *sessionID != "" && e.SessionID != *sessionID {
			continue
		}
		if *last && e.SessionID != lastSID {
			continue
		}
		filtered = append(filtered, e)
	}

	// Print newest-first.
	for i := len(filtered) - 1; i >= 0; i-- {
		e := filtered[i]
		fmt.Printf("%s  input=%s  output=%s  path=%s\n",
			e.TS.Local().Format("2006-01-02 15:04"),
			fmtInt64(e.Input), fmtInt64(e.Output), e.Path)
	}
	if len(filtered) == 0 {
		fmt.Println("No entries match.")
	}
}

func readHistory() []HistoryEntry {
	data, err := os.ReadFile(historyFile())
	if err != nil {
		return nil
	}
	var entries []HistoryEntry
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e HistoryEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			entries = append(entries, e)
		}
	}
	return entries
}

func fmtInt(n int) string    { return fmtInt64(int64(n)) }
func fmtInt64(n int64) string {
	s := strconv.FormatInt(n, 10)
	// Insert commas.
	out := []byte(s)
	result := make([]byte, 0, len(out)+len(out)/3)
	for i, c := range out {
		if i > 0 && (len(out)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, c)
	}
	return string(result)
}

// ── main ───────────────────────────────────────────────────────────────────

func main() {
	// Read session gap from env.
	if v := os.Getenv("CTX_SESSION_GAP_MINUTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			sessionGapMinutes = n
		}
	}

	// Subcommand dispatch.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "stats":
			cmdStats()
			return
		case "sessions":
			cmdSessions()
			return
		case "history":
			cmdHistory(os.Args[2:])
			return
		}
	}

	// Load existing session from disk so we survive restarts within gap.
	mu.Lock()
	session = loadSession()
	if session != nil {
		gap := time.Duration(sessionGapMinutes) * time.Minute
		if time.Since(session.LastRequestAt) > gap {
			session = nil
		}
	}
	mu.Unlock()

	http.HandleFunc("/", proxyHandler)
	log.Printf("claude-context-proxy listening on %s → %s", listenAddr, upstream)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}
