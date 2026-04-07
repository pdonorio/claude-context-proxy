package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/pdonorio/claude-context-proxy/internal/cert"
	"github.com/pdonorio/claude-context-proxy/internal/cli"
	"github.com/pdonorio/claude-context-proxy/internal/config"
	"github.com/pdonorio/claude-context-proxy/internal/proxy"
	"github.com/pdonorio/claude-context-proxy/internal/stats"
)

const version = "0.1.2"

// Type aliases so that tests (package main) can use the unqualified names.
type Session = stats.Session
type HistoryEntry = stats.HistoryEntry
type StatuslineState = stats.StatuslineState
type Config = config.Config
type ModelPrice = config.ModelPrice

// Package globals — accessed directly by tests.
var (
	mu      sync.Mutex
	session *Session
	cfg     *config.Config
	wg      sync.WaitGroup // tracks in-flight recordTokens goroutines

	// CA cert/key for MITM proxy mode; nil when not initialised.
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
)

// ── Forwarding functions ────────────────────────────────────────────────────

func cacheBase() string     { return stats.CacheBase() }
func sessionFile() string   { return stats.SessionFile() }
func historyFile() string   { return stats.HistoryFile() }
func loadSession() *Session { return stats.LoadSession() }

func saveSession(s *Session) {
	if err := stats.SaveSession(s); err != nil {
		log.Printf("saveSession: %v", err)
	}
}

func readHistory() []HistoryEntry { return stats.ReadHistory() }

func appendHistory(e HistoryEntry) {
	if err := stats.AppendHistory(e); err != nil {
		log.Printf("appendHistory: %v", err)
	}
}

func statuslinePath() string                          { return stats.StatuslinePath(cfg) }
func costUSD(model string, in, out int64) float64     { return stats.CostUSD(cfg, model, in, out) }
func extractModel(body []byte) string                 { return stats.ExtractModel(cfg, body) }
func writeStatusline(s *Session)                      { stats.WriteStatusline(cfg, s) }
func newSSEInspector(r io.Reader) *proxy.SSEInspector { return proxy.NewSSEInspector(r, true) }
func fmtInt64(n int64) string                         { return cli.FmtInt64(n) }
func fmtCompact(n int64) string                       { return cli.FmtCompact(n) }
func fmtInt(n int) string                             { return cli.FmtInt(n) }
func defaultConfig() *config.Config                   { return config.Default() }
func loadConfig() *config.Config                      { return config.Load() }

// recordTokens applies token counts to the current session and persists stats.
func recordTokens(input, output int64, path, model string, tools []string, bd *stats.ContextBreakdown) {
	mu.Lock()
	session = stats.ApplyTokens(session, cfg, input, output)
	s := session
	mu.Unlock()

	if err := stats.SaveSession(s); err != nil {
		log.Printf("recordTokens: save session: %v", err)
	}
	stats.WriteStatusline(cfg, s)
	if err := stats.AppendHistory(HistoryEntry{
		SessionID: s.SessionID,
		TS:        s.LastRequestAt,
		Input:     input,
		Output:    output,
		Path:      path,
		Model:     model,
		Tools:     tools,
		Breakdown: bd,
	}); err != nil {
		log.Printf("recordTokens: append history: %v", err)
	}
}

// ── CLI forwarding ──────────────────────────────────────────────────────────

func cmdStats(args []string)      { cli.CmdStats(args, cfg) }
func cmdSessions()                { cli.CmdSessions(cfg) }
func cmdHistory(args []string)    { cli.CmdHistory(args) }
func cmdStatusline(args []string) { cli.CmdStatusline(args, cfg) }
func cmdConfig(args []string)     { cli.CmdConfig(args, cfg) }

// ── Daemon management ───────────────────────────────────────────────────────

