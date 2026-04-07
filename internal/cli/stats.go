package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pdonorio/claude-context-proxy/internal/config"
	"github.com/pdonorio/claude-context-proxy/internal/stats"
)

// CmdStats implements the "stats" subcommand.
func CmdStats(args []string, cfg *config.Config) {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	showTools := fs.Bool("tools", false, "show tool call frequency table")
	_ = fs.Parse(args)

	s := stats.LoadSession()
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

	sep := "─────────────────────────────────────"
	fmt.Printf("Session: %s (%s)\n", s.StartedAt.Local().Format("2006-01-02 15:04"), durStr)
	fmt.Println(sep)
	fmt.Printf("Requests:       %s\n", FmtInt(s.Requests))

	entries := stats.ReadHistory()
	n := len(entries)
	start := n - 10
	if start < 0 {
		start = 0
	}
	recent := entries[start:]

	if cfg.Mode == "cost" {
		inputCost := stats.CostUSD(cfg, cfg.DefaultModel, s.InputTokens, 0)
		outputCost := stats.CostUSD(cfg, cfg.DefaultModel, 0, s.OutputTokens)
		fmt.Printf("Input tokens:   %s  (~$%.2f)\n", FmtInt64(s.InputTokens), inputCost)
		fmt.Printf("Output tokens:  %s  (~$%.2f)\n", FmtInt64(s.OutputTokens), outputCost)
		fmt.Printf("Total cost:     ~$%.2f\n", inputCost+outputCost)
		fmt.Println(sep)

		if len(recent) == 0 {
			return
		}
		type indexed struct {
			idx   int
			entry stats.HistoryEntry
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
			fmt.Printf("  req #%-4d %s tokens\n", x.idx, FmtInt64(x.entry.Input))
			shown++
			if shown >= 5 {
				break
			}
		}
		if shown == 0 {
			fmt.Println("  (none)")
		}
	} else {
		// context mode
		windowSize := cfg.ContextWindow(cfg.DefaultModel)
		windows := FmtWindows(s.InputTokens, windowSize)
		fmt.Printf("Input tokens:   %s  (%s windows)\n", FmtInt64(s.InputTokens), windows)
		fmt.Printf("Output tokens:  %s\n", FmtInt64(s.OutputTokens))
		ratio := "N/A"
		if s.OutputTokens > 0 {
			ratio = fmt.Sprintf("%.1f:1", float64(s.InputTokens)/float64(s.OutputTokens))
		}
		fmt.Printf("Context ratio:  %s  (in:out)\n", ratio)
		fmt.Println(sep)

		if len(recent) == 0 {
			return
		}
		type indexed struct {
			idx   int
			entry stats.HistoryEntry
		}
		indexed2 := make([]indexed, len(recent))
		for i, e := range recent {
			indexed2[i] = indexed{start + i + 1, e}
		}
		sort.Slice(indexed2, func(i, j int) bool {
			return indexed2[i].entry.Input > indexed2[j].entry.Input
		})
		fmt.Println("Top context spikes (last 10 req):")
		shown := 0
		for _, x := range indexed2 {
			if x.entry.Input == 0 {
				continue
			}
			pct := FmtWindowPct(x.entry.Input, windowSize)
			fmt.Printf("  req #%-4d %s tokens  (%s of window)\n", x.idx, FmtInt64(x.entry.Input), pct)
			shown++
			if shown >= 5 {
				break
			}
		}
		if shown == 0 {
			fmt.Println("  (none)")
		}
	}

	if *showTools {
		fmt.Println(sep)
		PrintToolBreakdown(s.SessionID, entries)
	}
}

