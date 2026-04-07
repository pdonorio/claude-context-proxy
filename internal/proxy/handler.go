package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/pdonorio/claude-context-proxy/internal/config"
)

// Upstream is the default Anthropic API base URL.
const Upstream = "https://api.anthropic.com"

// TokenInfo carries extracted token data from a proxied request/response pair.
type TokenInfo struct {
	Input         int64    // total input (NewInput + CacheRead + CacheCreation)
	Output        int64
	NewInput      int64    // input_tokens from message_start (non-cached)
	CacheRead     int64    // cache_read_input_tokens
	CacheCreation int64    // cache_creation_input_tokens
	Path          string
	Model         string
	Tools         []string // tool names (CTX_INSPECT=1 only)
	SystemLen     int      // byte length of "system" field in request body
	ToolsCount    int      // number of tool definitions
	ToolsLen      int      // total byte length of tools array
	MessagesLen   int      // total byte length of messages array
}

// OnTokensFn is called after each response with extracted token data.
type OnTokensFn func(TokenInfo)

// sseEventData holds the minimal JSON fields needed from SSE events.
type sseEventData struct {
	Type    string `json:"type"`
	Message struct {
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Usage struct {
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content_block"`
}

// SSEInspector wraps an io.Reader and extracts token counts and (optionally) tool
// names from SSE events inline. Token extraction is always active; tool name
// extraction is gated on the inspectTools flag.
type SSEInspector struct {
	r             io.Reader
	buf           []byte
	inspectTools  bool
	Tools         []string
	InputTokens   int64 // total: NewInput + CacheRead + CacheCreation
	OutputTokens  int64
	NewInput      int64 // raw input_tokens (non-cached)
	CacheRead     int64
	CacheCreation int64
}

// NewSSEInspector returns a new SSEInspector wrapping r.
// If inspectTools is true, tool names are also extracted (slightly more work).
func NewSSEInspector(r io.Reader, inspectTools bool) *SSEInspector {
	return &SSEInspector{r: r, inspectTools: inspectTools}
}

func (s *SSEInspector) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.ingest(p[:n])
	}
	return n, err
}

func (s *SSEInspector) ingest(chunk []byte) {
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

func (s *SSEInspector) parseEvent(raw []byte) {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		var ev sseEventData
		if json.Unmarshal(line[6:], &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			s.NewInput = ev.Message.Usage.InputTokens
			s.CacheRead = ev.Message.Usage.CacheReadInputTokens
			s.CacheCreation = ev.Message.Usage.CacheCreationInputTokens
			s.InputTokens = s.NewInput + s.CacheRead + s.CacheCreation
			s.OutputTokens = ev.Message.Usage.OutputTokens
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				s.OutputTokens = ev.Usage.OutputTokens
			}
		case "content_block_start":
			if s.inspectTools && ev.ContentBlock.Type == "tool_use" && ev.ContentBlock.Name != "" {
				s.Tools = append(s.Tools, ev.ContentBlock.Name)
			}
		}
	}
}

// parseReqBody extracts byte lengths of the system, tools, and messages sections
// from a JSON request body. Returns zero values if the body cannot be parsed.
func parseReqBody(body []byte) (sysLen, toolsLen, msgsLen, toolsCount int) {
	var req struct {
		System   json.RawMessage   `json:"system"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return
	}
	sysLen = len(req.System)
	toolsCount = len(req.Tools)
	for _, t := range req.Tools {
		toolsLen += len(t)
	}
	for _, m := range req.Messages {
		msgsLen += len(m)
	}
	return
}

// Handler returns an http.HandlerFunc that reverse-proxies to targetURL.
// After each response, onTokens is called with the extracted token data.
func Handler(targetURL string, cfg *config.Config, onTokens OnTokensFn) http.HandlerFunc {
	client := &http.Client{Timeout: 0} // no timeout — streaming responses can be long
	return func(w http.ResponseWriter, r *http.Request) {
		target := targetURL + r.RequestURI

		// Buffer the request body to extract the model and section sizes, then replay.
		var bodyBuf []byte
		if r.Body != nil {
			var err error
			bodyBuf, err = io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		model := extractModel(cfg, bodyBuf)
		sysLen, toolsLen, msgsLen, toolsCount := parseReqBody(bodyBuf)

		proxyReq, err := http.NewRequest(r.Method, target, io.NopCloser(bytes.NewReader(bodyBuf)))
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

		resp, err := client.Do(proxyReq)
		if err != nil {
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Extract token counts from response headers (fallback for non-SSE / API key users).
		headerInput, _ := strconv.ParseInt(resp.Header.Get("X-Anthropic-Input-Tokens"), 10, 64)
		headerOutput, _ := strconv.ParseInt(resp.Header.Get("X-Anthropic-Output-Tokens"), 10, 64)

		// Copy response headers then status.
		for k, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)

		// Stream body; always run SSEInspector for SSE to extract token counts.
		// Tool name extraction is additionally gated on cfg.Inspect.
		isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
		var inspector *SSEInspector
		if isSSE {
			inspector = NewSSEInspector(resp.Body, cfg.Inspect)
			flusher, ok := w.(http.Flusher)
			buf := make([]byte, 4096)
			for {
				n, readErr := inspector.Read(buf)
				if n > 0 {
					if _, werr := w.Write(buf[:n]); werr != nil {
						// Client disconnected — stop streaming.
						break
					}
					if ok {
						flusher.Flush()
					}
				}
				if readErr == io.EOF || readErr != nil {
					break
				}
			}
		} else {
			// Non-streaming: copy body; ignore client-disconnect errors.
			_, _ = io.Copy(w, resp.Body)
		}

		// Build TokenInfo, preferring SSE-parsed totals (include cache tokens) over
		// header counts — headers only report the tiny raw input_tokens value.
		ti := TokenInfo{
			Path:        r.URL.Path,
			Model:       model,
			SystemLen:   sysLen,
			ToolsCount:  toolsCount,
			ToolsLen:    toolsLen,
			MessagesLen: msgsLen,
		}
		if inspector != nil && (inspector.InputTokens > 0 || inspector.OutputTokens > 0) {
			ti.Input = inspector.InputTokens
			ti.Output = inspector.OutputTokens
			ti.NewInput = inspector.NewInput
			ti.CacheRead = inspector.CacheRead
			ti.CacheCreation = inspector.CacheCreation
			ti.Tools = inspector.Tools
		} else {
			ti.Input = headerInput
			ti.Output = headerOutput
		}

		if ti.Input > 0 || ti.Output > 0 {
			onTokens(ti)
		}
	}
}

// extractModel extracts the "model" field from a JSON request body.
func extractModel(cfg *config.Config, body []byte) string {
	var data map[string]interface{}
	if json.Unmarshal(body, &data) != nil {
		return cfg.DefaultModel
	}
	if model, ok := data["model"].(string); ok && model != "" {
		return model
	}
	return cfg.DefaultModel
}
