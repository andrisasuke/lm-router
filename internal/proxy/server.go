package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

func newThinkingSignature() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

type ServerConfig struct {
	Store       *store.DB
	Codex       *codex.Client
	RequireKey  bool
	Logger      Logger
	LogRequests bool
}

type Logger interface {
	Printf(format string, args ...any)
}

type stdLogger struct{}

func (stdLogger) Printf(format string, args ...any) {
	log.Printf(format, args...)
}

type Server struct {
	store      *store.DB
	codex      *codex.Client
	requireKey bool
}

func New(cfg ServerConfig) http.Handler {
	s := &Server{
		store:      cfg.Store,
		codex:      cfg.Codex,
		requireKey: cfg.RequireKey,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/v1/models", s.models)
	mux.HandleFunc("/v1/responses", s.responses)
	mux.HandleFunc("/v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("/v1/messages", s.messages)
	mux.HandleFunc("/v1/v1/messages", s.messages)
	mux.HandleFunc("/v1/messages/count_tokens", s.countAnthropicTokens)
	mux.HandleFunc("/v1/v1/messages/count_tokens", s.countAnthropicTokens)
	logger := cfg.Logger
	if logger == nil {
		logger = stdLogger{}
	}
	if !cfg.LogRequests {
		return mux
	}
	return logRequests(mux, logger)
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(body)
	w.bytes += n
	return n, err
}

func (w *loggingResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func logRequests(next http.Handler, logger Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w}
		logger.Printf("[request] start method=%s path=%s remote=%s", r.Method, r.URL.RequestURI(), r.RemoteAddr)
		next.ServeHTTP(lrw, r)
		status := lrw.status
		if status == 0 {
			status = http.StatusOK
		}
		logger.Printf("[request] done method=%s path=%s status=%d bytes=%d duration=%s", r.Method, r.URL.RequestURI(), status, lrw.bytes, time.Since(start))
	})
}

func (s *Server) authOK(r *http.Request) bool {
	if !s.requireKey {
		return true
	}
	ctx := r.Context()
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		key := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if key != "" && s.store.ValidateAPIKey(ctx, key) {
			return true
		}
	}
	if k := strings.TrimSpace(r.Header.Get("x-api-key")); k != "" {
		return s.store.ValidateAPIKey(ctx, k)
	}
	return false
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": "gpt-5.3-codex", "object": "model", "owned_by": "openai"},
			{"id": "gpt-5.3-codex-high", "object": "model", "owned_by": "openai"},
			{"id": "gpt-5.2-codex", "object": "model", "owned_by": "openai"},
		},
	})
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if stream, _ := reqBody["stream"].(bool); stream {
		contentType, status, err := s.streamResponses(r.Context(), body, w)
		if err != nil {
			writeOpenAIError(w, statusOrDefault(status, http.StatusBadGateway), "proxy_error", err.Error())
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_ = contentType
		return
	}
	respBody, contentType, status, err := s.routeResponses(r.Context(), body)
	if err != nil {
		writeOpenAIError(w, statusOrDefault(status, http.StatusBadGateway), "proxy_error", err.Error())
		return
	}
	if strings.Contains(contentType, "text/event-stream") {
		writeJSON(w, http.StatusOK, responsesJSONFromSSE(reqBody["model"], respBody))
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	responsesBody := map[string]any{
		"model":  body["model"],
		"input":  messagesToInput(body["messages"]),
		"stream": false,
	}
	if stream, ok := body["stream"].(bool); ok {
		responsesBody["stream"] = stream
	}
	payload, _ := json.Marshal(responsesBody)
	if stream, _ := responsesBody["stream"].(bool); stream {
		status, err := s.streamChatCompletions(r.Context(), payload, w)
		if err != nil {
			writeOpenAIError(w, statusOrDefault(status, http.StatusBadGateway), "proxy_error", err.Error())
		}
		return
	}
	respBody, _, status, err := s.routeResponses(r.Context(), payload)
	if err != nil {
		writeOpenAIError(w, statusOrDefault(status, http.StatusBadGateway), "proxy_error", err.Error())
		return
	}
	text := codex.ConvertResponsesSSEToOutput(respBody)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-lm-router",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   body["model"],
		"choices": []map[string]any{
			{"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop"},
		},
	})
}

