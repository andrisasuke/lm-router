package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/oauth"
	"github.com/andrisasuke/lm-router/internal/store"
)

const (
	DefaultMessagesURL = "https://api.anthropic.com/v1/messages"
	DefaultUsageURL    = "https://api.anthropic.com/api/oauth/usage"
	AnthropicVersion   = "2023-06-01"
	OAuthBeta          = "oauth-2025-04-20"
	ClaudeCodeBeta     = "claude-code-20250219"
	ClaudeRefreshLead  = 4 * time.Hour
)

type Logger interface {
	Printf(format string, args ...any)
}

type Client struct {
	messagesURL string
	usageURL    string
	http        *http.Client
	tokens      *codex.TokenManager
	logger      Logger
}

type ExecuteParams struct {
	Account store.Account
	Body    []byte
	Header  http.Header
}

type ExecuteResult struct {
	Status        int
	Header        http.Header
	Body          []byte
	Retryable     bool
	CooldownUntil time.Time
}

type StreamResult struct {
	Status        int
	Header        http.Header
	Body          io.ReadCloser
	Retryable     bool
	ErrorBody     []byte
	CooldownUntil time.Time
}

type OAuthRefresher struct{}

func (OAuthRefresher) Refresh(ctx context.Context, refreshToken string) (codex.TokenSet, error) {
	resp, err := oauth.RefreshAnthropicToken(ctx, refreshToken)
	if err != nil {
		return codex.TokenSet{}, err
	}
	return codex.TokenSet{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		ExpiresAt:    oauth.ExpiryTime(resp.ExpiresIn),
	}, nil
}

func NewClient(messagesURL, usageURL string, tokens *codex.TokenManager, logger Logger) *Client {
	if strings.TrimSpace(messagesURL) == "" {
		messagesURL = DefaultMessagesURL
	}
	if strings.TrimSpace(usageURL) == "" {
		usageURL = DefaultUsageURL
	}
	return &Client{
		messagesURL: strings.TrimRight(messagesURL, "/"),
		usageURL:    usageURL,
		http:        defaultHTTPClient(300 * time.Second),
		tokens:      tokens,
		logger:      logger,
	}
}

func (c *Client) SetHTTPClient(client *http.Client) {
	if client != nil {
		c.http = client
	}
}

func (c *Client) SetUpstreamTimeout(seconds int) {
	if seconds > 0 {
		c.http = defaultHTTPClient(time.Duration(seconds) * time.Second)
	}
}

func defaultHTTPClient(headerTimeout time.Duration) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		ExpectContinueTimeout: time.Second,
	}}
}

func (c *Client) ExecuteMessages(ctx context.Context, params ExecuteParams) (ExecuteResult, error) {
	return c.execute(ctx, params, c.messagesURL)
}

func (c *Client) ExecuteCountTokens(ctx context.Context, params ExecuteParams) (ExecuteResult, error) {
	endpoint := strings.TrimSuffix(c.messagesURL, "/messages") + "/messages/count_tokens"
	return c.execute(ctx, params, endpoint)
}

func (c *Client) execute(ctx context.Context, params ExecuteParams, endpoint string) (ExecuteResult, error) {
	stream, err := c.open(ctx, params, endpoint, true)
	if err != nil {
		return ExecuteResult{
			Status:        stream.Status,
			Header:        stream.Header,
			Body:          stream.ErrorBody,
			Retryable:     stream.Retryable,
			CooldownUntil: stream.CooldownUntil,
		}, err
	}
	if stream.Status < 200 || stream.Status >= 300 {
		return ExecuteResult{Status: stream.Status, Header: stream.Header, Body: stream.ErrorBody, Retryable: stream.Retryable, CooldownUntil: stream.CooldownUntil}, nil
	}
	defer stream.Body.Close()
	body, err := io.ReadAll(stream.Body)
	if err != nil {
		return ExecuteResult{}, err
	}
	return ExecuteResult{Status: stream.Status, Header: stream.Header, Body: body, Retryable: stream.Retryable, CooldownUntil: stream.CooldownUntil}, nil
}

func (c *Client) OpenMessagesStream(ctx context.Context, params ExecuteParams) (StreamResult, error) {
	return c.open(ctx, params, c.messagesURL, true)
}

func (c *Client) open(ctx context.Context, params ExecuteParams, endpoint string, allowRefresh bool) (StreamResult, error) {
	if c.tokens == nil {
		return StreamResult{}, errors.New("anthropic token manager is not configured")
	}
	account, err := c.tokens.EnsureFresh(ctx, params.Account.ID)
	if err != nil {
		return StreamResult{}, err
	}
	return c.openWithAccount(ctx, account, params.Body, params.Header, endpoint, allowRefresh)
}