// cmdStart launches the proxy as a background daemon.
// If _CCP_DAEMON=1 is set we are already the daemon child — run the server.
func cmdStart() {
	// Check if already running.
	if pid := stats.ReadPID(); pid != 0 {
		if proc, err := os.FindProcess(pid); err == nil {
			if proc.Signal(syscall.Signal(0)) == nil {
				fmt.Fprintf(os.Stderr, "proxy already running (pid %d)\n", pid)
				os.Exit(1)
			}
		}
		stats.RemovePID() // stale PID file
	}

	self, err := exec.LookPath(os.Args[0])
	if err != nil {
		self = os.Args[0]
	}

	logPath := stats.LogFile()
	if err := os.MkdirAll(stats.CacheBase(), 0o755); err != nil {
		log.Fatalf("start: mkdir: %v", err)
	}
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatalf("start: open log %s: %v", logPath, err)
	}

	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "_CCP_DAEMON=1")
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}
	lf.Close()

	fmt.Printf("proxy started (pid %d) — logs: %s\n", cmd.Process.Pid, logPath)
}

// cmdStop sends SIGTERM to the running proxy.
func cmdStop() {
	pid := stats.ReadPID()
	if pid == 0 {
		fmt.Fprintln(os.Stderr, "stop: proxy not running (no pid file)")
		os.Exit(1)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		log.Fatalf("stop: find process %d: %v", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		log.Fatalf("stop: signal %d: %v", pid, err)
	}
	fmt.Printf("proxy stopped (pid %d)\n", pid)
}

// cmdRestart stops the running proxy and starts a new daemon.
func cmdRestart() {
	pid := stats.ReadPID()
	if pid != 0 {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
			fmt.Printf("restart: sent SIGTERM to pid %d\n", pid)
			// Wait up to 3 s for old process to exit.
			for i := 0; i < 30; i++ {
				time.Sleep(100 * time.Millisecond)
				if stats.ReadPID() == 0 {
					break
				}
			}
		}
	}
	cmdStart()
}

// cmdHelp prints usage.
func cmdHelp() {
	fmt.Printf(`claude-context-proxy v%s

Usage:
  claude-context-proxy [command]

Daemon:
  start        Start proxy as background daemon
  stop         Stop the running daemon
  restart      Stop and restart the daemon
  log          Tail the daemon log (-f to follow)
  setup        Generate CA cert and install to keychain (for HTTPS_PROXY mode)
  -f           Run in foreground

Stats:
  stats        Current session token usage
  sessions     All past sessions
  history      Per-request breakdown (--last, --today, --since=DATE)
  statusline   Compact one-liner for shell prompts

Config:
  config       Show effective config (--path for file location)
  version      Print version
`, version)
}

// cmdSetup generates the CA certificate (if needed) and installs it into the
// macOS login keychain so that MITM interception is trusted by the OS.
func cmdSetup() {
	ca, _, err := cert.EnsureCA()
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup: generate CA: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("CA certificate: %s\n", cert.CACertPath())

	// Install into the user login keychain (no sudo required).
	cmd := exec.Command("security", "add-trusted-cert",
		"-d", "-r", "trustRoot",
		"-k", os.ExpandEnv("$HOME/Library/Keychains/login.keychain-db"),
		cert.CACertPath(),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "setup: install CA to keychain: %v\n", err)
		fmt.Fprintln(os.Stderr, "You may need to install it manually:")
		fmt.Fprintf(os.Stderr, "  security add-trusted-cert -d -r trustRoot -k ~/Library/Keychains/login.keychain-db %s\n", cert.CACertPath())
		os.Exit(1)
	}

	// Print the certificate's fingerprint for reference.
	fmt.Printf("Installed CA (valid until %s)\n", ca.NotAfter.Format("2006-01-02"))
	fmt.Println()
	fmt.Println("To route Claude Code through the proxy:")
	fmt.Printf("  export HTTPS_PROXY=http://localhost:%d\n", cfg.Port)
	fmt.Println()
	fmt.Println("Or for a single session:")
	fmt.Printf("  HTTPS_PROXY=http://localhost:%d claude\n", cfg.Port)
}