func (s *Server) routeResponses(ctx context.Context, body []byte) ([]byte, string, int, error) {
	accounts, err := s.store.ListRoutableAccounts(ctx, "openai-codex")
	if err != nil {
		return nil, "application/json", http.StatusInternalServerError, err
	}
	if len(accounts) == 0 {
		return nil, "application/json", http.StatusBadGateway, errors.New("no routable accounts")
	}
	var lastErr error
	var lastStatus int
	for _, account := range accounts {
		if !codex.IsAccountAvailable(account) {
			continue
		}
		result, err := s.codex.ExecuteResponses(ctx, codex.ExecuteParams{
			Account: account,
			Body:    body,
		})
		if err != nil {
			lastErr = err
			lastStatus = http.StatusBadGateway
			continue
		}
		if result.Status >= 200 && result.Status < 300 {
			return result.Body, contentTypeOrDefault(result.Header.Get("Content-Type")), result.Status, nil
		}
		lastStatus = result.Status
		lastErr = errors.New(string(result.Body))
		if !result.Retryable {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("all accounts unavailable")
	}
	return nil, "application/json", lastStatus, lastErr
}

func (s *Server) streamResponses(ctx context.Context, body []byte, w http.ResponseWriter) (string, int, error) {
	stream, status, err := s.openResponseStream(ctx, body)
	if err != nil {
		return "application/json", status, err
	}
	defer stream.Body.Close()
	w.Header().Set("Content-Type", contentTypeOrDefault(stream.Header.Get("Content-Type")))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(stream.Status)
	if err := codex.CopyStream(w, stream.Body); err != nil {
		return "", http.StatusBadGateway, err
	}
	return contentTypeOrDefault(stream.Header.Get("Content-Type")), stream.Status, nil
}

func (s *Server) streamChatCompletions(ctx context.Context, body []byte, w http.ResponseWriter) (int, error) {
	stream, status, err := s.openResponseStream(ctx, body)
	if err != nil {
		return status, err
	}
	defer stream.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if err := convertResponsesStreamToChatSSE(w, stream.Body); err != nil {
		return http.StatusBadGateway, err
	}
	return http.StatusOK, nil
}

func (s *Server) openResponseStream(ctx context.Context, body []byte) (codex.StreamResult, int, error) {
	accounts, err := s.store.ListRoutableAccounts(ctx, "openai-codex")
	if err != nil {
		return codex.StreamResult{}, http.StatusInternalServerError, err
	}
	if len(accounts) == 0 {
		return codex.StreamResult{}, http.StatusBadGateway, errors.New("no routable accounts")
	}
	var lastErr error
	var lastStatus int
	for _, account := range accounts {
		if !codex.IsAccountAvailable(account) {
			continue
		}
		result, err := s.codex.OpenResponsesStream(ctx, codex.ExecuteParams{
			Account: account,
			Body:    body,
		})
		if err != nil {
			lastErr = err
			lastStatus = http.StatusBadGateway
			continue
		}
		if result.Status >= 200 && result.Status < 300 {
			return result, result.Status, nil
		}
		lastStatus = result.Status
		lastErr = errors.New(string(result.ErrorBody))
		if !result.Retryable {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("all accounts unavailable")
	}
	return codex.StreamResult{}, lastStatus, lastErr
}

func messagesToInput(raw any) []map[string]any {
	items, ok := raw.([]any)
	if !ok {
		return []map[string]any{{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "..."}}}}
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "" {
			role = "user"
		}
		text := extractText(msg["content"])
		contentType := "input_text"
		if role == "assistant" {
			contentType = "output_text"
		}
		out = append(out, map[string]any{
			"type": "message",
			"role": role,
			"content": []map[string]any{{
				"type": contentType,
				"text": text,
			}},
		})
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "..."}}})
	}
	return out
}

func extractText(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, _ := m["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	default:
		return ""
	}
}

