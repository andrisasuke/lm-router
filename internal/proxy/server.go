package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
)

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

func writeAnthropicError(w http.ResponseWriter, status int, typ string, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": message,
		},
	})
}

type anthropicMessageRequest struct {
	Model    string `json:"model"`
	System   any    `json:"system,omitempty"`
	Messages []struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream,omitempty"`
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
	input := make([]map[string]any, 0, len(req.Messages))
	for _, msg := range req.Messages {
		role := msg.Role
		if role != "assistant" {
			role = "user"
		}
		input = append(input, map[string]any{
			"role":    role,
			"content": anthropicContentToResponsesContent(msg.Content),
		})
	}

	out := map[string]any{
		"model":  req.Model,
		"input":  input,
		"stream": true,
		"store":  false,
	}
	if instructions != "" {
		out["instructions"] = instructions
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

func anthropicContentToResponsesContent(raw any) []map[string]string {
	switch v := raw.(type) {
	case string:
		return []map[string]string{{"type": "input_text", "text": v}}
	case []any:
		out := make([]map[string]string, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok || m["type"] != "text" {
				continue
			}
			if text, ok := m["text"].(string); ok {
				out = append(out, map[string]string{"type": "input_text", "text": text})
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []map[string]string{{"type": "input_text", "text": ""}}
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
	responsesBody, model, stream, err := anthropicMessagesToResponsesBody(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if stream {
		// streaming implemented in Task 5
		writeAnthropicError(w, http.StatusNotImplemented, "api_error", "streaming not yet implemented")
		return
	}
	respBody, _, status, err := s.routeResponses(r.Context(), responsesBody)
	if err != nil {
		writeAnthropicError(w, statusOrDefault(status, http.StatusBadGateway), "api_error", err.Error())
		return
	}
	text := codex.ConvertResponsesSSEToOutput(respBody)
	writeAnthropicMessageJSON(w, model, text)
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
	// TODO: full implementation in later tasks
	writeAnthropicError(w, http.StatusNotImplemented, "api_error", "not yet implemented")
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
