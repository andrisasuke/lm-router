package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/andrisasuke/lm-router/internal/oauth"
	"github.com/andrisasuke/lm-router/internal/store"
)

const DefaultInstructions = "You are Codex, based on GPT-5. You are running as a coding agent in the Codex CLI on a user's computer."
const logBodyLimit = 64 * 1024

type Client struct {
	baseURL      string
	http         *http.Client
	tokens       *TokenManager
	logger       Logger
	logBodyLimit int
}

type Logger interface {
	Printf(format string, args ...any)
}

type stdLogger struct{}

func (stdLogger) Printf(format string, args ...any) {
	log.Printf(format, args...)
}

type ExecuteParams struct {
	Account store.Account
	Body    []byte
}

type ExecuteResult struct {
	Status    int
	Header    http.Header
	Body      []byte
	Retryable bool
}

type StreamResult struct {
	Status    int
	Header    http.Header
	Body      io.ReadCloser
	Retryable bool
	ErrorBody []byte
}

func NewClient(baseURL string, tokens *TokenManager) *Client {
	return NewClientWithLogger(baseURL, tokens, stdLogger{}, logBodyLimit)
}

func NewClientWithLogger(baseURL string, tokens *TokenManager, logger Logger, bodyLimit int) *Client {
	if logger == nil {
		logger = stdLogger{}
	}
	if bodyLimit <= 0 {
		bodyLimit = logBodyLimit
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   15 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
		tokens:       tokens,
		logger:       logger,
		logBodyLimit: bodyLimit,
	}
}

type OAuthRefresher struct{}

func (OAuthRefresher) Refresh(ctx context.Context, refreshToken string) (TokenSet, error) {
	resp, err := oauth.RefreshToken(ctx, refreshToken)
	if err != nil {
		return TokenSet{}, err
	}
	return TokenSet{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ExpiresAt:    oauth.ExpiryTime(resp.ExpiresIn),
	}, nil
}

func (c *Client) ExecuteResponses(ctx context.Context, params ExecuteParams) (ExecuteResult, error) {
	streamResult, err := c.OpenResponsesStream(ctx, params)
	if err != nil {
		return ExecuteResult{}, err
	}
	if streamResult.Status < 200 || streamResult.Status >= 300 {
		return ExecuteResult{
			Status:    streamResult.Status,
			Header:    streamResult.Header,
			Body:      streamResult.ErrorBody,
			Retryable: streamResult.Retryable,
		}, nil
	}
	defer streamResult.Body.Close()
	respBody, err := io.ReadAll(streamResult.Body)
	if err != nil {
		return ExecuteResult{}, err
	}
	return ExecuteResult{
		Status:    streamResult.Status,
		Header:    streamResult.Header,
		Body:      respBody,
		Retryable: streamResult.Retryable,
	}, nil
}

func (c *Client) OpenResponsesStream(ctx context.Context, params ExecuteParams) (StreamResult, error) {
	account, err := c.tokens.EnsureFresh(ctx, params.Account.ID)
	if err != nil {
		return StreamResult{}, err
	}
	body, err := TransformRequest(params.Body)
	if err != nil {
		return StreamResult{}, err
	}
	return c.openWithAccount(ctx, account, body, true)
}