func chatSSEFromResponsesSSE(body []byte) []byte {
	text := codex.ConvertResponsesSSEToOutput(body)
	chunk := map[string]any{
		"id":      "chatcmpl-lm-router",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "gpt-5.3-codex",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}},
	}
	payload, _ := json.Marshal(chunk)
	doneChunk := map[string]any{
		"id":      "chatcmpl-lm-router",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "gpt-5.3-codex",
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	}
	donePayload, _ := json.Marshal(doneChunk)
	return []byte("data: " + string(payload) + "\n\n" + "data: " + string(donePayload) + "\n\n" + "data: [DONE]\n\n")
}

func responsesJSONFromSSE(model any, body []byte) map[string]any {
	text := codex.ConvertResponsesSSEToOutput(body)
	return map[string]any{
		"id":         "resp_lm_router",
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      model,
		"output": []map[string]any{
			{
				"id":   "msg_lm_router",
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{
						"type": "output_text",
						"text": text,
					},
				},
			},
		},
		"output_text": text,
	}
}

func convertResponsesStreamToChatSSE(w io.Writer, r io.Reader) error {
	encoder := func(payload any) []byte {
		data, _ := json.Marshal(payload)
		return data
	}
	writeFinal := func() error {
		doneChunk := map[string]any{
			"id":      "chatcmpl-lm-router",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "gpt-5.3-codex",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		}
		_, err := w.Write([]byte("data: " + string(encoder(doneChunk)) + "\n\n" + "data: [DONE]\n\n"))
		return err
	}
	scanner := NewSSEScanner(r)
	for scanner.Scan() {
		payload := scanner.Payload()
		if payload == "[DONE]" {
			return writeFinal()
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		delta, _ := event["delta"].(string)
		if delta == "" {
			continue
		}
		chunk := map[string]any{
			"id":      "chatcmpl-lm-router",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "gpt-5.3-codex",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": delta}, "finish_reason": nil}},
		}
		if _, err := w.Write([]byte("data: " + string(encoder(chunk)) + "\n\n")); err != nil {
			return err
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return writeFinal()
}

type SSEScanner struct {
	scanner *bufio.Scanner
	payload string
}

func NewSSEScanner(r io.Reader) *SSEScanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &SSEScanner{scanner: scanner}
}

func (s *SSEScanner) Scan() bool {
	for s.scanner.Scan() {
		line := s.scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		s.payload = strings.TrimPrefix(line, "data: ")
		return true
	}
	return false
}

func (s *SSEScanner) Payload() string {
	return s.payload
}

func (s *SSEScanner) Err() error {
	return s.scanner.Err()
}

func writeAnthropicSSE(w io.Writer, event string, payload map[string]any) {
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("writeAnthropicSSE: marshal %s: %v", event, err)
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

type anthropicStreamState struct {
	flusher           http.Flusher
	nextBlockIndex    int
	thinkingOpen      bool
	thinkingIndex     int
	thinkingSignature string
	textOpen          bool
	textIndex         int
	toolOpen          bool
	toolIndex         int
	toolItemID        string
	sawToolCall       bool
	inputTokens       int
	outputTokens      int
	cacheRead         int
	cacheCreate       int
	stopReason        string
}

func closeThinkingBlock(w io.Writer, state *anthropicStreamState, flusher http.Flusher) {
	if !state.thinkingOpen {
		return
	}
	if state.thinkingSignature != "" {
		writeAnthropicSSE(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": state.thinkingIndex,
			"delta": map[string]string{"type": "signature_delta", "signature": state.thinkingSignature},
		})
	}
	writeAnthropicSSE(w, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": state.thinkingIndex,
	})
	state.thinkingOpen = false
	state.thinkingSignature = ""
	if flusher != nil {
		flusher.Flush()
	}
}

func convertResponsesStreamToAnthropicSSE(w io.Writer, body io.Reader, flusher http.Flusher) {
	state := &anthropicStreamState{
		flusher:    flusher,
		stopReason: "end_turn",
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		switch event["type"] {
		case "response.reasoning_summary_text.delta",
			"response.reasoning_text.delta",
			"response.reasoning.delta",
			"response.reasoning_summary_part.delta",
			"response.reasoning_part.delta":
			delta, _ := event["delta"].(string)
			if delta == "" {
				continue
			}
			if state.textOpen {
				writeAnthropicSSE(w, "content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": state.textIndex,
				})
				state.textOpen = false
			}
			if !state.thinkingOpen {
				state.thinkingIndex = state.nextBlockIndex
				state.nextBlockIndex++
				writeAnthropicSSE(w, "content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         state.thinkingIndex,
					"content_block": map[string]string{"type": "thinking", "thinking": ""},
				})
				state.thinkingOpen = true
				state.thinkingSignature = newThinkingSignature()
			}
			writeAnthropicSSE(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": state.thinkingIndex,
				"delta": map[string]string{"type": "thinking_delta", "thinking": delta},
			})
			if flusher != nil {
				flusher.Flush()
			}
		case "response.output_text.delta":
			delta, _ := event["delta"].(string)
			if delta == "" {
				continue
			}
			closeThinkingBlock(w, state, flusher)
			if !state.textOpen {
				state.textIndex = state.nextBlockIndex
				state.nextBlockIndex++
				writeAnthropicSSE(w, "content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         state.textIndex,
					"content_block": map[string]string{"type": "text", "text": ""},
				})
				state.textOpen = true
			}
			writeAnthropicSSE(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": state.textIndex,
				"delta": map[string]string{"type": "text_delta", "text": delta},
			})
			if flusher != nil {
				flusher.Flush()
			}
		case "response.output_item.added":
			item, _ := event["item"].(map[string]any)
			if item == nil || item["type"] != "function_call" {
				continue
			}
			closeThinkingBlock(w, state, flusher)
			if state.textOpen {
				writeAnthropicSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": state.textIndex})
				state.textOpen = false
			}
			state.toolIndex = state.nextBlockIndex
			state.nextBlockIndex++
			state.toolOpen = true
			state.sawToolCall = true
			callID := stringVal(item["call_id"])
			if callID == "" {
				callID = stringVal(item["id"])
			}
			state.toolItemID = stringVal(item["id"])
			writeAnthropicSSE(w, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": state.toolIndex,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    callID,
					"name":  stringVal(item["name"]),
					"input": map[string]any{},
				},
			})
			if flusher != nil {
				flusher.Flush()
			}
		case "response.function_call_arguments.delta":
			if !state.toolOpen {
				continue
			}
			delta, _ := event["delta"].(string)
			if delta == "" {
				continue
			}
			writeAnthropicSSE(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": state.toolIndex,
				"delta": map[string]string{"type": "input_json_delta", "partial_json": delta},
			})
			if flusher != nil {
				flusher.Flush()
			}
		case "response.output_item.done":
			item, _ := event["item"].(map[string]any)
			if item == nil || item["type"] != "function_call" {
				continue
			}
			if !state.toolOpen {
				continue
			}
			writeAnthropicSSE(w, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": state.toolIndex,
			})
			state.toolOpen = false
			if flusher != nil {
				flusher.Flush()
			}
		case "response.completed":
			resp, _ := event["response"].(map[string]any)
			if resp == nil {
				continue
			}
			usage, _ := resp["usage"].(map[string]any)
			if usage == nil {
				continue
			}
			toInt := func(v any) int {
				switch n := v.(type) {
				case float64:
					return int(n)
				case int:
					return n
				}
				return 0
			}
			state.inputTokens = toInt(usage["input_tokens"])
			state.outputTokens = toInt(usage["output_tokens"])
			state.cacheRead = toInt(usage["cache_read_input_tokens"])
			state.cacheCreate = toInt(usage["cache_creation_input_tokens"])
			// Codex response.status ("completed", "failed", …) is a lifecycle string,
			// not an Anthropic stop_reason. Keep the "end_turn" default unless tool calls seen.
			if state.sawToolCall {
				state.stopReason = "tool_use"
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("convertResponsesStreamToAnthropicSSE: scanner error: %v", err)
		return
	}
	if state.toolOpen {
		writeAnthropicSSE(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": state.toolIndex,
		})
	}
	closeThinkingBlock(w, state, flusher)
	if state.textOpen {
		writeAnthropicSSE(w, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": state.textIndex,
		})
	}
	usageOut := map[string]int{
		"input_tokens":                state.inputTokens,
		"output_tokens":               state.outputTokens,
		"cache_read_input_tokens":     state.cacheRead,
		"cache_creation_input_tokens": state.cacheCreate,
	}
	writeAnthropicSSE(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": state.stopReason, "stop_sequence": nil},
		"usage": usageOut,
	})
	writeAnthropicSSE(w, "message_stop", map[string]any{"type": "message_stop"})
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) streamAnthropicMessages(w http.ResponseWriter, r *http.Request, body []byte, model string) {
	result, status, err := s.openResponseStream(r.Context(), body)
	if err != nil {
		writeAnthropicError(w, statusOrDefault(status, http.StatusBadGateway), "api_error", err.Error())
		return
	}
	defer result.Body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	writeAnthropicSSE(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_lm_router", "type": "message", "role": "assistant",
			"model": model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	if flusher != nil {
		flusher.Flush()
	}

	convertResponsesStreamToAnthropicSSE(w, result.Body, flusher)
}

func writeAnthropicError(w http.ResponseWriter, status int, typ string, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": message,
		},
	})
}

