package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/andrisasuke/lm-router/internal/store"
)

const UsageRetryCooldown = 3 * time.Minute

type UsageWindow struct {
	Name        string
	Utilization float64
	ResetsAt    time.Time
}

type UsageInfo struct {
	Connected bool
	Available bool
	Status    int
	FetchedAt time.Time
	RetryAt   time.Time
	Windows   []UsageWindow
}

func (c *Client) FetchUsage(ctx context.Context, account store.Account) (UsageInfo, error) {
	return c.fetchUsage(ctx, account, true)
}

func (c *Client) fetchUsage(ctx context.Context, account store.Account, allowRefresh bool) (UsageInfo, error) {
	if c.tokens == nil {
		return UsageInfo{}, errors.New("anthropic token manager is not configured")
	}
	account, err := c.tokens.EnsureFresh(ctx, account.ID)
	if err != nil {
		return UsageInfo{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.usageURL, nil)
	if err != nil {
		return UsageInfo{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+account.AccessToken)
	req.Header.Set("Anthropic-Version", AnthropicVersion)
	req.Header.Set("Anthropic-Beta", mergeBeta(nil, OAuthBeta, ClaudeCodeBeta))
	req.Header["User-Agent"] = []string{""}
	resp, err := c.http.Do(req)
	if err != nil {
		return UsageInfo{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return UsageInfo{}, err
	}
	if allowRefresh && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		if _, err := c.tokens.RefreshNow(ctx, account.ID); err != nil {
			return UsageInfo{}, err
		}
		return c.fetchUsage(ctx, account, false)
	}
	now := time.Now()
	info := UsageInfo{Connected: resp.StatusCode == http.StatusTooManyRequests, Available: false, Status: resp.StatusCode, FetchedAt: now}
	if resp.StatusCode == http.StatusTooManyRequests {
		info.RetryAt = now.Add(UsageRetryCooldown)
		return info, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return info, errors.New(string(body))
	}
	info.Connected = true
	info.Available = true
	info.Windows = parseUsageWindows(body)
	return info, nil
}

func parseUsageWindows(body []byte) []UsageWindow {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	windows := make([]UsageWindow, 0)
	for name, raw := range payload {
		var value struct {
			Utilization float64 `json:"utilization"`
			ResetsAt    string  `json:"resets_at"`
		}
		if json.Unmarshal(raw, &value) != nil || (value.Utilization == 0 && value.ResetsAt == "") {
			continue
		}
		reset, _ := time.Parse(time.RFC3339, value.ResetsAt)
		windows = append(windows, UsageWindow{Name: strings.ReplaceAll(name, "_", " "), Utilization: value.Utilization, ResetsAt: reset})
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Name < windows[j].Name })
	return windows
}