func (c *Client) openWithAccount(ctx context.Context, account store.Account, reqBody []byte, allowRefreshRetry bool) (StreamResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return StreamResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+account.AccessToken)
	req.Header.Set("originator", "codex-cli")
	req.Header.Set("User-Agent", "codex-cli/1.0.18 (macOS; arm64)")
	req.Header.Set("session_id", account.ID)

	start := time.Now()
	c.logf("[openai-api] request account=%s method=%s url=%s headers=%s body=%s",
		account.ID,
		req.Method,
		req.URL.String(),
		formatHeaders(req.Header),
		formatBody(reqBody, c.logBodyLimit),
	)
	resp, err := c.http.Do(req)
	if err != nil {
		c.logf("[openai-api] error account=%s url=%s duration=%s error=%s", account.ID, c.baseURL, time.Since(start), err)
		return StreamResult{Retryable: true}, err
	}
	c.logf("[openai-api] response-start account=%s status=%d headers=%s duration=%s",
		account.ID,
		resp.StatusCode,
		formatHeaders(resp.Header),
		time.Since(start),
	)

	result := StreamResult{
		Status:    resp.StatusCode,
		Header:    cloneHeader(resp.Header),
		Body:      newLoggingReadCloser(resp.Body, account.ID, resp.StatusCode, start, c.logger, c.logBodyLimit),
		Retryable: resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests,
	}
	if allowRefreshRetry && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && c.tokens != nil {
		errorBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		c.logf("[openai-api] response-body account=%s status=%d duration=%s body=%s", account.ID, resp.StatusCode, time.Since(start), formatBody(errorBody, c.logBodyLimit))
		refreshed, refreshErr := c.tokens.RefreshNow(ctx, account.ID)
		if refreshErr != nil {
			result.ErrorBody = errorBody
			return result, refreshErr
		}
		return c.openWithAccount(ctx, refreshed, reqBody, false)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return result, nil
	}
	errorBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return StreamResult{}, err
	}
	c.logf("[openai-api] response-body account=%s status=%d duration=%s body=%s", account.ID, resp.StatusCode, time.Since(start), formatBody(errorBody, c.logBodyLimit))
	result.Body = nil
	result.ErrorBody = errorBody
	if resp.StatusCode == http.StatusTooManyRequests {
		if until, ok := parseCooldown(errorBody); ok {
			_ = c.tokens.db.SetCooldown(ctx, account.ID, until)
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		result.Retryable = true
	}
	return result, nil
}

func (c *Client) FetchQuota(ctx context.Context, account store.Account) (QuotaInfo, error) {
	refreshed, err := c.tokens.EnsureFresh(ctx, account.ID)
	if err != nil {
		return QuotaInfo{}, err
	}
	body, err := TransformRequest([]byte(`{"model":"gpt-5.3-codex","input":"ping","stream":false}`))
	if err != nil {
		return QuotaInfo{}, err
	}
	result, err := c.openWithAccount(ctx, refreshed, body, true)
	if err != nil {
		return QuotaInfo{}, err
	}
	if result.Body != nil {
		_ = result.Body.Close()
	}
	now := time.Now()
	info := ParseQuotaHeaders(result.Header, now)
	if info.Primary == nil && info.Secondary == nil {
		c.logf("[quota] no x-codex-* headers account=%s headers=%s", refreshed.ID, formatHeaders(result.Header))
	}
	return info, nil
}

func (c *Client) logf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Printf(format, args...)
	}
}

func ConvertResponsesSSEToOutput(sse []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(sse))
	var out strings.Builder
	var eventName string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			eventName = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if typ, _ := event["type"].(string); typ != "" {
			eventName = typ
		}
		if delta, ok := event["delta"].(string); ok {
			out.WriteString(delta)
			continue
		}
		if eventName == "response.output_text.done" {
			if text, ok := event["text"].(string); ok && out.Len() == 0 {
				out.WriteString(text)
			}
		}
	}
	return out.String()
}

func TransformRequest(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if instructions, _ := payload["instructions"].(string); strings.TrimSpace(instructions) == "" {
		payload["instructions"] = DefaultInstructions
	}
	payload["stream"] = true
	payload["store"] = false
	payload["input"] = normalizeInput(payload["input"])
	for _, key := range []string{
		"temperature",
		"top_p",
		"frequency_penalty",
		"presence_penalty",
		"logprobs",
		"top_logprobs",
		"n",
		"seed",
		"max_tokens",
		"max_completion_tokens",
		"max_output_tokens",
		"user",
		"prompt_cache_retention",
		"metadata",
		"stream_options",
		"safety_identifier",
	} {
		delete(payload, key)
	}
	return json.Marshal(payload)
}