type anthropicThinkingConfig struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicMessageRequest struct {
	Model      string                   `json:"model"`
	System     any                      `json:"system,omitempty"`
	Messages   []anthropicMessage       `json:"messages"`
	Stream     bool                     `json:"stream,omitempty"`
	Thinking   *anthropicThinkingConfig `json:"thinking,omitempty"`
	Tools      []anthropicToolDef       `json:"tools,omitempty"`
	ToolChoice any                      `json:"tool_choice,omitempty"`
}

func budgetToEffort(budget int) string {
	switch {
	case budget <= 0:
		return "medium"
	case budget <= 4096:
		return "low"
	case budget <= 8192:
		return "medium"
	default:
		return "high"
	}
}

func anthropicMessagesToResponsesBody(body []byte) ([]byte, string, bool, error) {
	var req anthropicMessageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", false, err
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, "", false, errors.New("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, "", false, errors.New("messages are required")
	}

	instructions := anthropicSystemText(req.System)
	input := anthropicMessagesToResponsesInput(req.Messages)

	// Always request streaming from Codex — it is the only supported mode.
	// The Anthropic stream flag controls whether we forward SSE or wrap as JSON.
	out := map[string]any{
		"model":  req.Model,
		"input":  input,
		"stream": true,
		"store":  false,
	}
	if instructions != "" {
		out["instructions"] = instructions
	}
	if req.Thinking != nil && (req.Thinking.Type == "enabled" || req.Thinking.Type == "adaptive") {
		out["reasoning"] = map[string]any{
			"effort":  budgetToEffort(req.Thinking.BudgetTokens),
			"summary": "auto",
		}
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool := map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
			}
			if t.InputSchema != nil {
				tool["parameters"] = t.InputSchema
			} else {
				tool["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, tool)
		}
		out["tools"] = tools
	}
	if tc := translateToolChoice(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, "", false, err
	}
	return encoded, req.Model, req.Stream, nil
}

