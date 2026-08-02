package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/customprovider"
	"github.com/andrisasuke/lm-router/internal/store"
)

// modelRoute is the resolved target for one request: either one of the two
// built-in provider families (Account is unset — the existing failover pools
// in routeResponses/routeAnthropic pick the connection), or a single, named
// custom-provider connection.
type modelRoute struct {
	Provider string
	Model    string
	Account  store.Account
}

// resolveModelRoute extends the existing gpt*/claude* switch (providerForModel,
// left unmodified) with custom-provider prefix routing. A model containing a
// "/" is treated as "<prefix>/<model>" and resolved exclusively against
// registered custom connections — it never falls through to the built-in
// switch, so "claude/foo" cannot accidentally reach real Claude.
//
// ponytail: one connection per prefix for v1 — no failover pool. If a prefix
// ever needs multiple keys, replace this direct lookup with a pool + failover
// loop mirroring routeAnthropic (below), including cooldown/backoff bookkeeping.
func (s *Server) resolveModelRoute(ctx context.Context, raw any) (modelRoute, error) {
	modelStr, ok := raw.(string)
	if !ok || strings.TrimSpace(modelStr) == "" {
		return modelRoute{}, errors.New("model is required")
	}
	modelStr = strings.TrimSpace(modelStr)
	if prefix, rest, found := strings.Cut(modelStr, "/"); found && prefix != "" {
		account, err := s.store.GetAccountByPrefix(ctx, prefix)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return modelRoute{}, fmt.Errorf("unknown custom provider prefix %q", prefix)
			}
			return modelRoute{}, err
		}
		if !account.Enabled {
			return modelRoute{}, fmt.Errorf("custom provider connection %q is disabled", account.Name)
		}
		if strings.TrimSpace(rest) == "" {
			return modelRoute{}, fmt.Errorf("model is required after prefix %q", prefix)
		}
		return modelRoute{Provider: store.ProviderCustom, Model: rest, Account: account}, nil
	}
	provider, model, err := providerForModel(raw)
	if err != nil {
		return modelRoute{}, err
	}
	return modelRoute{Provider: provider, Model: model}, nil
}

func isStreamRequested(body []byte) bool {
	var envelope struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &envelope)
	return envelope.Stream
}

// dispatchCustom forwards body to route.Account verbatim (passthrough only —
// no request/response translation between mismatched shapes; if that's ever
// needed, it belongs in customprovider.Client next to the model-field
// rewrite, not spread across these dispatch functions) and writes the
// upstream response through unmodified, including non-2xx statuses — there is
// exactly one connection for this prefix, so there is nothing to fail over to.
func (s *Server) dispatchCustom(ctx context.Context, w http.ResponseWriter, route modelRoute, path string, body []byte, writeError func(status int, msg string)) {
	params := customprovider.ExecuteParams{Account: route.Account, Path: path, Model: route.Model, Body: body}
	if isStreamRequested(body) {
		stream, err := s.custom.OpenStream(ctx, params)
		if err != nil {
			writeError(http.StatusBadGateway, err.Error())
			return
		}
		defer stream.Body.Close()
		w.Header().Set("Content-Type", contentTypeOrDefault(stream.Header.Get("Content-Type")))
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(stream.Status)
		_ = codex.CopyStream(w, stream.Body)
		return
	}
	result, err := s.custom.Execute(ctx, params)
	if err != nil {
		writeError(http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", contentTypeOrDefault(result.Header.Get("Content-Type")))
	w.WriteHeader(result.Status)
	_, _ = w.Write(result.Body)
}

func (s *Server) dispatchCustomResponses(w http.ResponseWriter, r *http.Request, route modelRoute, body []byte) {
	s.dispatchCustom(r.Context(), w, route, "/responses", body, func(status int, msg string) {
		writeOpenAIError(w, status, "proxy_error", msg)
	})
}

func (s *Server) dispatchCustomChatCompletions(w http.ResponseWriter, r *http.Request, route modelRoute, body []byte) {
	s.dispatchCustom(r.Context(), w, route, "/chat/completions", body, func(status int, msg string) {
		writeOpenAIError(w, status, "proxy_error", msg)
	})
}

func (s *Server) dispatchCustomMessages(w http.ResponseWriter, r *http.Request, route modelRoute, body []byte) {
	s.dispatchCustom(r.Context(), w, route, "/messages", body, func(status int, msg string) {
		writeAnthropicError(w, status, "api_error", msg)
	})
}