func normalizeInput(input any) any {
	switch v := input.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			v = "..."
		}
		return []map[string]any{{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": v,
			}},
		}}
	case []any:
		if len(v) > 0 {
			return v
		}
	}
	return []map[string]any{{
		"type": "message",
		"role": "user",
		"content": []map[string]any{{
			"type": "input_text",
			"text": "...",
		}},
	}}
}

func parseCooldown(body []byte) (time.Time, bool) {
	var payload struct {
		Error struct {
			ResetsAt        int64 `json:"resets_at"`
			ResetsInSeconds int64 `json:"resets_in_seconds"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return time.Time{}, false
	}
	if payload.Error.ResetsAt > 0 {
		return time.Unix(payload.Error.ResetsAt, 0), true
	}
	if payload.Error.ResetsInSeconds > 0 {
		return time.Now().Add(time.Duration(payload.Error.ResetsInSeconds) * time.Second), true
	}
	return time.Time{}, false
}

func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		vv := make([]string, len(v))
		copy(vv, v)
		out[k] = vv
	}
	return out
}

type loggingReadCloser struct {
	body      io.ReadCloser
	accountID string
	status    int
	start     time.Time
	buf       bytes.Buffer
	total     int
	logged    bool
	logger    Logger
	bodyLimit int
}

func newLoggingReadCloser(body io.ReadCloser, accountID string, status int, start time.Time, logger Logger, bodyLimit int) io.ReadCloser {
	if logger == nil {
		logger = stdLogger{}
	}
	if bodyLimit <= 0 {
		bodyLimit = logBodyLimit
	}
	return &loggingReadCloser{body: body, accountID: accountID, status: status, start: start, logger: logger, bodyLimit: bodyLimit}
}

func (r *loggingReadCloser) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.total += n
		if r.buf.Len() < r.bodyLimit {
			remaining := r.bodyLimit - r.buf.Len()
			if n < remaining {
				remaining = n
			}
			_, _ = r.buf.Write(p[:remaining])
		}
	}
	if err != nil {
		r.log()
	}
	return n, err
}

func (r *loggingReadCloser) Close() error {
	r.log()
	return r.body.Close()
}

func (r *loggingReadCloser) log() {
	if r.logged {
		return
	}
	r.logged = true
	suffix := ""
	if r.total > r.buf.Len() {
		suffix = " (truncated)"
	}
	r.logger.Printf("[openai-api] response-body account=%s status=%d bytes=%d duration=%s body=%s%s",
		r.accountID,
		r.status,
		r.total,
		time.Since(r.start),
		formatBody(r.buf.Bytes(), r.bodyLimit),
		suffix,
	)
}

func formatHeaders(headers http.Header) string {
	redacted := make(map[string][]string, len(headers))
	for key, values := range headers {
		if strings.EqualFold(key, "Authorization") {
			redacted[key] = []string{"<redacted>"}
			continue
		}
		copied := make([]string, len(values))
		copy(copied, values)
		redacted[key] = copied
	}
	data, err := json.Marshal(redacted)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func formatBody(body []byte, limit int) string {
	if limit <= 0 {
		limit = logBodyLimit
	}
	if len(body) > limit {
		return string(body[:limit]) + " (truncated)"
	}
	return string(body)
}

func CopyStream(dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, src)
	return err
}

func IsAccountAvailable(account store.Account) bool {
	if !account.Enabled || account.NeedsReauth {
		return false
	}
	if account.CooldownUntil.Valid && account.CooldownUntil.Time.After(time.Now()) {
		return false
	}
	return true
}

func IsCooldownActive(account store.Account) bool {
	return account.CooldownUntil.Valid && account.CooldownUntil.Time.After(time.Now())
}

func IsUnrecoverableAuthError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrNeedsReauth) || oauth.IsUnrecoverableRefreshError(err)
}