// PrintToolBreakdown prints a frequency table of tool calls for the session.
func PrintToolBreakdown(sessionID string, entries []stats.HistoryEntry) {
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

// CmdSessions implements the "sessions" subcommand.
func CmdSessions(cfg *config.Config) {
	entries := stats.ReadHistory()
	if len(entries) == 0 {
		fmt.Println("No history found.")
		return
	}

	type sessionRow struct {
		id        string
		startedAt time.Time
		requests  int
		input     int64
		output    int64
	}

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

	sort.Slice(order, func(i, j int) bool {
		return rows[order[i]].startedAt.After(rows[order[j]].startedAt)
	})

	fmt.Printf("%-20s  %-9s  %-12s  %-12s  %s\n", "Session", "Requests", "Input", "Output", "Cost")
	fmt.Println(strings.Repeat("─", 72))
	for _, sid := range order {
		r := rows[sid]
		cost := stats.CostUSD(cfg, cfg.DefaultModel, r.input, r.output)
		label := sid
		if sid != "(unknown)" {
			label = r.startedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Printf("%-20s  %-9s  %-12s  %-12s  $%.2f\n",
			label, FmtInt(r.requests), FmtInt64(r.input), FmtInt64(r.output), cost)
	}
}

// CmdHistory implements the "history" subcommand.
func CmdHistory(args []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	today := fs.Bool("today", false, "entries from today")
	since := fs.String("since", "", "entries on or after YYYY-MM-DD")
	sessionID := fs.String("session", "", "entries for specific session_id")
	last := fs.Bool("last", false, "entries from the most recent session")
	fs.Parse(args)

	entries := stats.ReadHistory()
	if len(entries) == 0 {
		fmt.Println("No history found.")
		return
	}

	noFilter := !*today && *since == "" && *sessionID == "" && !*last
	if noFilter {
		*last = true
	}

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

	var filtered []stats.HistoryEntry
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

	for i := len(filtered) - 1; i >= 0; i-- {
		e := filtered[i]
		fmt.Printf("%s  input=%s  output=%s  path=%s\n",
			e.TS.Local().Format("2006-01-02 15:04"),
			FmtInt64(e.Input), FmtInt64(e.Output), e.Path)
	}
	if len(filtered) == 0 {
		fmt.Println("No entries match.")
	}
}

// CmdStatusline implements the "statusline" subcommand.
func CmdStatusline(args []string, cfg *config.Config) {
	fs := flag.NewFlagSet("statusline", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "print raw JSON")
	fs.Parse(args)

	path := stats.StatuslinePath(cfg)
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

	var state stats.StatuslineState
	if json.Unmarshal(data, &state) != nil {
		return
	}

	// Stale check: >35 min ago.
	if time.Since(state.UpdatedAt) > 35*time.Minute {
		return
	}

	// Compute most-called tool from history for this session.
	toolSuffix := ""
	if entries := stats.ReadHistory(); len(entries) > 0 {
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

	if cfg.Mode == "cost" {
		fmt.Printf("⬡ %s in · %s out · $%.2f%s\n",
			FmtCompact(state.InputTokens),
			FmtCompact(state.OutputTokens),
			state.CostUSD,
			toolSuffix)
	} else {
		windowSize := cfg.ContextWindow(cfg.DefaultModel)
		ratio := "N/A"
		if state.OutputTokens > 0 {
			ratio = fmt.Sprintf("%.1f:1", float64(state.InputTokens)/float64(state.OutputTokens))
		}
		// Format windows with 'w' suffix for compact display.
		winVal := float64(state.InputTokens) / float64(windowSize)
		if winVal < 0.1 {
			winVal = 0.1
		}
		wStr := fmt.Sprintf("%.1fw", winVal)
		fmt.Printf("⬡ %s in · %s · %s%s\n",
			FmtCompact(state.InputTokens),
			wStr,
			ratio,
			toolSuffix)
	}
}

// CmdConfig implements the "config" subcommand.
func CmdConfig(args []string, cfg *config.Config) {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	pathOnly := fs.Bool("path", false, "print config file path only")
	_ = fs.Parse(args)

	if *pathOnly {
		fmt.Println(config.Path())
		return
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}

// FmtInt64 formats n with comma separators.
func FmtInt64(n int64) string {
	s := strconv.FormatInt(n, 10)
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

// FmtInt formats n with comma separators.
func FmtInt(n int) string { return FmtInt64(int64(n)) }

// FmtWindows formats cumulative tokens as "N.N×" context windows consumed.
// Minimum display value is "0.1×".
func FmtWindows(tokens, windowSize int64) string {
	if windowSize <= 0 {
		windowSize = 200000
	}
	ratio := float64(tokens) / float64(windowSize)
	if ratio < 0.1 {
		ratio = 0.1
	}
	return fmt.Sprintf("%.1f×", ratio)
}

// FmtWindowPct formats tokens as a percentage of the window size.
func FmtWindowPct(tokens, windowSize int64) string {
	if windowSize <= 0 {
		windowSize = 200000
	}
	pct := int(float64(tokens) / float64(windowSize) * 100)
	return fmt.Sprintf("%d%%", pct)
}

// FmtCompact formats n as a compact string (e.g. 1k, 1M).
func FmtCompact(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%dM", (n+500_000)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", (n+500)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