func (c *Client) openWithAccount(ctx context.Context, account store.Account, body []byte, inbound http.Header, endpoint string, allowRefresh bool) (StreamResult, error) {
	endpoint = withBeta(endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return StreamResult{}, err
	}
	copyClaudeCodeHeaders(req.Header, inbound)
	if !hasHeader(inbound, "User-Agent") {
		// An explicit empty value suppresses net/http's synthetic
		// "Go-http-client" identity when the original client sent none.
		req.Header["User-Agent"] = []string{""}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+account.AccessToken)
	if req.Header.Get("Anthropic-Version") == "" {
		req.Header.Set("Anthropic-Version", AnthropicVersion)
	}
	req.Header.Set("Anthropic-Beta", mergeBeta(req.Header.Values("Anthropic-Beta"), OAuthBeta, ClaudeCodeBeta))

	started := time.Now()
	c.logf("[anthropic-api] request account=%s method=POST url=%s body_bytes=%d", account.ID, req.URL.String(), len(body))
	resp, err := c.http.Do(req)
	if err != nil {
		c.logf("[anthropic-api] error account=%s duration=%s error=%s", account.ID, time.Since(started), err)
		return StreamResult{Retryable: true}, err
	}
	result := StreamResult{
		Status:    resp.StatusCode,
		Header:    cloneHeader(resp.Header),
		Body:      resp.Body,
		Retryable: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
	}
	if result.Retryable {
		result.CooldownUntil, _ = cooldownUntil(resp.Header, time.Now())
	}
	c.logf("[anthropic-api] response account=%s status=%d duration=%s", account.ID, resp.StatusCode, time.Since(started))

	if allowRefresh && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		errorBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		refreshed, refreshErr := c.tokens.RefreshNow(ctx, account.ID)
		if refreshErr != nil {
			result.Body = nil
			result.ErrorBody = errorBody
			result.Retryable = true
			return result, refreshErr
		}
		return c.openWithAccount(ctx, refreshed, body, inbound, endpoint, false)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return result, nil
	}
	errorBody, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return StreamResult{}, readErr
	}
	result.Body = nil
	result.ErrorBody = errorBody
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		result.Retryable = true
		if !allowRefresh {
			_ = c.tokens.MarkNeedsReauth(ctx, account.ID)
		}
	}
	if result.Retryable && result.CooldownUntil.IsZero() {
		result.CooldownUntil, _ = cooldownUntil(resp.Header, time.Now())
	}
	return result, nil
}

func withBeta(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("beta", "true")
	u.RawQuery = q.Encode()
	return u.String()
}

func hasHeader(header http.Header, wanted string) bool {
	for key := range header {
		if strings.EqualFold(key, wanted) {
			return true
		}
	}
	return false
}

func copyClaudeCodeHeaders(dst, src http.Header) {
	for key, values := range src {
		lower := strings.ToLower(key)
		allowed := strings.HasPrefix(lower, "anthropic-") ||
			lower == "user-agent" || lower == "x-app" ||
			strings.HasPrefix(lower, "x-stainless-") ||
			strings.HasPrefix(lower, "x-claude-code-")
		if !allowed {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func mergeBeta(values []string, required ...string) string {
	seen := map[string]bool{}
	merged := make([]string, 0)
	add := func(raw string) {
		for _, value := range strings.Split(raw, ",") {
			value = strings.TrimSpace(value)
			if value != "" && !seen[value] {
				seen[value] = true
				merged = append(merged, value)
			}
		}
	}
	for _, value := range values {
		add(value)
	}
	for _, value := range required {
		add(value)
	}
	return strings.Join(merged, ",")
}

func cooldownUntil(header http.Header, now time.Time) (time.Time, bool) {
	if raw := strings.TrimSpace(header.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
			return now.Add(time.Duration(seconds) * time.Second), true
		}
		if parsed, err := http.ParseTime(raw); err == nil {
			return parsed, true
		}
	}
	for _, key := range []string{
		"anthropic-ratelimit-unified-reset",
		"anthropic-ratelimit-tokens-reset",
		"anthropic-ratelimit-requests-reset",
		"x-ratelimit-reset",
	} {
		raw := strings.TrimSpace(header.Get(key))
		if raw == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			return parsed, true
		}
		if unix, err := strconv.ParseInt(raw, 10, 64); err == nil && unix > now.Unix() {
			return time.Unix(unix, 0), true
		}
	}
	return time.Time{}, false
}

func cloneHeader(header http.Header) http.Header {
	out := make(http.Header, len(header))
	for key, values := range header {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func (c *Client) logf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Printf(format, args...)
	}
}

// ModelFromBody performs routing only. It intentionally does not validate a
// Claude model against the advertised catalog.
func ModelFromBody(body []byte) (string, error) {
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	model := strings.TrimSpace(payload.Model)
	if model == "" {
		return "", errors.New("model is required")
	}
	return model, nil
}
