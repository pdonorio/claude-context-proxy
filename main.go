package main

import (
	"bytes"
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
	Tools     []string  `json:"tools,omitempty"`
}

var (
	mu      sync.Mutex
	session *Session

	sessionGapMinutes int64 = 30
	inspectEnabled    bool
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

// ── Statusline state file ───────────────────────────────────────────────────

// StatuslineState is the schema of ~/.files/states/ctx.json.
type StatuslineState struct {
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	Requests     int       `json:"requests"`
	CostUSD      float64   `json:"cost_usd"`
	SessionID    string    `json:"session_id"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func statuslinePath() string {
	if v, ok := os.LookupEnv("CTX_STATUSLINE_PATH"); ok {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".files", "states", "ctx.json")
}

func writeStatusline(s *Session) {
	path := statuslinePath()
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("statusline: cannot create dir %s: %v", dir, err)
		return
	}
	cost := float64(s.InputTokens)/1_000_000*inputPriceMtok + float64(s.OutputTokens)/1_000_000*outputPriceMtok
	state := StatuslineState{
		InputTokens:  s.InputTokens,
		OutputTokens: s.OutputTokens,
		Requests:     s.Requests,
		CostUSD:      cost,
		SessionID:    s.SessionID,
		UpdatedAt:    time.Now().UTC(),
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("statusline: write tmp: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("statusline: rename: %v", err)
		_ = os.Remove(tmp)
	}
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

// ── SSE inspector ──────────────────────────────────────────────────────────

// sseEventData holds the minimal JSON fields needed from SSE events.
type sseEventData struct {
	Type         string `json:"type"`
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content_block"`
}

// sseInspector wraps an io.Reader and extracts tool names from SSE events inline.
// It is only instantiated when CTX_INSPECT=1; zero overhead in the default path.
type sseInspector struct {
	r     io.Reader
	buf   []byte
	Tools []string
}

func newSSEInspector(r io.Reader) *sseInspector { return &sseInspector{r: r} }

func (s *sseInspector) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.ingest(p[:n])
	}
	return n, err
}

func (s *sseInspector) ingest(chunk []byte) {
	s.buf = append(s.buf, chunk...)
	for {
		idx := bytes.Index(s.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		s.parseEvent(s.buf[:idx])
		s.buf = s.buf[idx+2:]
	}
}

func (s *sseInspector) parseEvent(raw []byte) {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		var ev sseEventData
		if json.Unmarshal(line[6:], &ev) != nil {
			continue
		}
		if ev.Type == "content_block_start" && ev.ContentBlock.Type == "tool_use" && ev.ContentBlock.Name != "" {
			s.Tools = append(s.Tools, ev.ContentBlock.Name)
		}
	}
}

func recordTokens(input, output int64, path string, tools []string) {
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
	writeStatusline(session)
	appendHistory(HistoryEntry{SessionID: session.SessionID, TS: now, Input: input, Output: output, Path: path, Tools: tools})
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
	var inspector *sseInspector
	if isSSE {
		bodyReader := io.Reader(resp.Body)
		if inspectEnabled {
			inspector = newSSEInspector(resp.Body)
			bodyReader = inspector
		}
		flusher, ok := w.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, readErr := bodyReader.Read(buf)
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
		var tools []string
		if inspector != nil {
			tools = inspector.Tools
		}
		go recordTokens(inputTokens, outputTokens, r.URL.Path, tools)
	}
}

// ── Stats CLI ──────────────────────────────────────────────────────────────

func cmdStats(args []string) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	showTools := fs.Bool("tools", false, "show tool call frequency table")
	_ = fs.Parse(args)

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

	if *showTools {
		fmt.Println(sep)
		printToolBreakdown(s.SessionID, entries)
	}
}

func printToolBreakdown(sessionID string, entries []HistoryEntry) {
	counts := map[string]int{}
	for _, e := range entries {
		if e.SessionID != sessionID {
			continue
		}
		for _, t := range e.Tools {
			counts[t]++
		}
	}
	fmt.Println("Tool call breakdown (current session):")
	if len(counts) == 0 {
		fmt.Println("  (no data — run proxy with CTX_INSPECT=1)")
		return
	}
	type pair struct {
		name  string
		count int
	}
	pairs := make([]pair, 0, len(counts))
	for k, v := range counts {
		pairs = append(pairs, pair{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].name < pairs[j].name
	})
	for _, p := range pairs {
		fmt.Printf("  %-10s %d calls\n", p.name, p.count)
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

// ── Statusline CLI ─────────────────────────────────────────────────────────

func fmtCompact(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%dM", (n+500_000)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", (n+500)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func cmdStatusline(args []string) {
	fs := flag.NewFlagSet("statusline", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print raw JSON")
	fs.Parse(args)

	path := statuslinePath()
	if path == "" {
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return // file missing — print nothing
	}

	if *jsonOut {
		fmt.Print(string(data))
		return
	}

	var state StatuslineState
	if json.Unmarshal(data, &state) != nil {
		return
	}

	// Stale check: >35 min ago.
	if time.Since(state.UpdatedAt) > 35*time.Minute {
		return
	}

	// Compute most-called tool from history for this session.
	toolSuffix := ""
	if entries := readHistory(); len(entries) > 0 {
		counts := map[string]int{}
		for _, e := range entries {
			if e.SessionID == state.SessionID {
				for _, t := range e.Tools {
					counts[t]++
				}
			}
		}
		if len(counts) > 0 {
			best, bestN := "", 0
			for t, c := range counts {
				if c > bestN || (c == bestN && t < best) {
					best, bestN = t, c
				}
			}
			toolSuffix = fmt.Sprintf(" · %s×%d", best, bestN)
		}
	}

	fmt.Printf("⬡ %s in · %s out · $%.2f%s\n",
		fmtCompact(state.InputTokens),
		fmtCompact(state.OutputTokens),
		state.CostUSD,
		toolSuffix)
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

	// Enable SSE inspection if requested.
	inspectEnabled = os.Getenv("CTX_INSPECT") == "1"

	// Subcommand dispatch.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "stats":
			cmdStats(os.Args[2:])
			return
		case "sessions":
			cmdSessions()
			return
		case "history":
			cmdHistory(os.Args[2:])
			return
		case "statusline":
			cmdStatusline(os.Args[2:])
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