// cmdLog tails the daemon log file (last 40 lines, then follows).
func cmdLog() {
	logPath := stats.LogFile()
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "log: no log file at %s — has the proxy been started?\n", logPath)
		os.Exit(1)
	}
	cmd := exec.Command("tail", "-n", "40", "-f", logPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

// ── Server ──────────────────────────────────────────────────────────────────

func runServer() {
	stats.WritePID()
	defer stats.RemovePID()

	mu.Lock()
	session = loadSession()
	if session != nil {
		gap := time.Duration(cfg.SessionGapMinutes) * time.Minute
		if time.Since(session.LastRequestAt) > gap {
			session = nil
		}
	}
	mu.Unlock()

	// Load CA for MITM proxy mode (non-fatal if not set up yet).
	if ca, key, err := cert.EnsureCA(); err == nil {
		caCert, caKey = ca, key
	} else {
		log.Printf("mitm: CA not available (%v) — run 'claude-context-proxy setup' to enable HTTPS_PROXY mode", err)
	}

	onTokens := func(ti proxy.TokenInfo) {
		if cfg.Debug {
			log.Printf("tokens: input=%d output=%d (new=%d cache_read=%d cache_create=%d) path=%s model=%s",
				ti.Input, ti.Output, ti.NewInput, ti.CacheRead, ti.CacheCreation, ti.Path, ti.Model)
		}
		// Compute proportional context breakdown from request body section sizes.
		var bd *stats.ContextBreakdown
		cached := ti.CacheRead + ti.CacheCreation
		totalLen := ti.SystemLen + ti.ToolsLen + ti.MessagesLen
		if totalLen > 0 {
			bd = &stats.ContextBreakdown{
				NewMsgTokens:        ti.NewInput,
				CacheReadTokens:     ti.CacheRead,
				CacheCreationTokens: ti.CacheCreation,
				SystemTokens:        int64(float64(cached) * float64(ti.SystemLen) / float64(totalLen)),
				ToolsCount:          ti.ToolsCount,
				ToolsTokens:         int64(float64(cached) * float64(ti.ToolsLen) / float64(totalLen)),
				HistoryTokens:       int64(float64(cached) * float64(ti.MessagesLen) / float64(totalLen)),
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			recordTokens(ti.Input, ti.Output, ti.Path, ti.Model, ti.Tools, bd)
		}()
	}

	connectHandler := proxy.ConnectHandler(caCert, caKey, cfg, onTokens)
	regularHandler := proxy.Handler(proxy.Upstream, cfg, onTokens)

	// CONNECT requests use authority-form URIs (host:port) with no leading slash,
	// so they don't match ServeMux patterns — use a plain HandlerFunc instead.
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				connectHandler(w, r)
				return
			}
			regularHandler(w, r)
		}),
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("claude-context-proxy v%s listening on :%d → %s", version, cfg.Port, proxy.Upstream)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("server: %v", err)
		}
	}()

	<-stop
	log.Println("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("http shutdown: %v", err)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Println("all stats flushed")
	case <-ctx.Done():
		log.Println("flush timeout: some stats may not have been written")
	}
}

// ── main ────────────────────────────────────────────────────────────────────

func main() {
	cfg = config.Load()
	config.EnsureFile()

	// Daemon child: launched by cmdStart with _CCP_DAEMON=1 and no extra args.
	if os.Getenv("_CCP_DAEMON") == "1" {
		runServer()
		return
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start":
			cmdStart()
			return
		case "stop":
			cmdStop()
			return
		case "restart":
			cmdRestart()
			return
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
		case "config":
			cmdConfig(os.Args[2:])
			return
		case "setup":
			cmdSetup()
			return
		case "log", "logs":
			cmdLog()
			return
		case "help", "--help", "-h":
			cmdHelp()
			return
		case "version", "--version", "-v":
			fmt.Printf("claude-context-proxy v%s\n", version)
			return
		case "--foreground", "-f":
			runServer()
			return
		}
	}

	// Unknown subcommand.
	if len(os.Args) > 1 {
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		cmdHelp()
		os.Exit(1)
	}

	// Default: show help.
	cmdHelp()
}
