package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/pdonorio/claude-context-proxy/internal/proxy"
)

// setupBench initialises a temp cache dir (with statusline disabled) and
// returns a cleanup function. Statusline is disabled to avoid concurrent-write
// noise in the benchmark output.
func setupBench(b *testing.B) func() {
	b.Helper()
	tmp := b.TempDir()
	origHome := os.Getenv("HOME")
	origSL := os.Getenv("CTX_STATUSLINE_PATH")
	os.Setenv("HOME", tmp)
	os.Setenv("CTX_STATUSLINE_PATH", "") // disable statusline writes
	mu.Lock()
	session = nil
	mu.Unlock()
	origCfg := cfg
	cfg = loadConfig() // pick up env overrides (empty statusline path)
	return func() {
		os.Setenv("HOME", origHome)
		if origSL == "" {
			os.Unsetenv("CTX_STATUSLINE_PATH")
		} else {
			os.Setenv("CTX_STATUSLINE_PATH", origSL)
		}
		mu.Lock()
		session = nil
		mu.Unlock()
		cfg = origCfg
	}
}

// BenchmarkProxyHandler measures the overhead of the proxy layer using a mock upstream.
func BenchmarkProxyHandler(b *testing.B) {
	cleanup := setupBench(b)
	defer cleanup()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Anthropic-Input-Tokens", "1000")
		w.Header().Set("X-Anthropic-Output-Tokens", "100")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"type":"message"}`))
	}))
	defer upstream.Close()

	handler := proxy.Handler(upstream.URL, cfg, func(ti proxy.TokenInfo) {
		// no-op: benchmarking proxy overhead, not stats writes
		_ = ti
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Use a single shared client with connection reuse to avoid port exhaustion.
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 100}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := client.Get(srv.URL + "/v1/messages")
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkRecordTokens measures stats write throughput (sequential).
func BenchmarkRecordTokens(b *testing.B) {
	cleanup := setupBench(b)
	defer cleanup()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recordTokens(1000, 100, "/v1/messages", "claude-sonnet-4", nil, nil)
	}
}

// BenchmarkSSETeeParser measures tee-parser overhead versus a raw io.Copy.
func BenchmarkSSETeeParser(b *testing.B) {
	stream := "" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_01\",\"name\":\"Read\",\"input\":{}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\"}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_02\",\"name\":\"Bash\",\"input\":{}}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	b.Run("RawCopy", func(b *testing.B) {
		b.SetBytes(int64(len(stream)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			io.Copy(io.Discard, strings.NewReader(stream))
		}
	})

	b.Run("SSEInspector", func(b *testing.B) {
		b.SetBytes(int64(len(stream)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			inspector := newSSEInspector(strings.NewReader(stream))
			io.Copy(io.Discard, inspector)
		}
	})
}