func anthropicSystemText(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok || m["type"] != "text" {
				continue
			}
			if text, ok := m["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

func translateToolChoice(tc any) any {
	if tc == nil {
		return nil
	}
	m, ok := tc.(map[string]any)
	if !ok {
		return nil
	}
	switch m["type"] {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if name, ok := m["name"].(string); ok {
			return map[string]any{"type": "function", "name": name}
		}
	}
	return nil
}

func stringVal(v any) string {
	s, _ := v.(string)
	return s
}

func toolResultOutput(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		allText := true
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok || m["type"] != "text" {
				allText = false
				break
			}
			if text, ok := m["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		if allText && len(parts) > 0 {
			return strings.Join(parts, "")
		}
		b, _ := json.Marshal(content)
		return string(b)
	}
	if content == nil {
		return ""
	}
	b, _ := json.Marshal(content)
	return string(b)
}

func normalizeAnthropicContent(raw any) []map[string]any {
	switch v := raw.(type) {
	case string:
		return []map[string]any{{"type": "text", "text": v}}
	case []any:
		blocks := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				blocks = append(blocks, m)
			}
		}
		return blocks
	}
	return nil
}

func anthropicMessagesToResponsesInput(msgs []anthropicMessage) []map[string]any {
	items := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		blocks := normalizeAnthropicContent(m.Content)
		textParts := make([]map[string]any, 0)

		flushText := func() {
			if len(textParts) == 0 {
				return
			}
			role := m.Role
			if role != "assistant" {
				role = "user"
			}
			items = append(items, map[string]any{
				"type":    "message",
				"role":    role,
				"content": textParts,
			})
			textParts = nil
		}

		for _, b := range blocks {
			switch b["type"] {
			case "thinking", "redacted_thinking":
				// Codex Responses API does not accept thinking blocks.
				// Drop them so the rest of the message structure is preserved.
				continue
			case "text":
				contentType := "input_text"
				if m.Role == "assistant" {
					contentType = "output_text"
				}
				textParts = append(textParts, map[string]any{
					"type": contentType,
					"text": stringVal(b["text"]),
				})
			case "tool_use":
				flushText()
				var argsStr string
				if inp := b["input"]; inp != nil {
					if bs, err := json.Marshal(inp); err == nil {
						argsStr = string(bs)
					}
				}
				if argsStr == "" {
					argsStr = "{}"
				}
				items = append(items, map[string]any{
					"type":      "function_call",
					"call_id":   stringVal(b["id"]),
					"name":      stringVal(b["name"]),
					"arguments": argsStr,
				})
			case "tool_result":
				flushText()
				items = append(items, map[string]any{
					"type":    "function_call_output",
					"call_id": stringVal(b["tool_use_id"]),
					"output":  toolResultOutput(b["content"]),
				})
			}
		}
		flushText()
	}
	return items
}

