// Package customprovider forwards requests to user-configured
// OpenAI-compatible or Anthropic-compatible HTTP endpoints. Unlike
// internal/codex and internal/anthropic, it holds no OAuth/refresh state and
// performs no request/response translation: it is a thin, per-account
// passthrough client (base URL and API key come from the store.Account
// passed into each call, not from process-global configuration).
package customprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/andrisasuke/lm-router/internal/store"
)

type Logger interface {
	Printf(format string, args ...any)
}

type Client struct {
	http   *http.Client
	logger Logger
}

// ExecuteParams describes one upstream request. Path is the endpoint suffix
// appended to the account's BaseURL ("/chat/completions", "/responses", or
// "/messages"). Method defaults to POST; the connection-test probe uses GET
// with an empty Body. Model, when non-empty, replaces the "model" field of
// Body before it is forwarded (see rewriteModelField) — callers pass the
// prefix already stripped.
type ExecuteParams struct {
	Account store.Account
	Path    string
	Method  string
	Model   string
	Body    []byte
}

type ExecuteResult struct {
	Status int
	Header http.Header
	Body   []byte
}

type StreamResult struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

func NewClient(logger Logger) *Client {
	return &Client{http: defaultHTTPClient(300 * time.Second), logger: logger}
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

func (c *Client) Execute(ctx context.Context, params ExecuteParams) (ExecuteResult, error) {
	stream, err := c.open(ctx, params)
	if err != nil {
		return ExecuteResult{}, err
	}
	defer stream.Body.Close()
	body, err := io.ReadAll(stream.Body)
	if err != nil {
		return ExecuteResult{}, err
	}
	return ExecuteResult{Status: stream.Status, Header: stream.Header, Body: body}, nil
}

func (c *Client) OpenStream(ctx context.Context, params ExecuteParams) (StreamResult, error) {
	return c.open(ctx, params)
}

func (c *Client) open(ctx context.Context, params ExecuteParams) (StreamResult, error) {
	method := params.Method
	if method == "" {
		method = http.MethodPost
	}
	endpoint := strings.TrimRight(params.Account.BaseURL, "/") + params.Path

	body := params.Body
	if len(body) > 0 && params.Model != "" {
		rewritten, err := rewriteModelField(body, params.Model)
		if err != nil {
			return StreamResult{}, fmt.Errorf("rewrite model field: %w", err)
		}
		body = rewritten
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return StreamResult{}, err
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch params.Account.CompatType {
	case store.CompatAnthropicStyle:
		req.Header.Set("x-api-key", params.Account.AccessToken)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		req.Header.Set("Authorization", "Bearer "+params.Account.AccessToken)
	}

	started := time.Now()
	c.logf("[custom-provider] request account=%s method=%s url=%s body_bytes=%d", params.Account.ID, method, req.URL.String(), len(body))
	resp, err := c.http.Do(req)
	if err != nil {
		c.logf("[custom-provider] error account=%s duration=%s error=%s", params.Account.ID, time.Since(started), err)
		return StreamResult{}, err
	}
	c.logf("[custom-provider] response account=%s status=%d duration=%s", params.Account.ID, resp.StatusCode, time.Since(started))
	return StreamResult{Status: resp.StatusCode, Header: cloneHeader(resp.Header), Body: resp.Body}, nil
}

// rewriteModelField decodes into map[string]json.RawMessage rather than
// map[string]any: an `any` round-trip pushes every JSON number through
// float64 and can re-emit e.g. "max_tokens": 1000000 as 1e+06, corrupting
// the forwarded body.
func rewriteModelField(body []byte, model string) ([]byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	raw["model"] = encoded
	return json.Marshal(raw)
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