func writeAnthropicMessageJSON(w http.ResponseWriter, model string, text string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            "msg_lm_router",
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []map[string]string{{"type": "text", "text": text}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	})
}

type anthropicNonStreamResult struct {
	Content      []map[string]any
	StopReason   string
	InputTokens  int
	OutputTokens int
}

func convertResponsesSSEToAnthropicBlocks(sse []byte) anthropicNonStreamResult {
	scanner := bufio.NewScanner(bytes.NewReader(sse))

	result := anthropicNonStreamResult{
		StopReason: "end_turn",
	}

	var thinkingBuf strings.Builder
	var textBuf strings.Builder
	type pendingTool struct {
		callID string
		name   string
		args   strings.Builder
	}
	var toolOrder []string
	tools := map[string]*pendingTool{}

	toInt := func(v any) int {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		}
		return 0
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		switch event["type"] {
		case "response.reasoning_summary_text.delta",
			"response.reasoning_text.delta",
			"response.reasoning.delta",
			"response.reasoning_summary_part.delta",
			"response.reasoning_part.delta":
			delta, _ := event["delta"].(string)
			thinkingBuf.WriteString(delta)
		case "response.output_text.delta":
			delta, _ := event["delta"].(string)
			textBuf.WriteString(delta)
		case "response.output_item.added":
			item, _ := event["item"].(map[string]any)
			if item == nil || item["type"] != "function_call" {
				continue
			}
			callID := stringVal(item["call_id"])
			if callID == "" {
				callID = stringVal(item["id"])
			}
			p := &pendingTool{callID: callID, name: stringVal(item["name"])}
			tools[stringVal(item["id"])] = p
			toolOrder = append(toolOrder, stringVal(item["id"]))
		case "response.function_call_arguments.delta":
			itemID := stringVal(event["item_id"])
			if p, ok := tools[itemID]; ok {
				delta, _ := event["delta"].(string)
				p.args.WriteString(delta)
			}
		case "response.output_item.done":
			item, _ := event["item"].(map[string]any)
			if item == nil || item["type"] != "function_call" {
				continue
			}
			itemID := stringVal(item["id"])
			if p, ok := tools[itemID]; ok && p.args.Len() == 0 {
				if args, ok2 := item["arguments"].(string); ok2 {
					p.args.WriteString(args)
				}
			}
		case "response.completed":
			resp, _ := event["response"].(map[string]any)
			if resp != nil {
				if usage, _ := resp["usage"].(map[string]any); usage != nil {
					result.InputTokens = toInt(usage["input_tokens"])
					result.OutputTokens = toInt(usage["output_tokens"])
				}
			}
		}
	}

	if thinkingBuf.Len() > 0 {
		result.Content = append(result.Content, map[string]any{
			"type":      "thinking",
			"thinking":  thinkingBuf.String(),
			"signature": newThinkingSignature(),
		})
	}
	if textBuf.Len() > 0 {
		result.Content = append(result.Content, map[string]any{
			"type": "text",
			"text": textBuf.String(),
		})
	}
	for _, itemID := range toolOrder {
		p := tools[itemID]
		var inputObj map[string]any
		if p.args.Len() > 0 {
			_ = json.Unmarshal([]byte(p.args.String()), &inputObj)
		}
		if inputObj == nil {
			inputObj = map[string]any{}
		}
		result.Content = append(result.Content, map[string]any{
			"type":  "tool_use",
			"id":    p.callID,
			"name":  p.name,
			"input": inputObj,
		})
	}
	if len(toolOrder) > 0 {
		result.StopReason = "tool_use"
	}

	return result
}

func writeAnthropicMessageFull(w http.ResponseWriter, model string, r anthropicNonStreamResult) {
	content := r.Content
	if len(content) == 0 {
		content = []map[string]any{{"type": "text", "text": ""}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            "msg_lm_router",
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   r.StopReason,
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  r.InputTokens,
			"output_tokens": r.OutputTokens,
		},
	})
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	log.Printf("[anthropic-api] inbound /v1/messages %s", summarizeInboundBody(body))
	responsesBody, model, stream, err := anthropicMessagesToResponsesBody(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if stream {
		s.streamAnthropicMessages(w, r, responsesBody, model)
		return
	}
	respBody, _, status, err := s.routeResponses(r.Context(), responsesBody)
	if err != nil {
		writeAnthropicError(w, statusOrDefault(status, http.StatusBadGateway), "api_error", err.Error())
		return
	}
	blocks := convertResponsesSSEToAnthropicBlocks(respBody)
	writeAnthropicMessageFull(w, model, blocks)
}

func (s *Server) countAnthropicTokens(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	tokens := estimateAnthropicTokens(body)
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": tokens})
}

func estimateAnthropicTokens(body []byte) int {
	// Approximates token count at ~4 chars per token — not tokenizer-exact.
	n := len(body) / 4
	if n < 1 {
		return 1
	}
	return n
}

func summarizeInboundBody(body []byte) string {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Sprintf("size=%d parse_error=%v", len(body), err)
	}
	model, _ := raw["model"].(string)
	stream, _ := raw["stream"].(bool)
	msgs, _ := raw["messages"].([]any)
	tools, _ := raw["tools"].([]any)
	thinking := "none"
	if t, ok := raw["thinking"].(map[string]any); ok {
		typ, _ := t["type"].(string)
		budget := 0
		switch n := t["budget_tokens"].(type) {
		case float64:
			budget = int(n)
		case int:
			budget = n
		}
		thinking = fmt.Sprintf("type=%s,budget=%d", typ, budget)
	}
	return fmt.Sprintf("size=%d model=%q stream=%t messages=%d tools=%d thinking=%s",
		len(body), model, stream, len(msgs), len(tools), thinking)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeOpenAIError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"type":    typ,
			"message": msg,
		},
	})
}

func contentTypeOrDefault(v string) string {
	if v == "" {
		return "text/event-stream"
	}
	return v
}

func statusOrDefault(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}
