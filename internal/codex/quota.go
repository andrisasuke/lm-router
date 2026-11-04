package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type QuotaWindow struct {
	WindowMinutes  int
	UsedPercent    float64
	ResetAfterSecs int
	ResetAt        time.Time
}

type QuotaInfo struct {
	FetchedAt  time.Time
	Primary    *QuotaWindow // 5h rolling window
	Secondary  *QuotaWindow // weekly window
	HeaderKeys []string     // populated when both windows are nil; lists x-* response header keys for debugging
}

// QuotaHTTPError identifies a non-successful ChatGPT usage response without
// exposing its body, which can contain provider diagnostics or identifiers.
type QuotaHTTPError struct {
	Status int
}

func (e *QuotaHTTPError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("Codex quota authentication failed (HTTP %d)", e.Status)
	case http.StatusTooManyRequests:
		return "Codex quota request was rate limited (HTTP 429)"
	default:
		return fmt.Sprintf("Codex quota request failed (HTTP %d)", e.Status)
	}
}

type quotaNetworkError struct {
	cause error
}

func newQuotaNetworkError(cause error) error {
	return &quotaNetworkError{cause: cause}
}

func (e *quotaNetworkError) Error() string {
	return "Codex quota request network error"
}

func (e *quotaNetworkError) Unwrap() error {
	return e.cause
}

func ParseQuotaHeaders(h http.Header, now time.Time) QuotaInfo {
	info := QuotaInfo{
		FetchedAt: now,
		Primary:   parseQuotaWindow(h, "x-codex-primary", now),
		Secondary: parseQuotaWindow(h, "x-codex-secondary", now),
	}
	if info.Primary == nil && info.Secondary == nil {
		for k := range h {
			lk := strings.ToLower(k)
			if strings.HasPrefix(lk, "x-") {
				info.HeaderKeys = append(info.HeaderKeys, lk)
			}
		}
	}
	return info
}

func quotaWindowsRecovered(info QuotaInfo) bool {
	return info.Primary != nil && info.Secondary != nil &&
		info.Primary.UsedPercent < 100 && info.Secondary.UsedPercent < 100
}

type usageQuotaResponse struct {
	RateLimit *struct {
		PrimaryWindow   *usageQuotaWindow `json:"primary_window"`
		SecondaryWindow *usageQuotaWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

type usageQuotaWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *float64 `json:"limit_window_seconds"`
	ResetAfterSeconds  *float64 `json:"reset_after_seconds"`
	ResetAt            *float64 `json:"reset_at"`
}

// ParseQuotaUsage converts the ChatGPT /backend-api/wham/usage payload into
// the shared quota model. Missing or null windows are valid, empty results;
// malformed JSON remains an error so callers do not mistake it for a healthy
// quota response.
func ParseQuotaUsage(body io.Reader, now time.Time) (QuotaInfo, error) {
	var payload usageQuotaResponse
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return QuotaInfo{}, errors.New("Codex quota response was invalid")
	}
	info := QuotaInfo{FetchedAt: now}
	if payload.RateLimit == nil {
		return info, nil
	}
	info.Primary = parseUsageQuotaWindow(payload.RateLimit.PrimaryWindow, now)
	info.Secondary = parseUsageQuotaWindow(payload.RateLimit.SecondaryWindow, now)
	return info, nil
}

func parseUsageQuotaWindow(window *usageQuotaWindow, now time.Time) *QuotaWindow {
	if window == nil || window.UsedPercent == nil || !validUsagePercent(*window.UsedPercent) {
		return nil
	}
	quota := &QuotaWindow{}
	quota.UsedPercent = *window.UsedPercent
	if window.LimitWindowSeconds != nil && *window.LimitWindowSeconds >= 0 {
		quota.WindowMinutes = int(*window.LimitWindowSeconds / 60)
	}
	if window.ResetAfterSeconds != nil {
		quota.ResetAfterSecs = int(*window.ResetAfterSeconds)
	}
	if window.ResetAt != nil && *window.ResetAt > 0 {
		quota.ResetAt = time.Unix(int64(*window.ResetAt), 0)
	} else if quota.ResetAfterSecs > 0 {
		quota.ResetAt = now.Add(time.Duration(quota.ResetAfterSecs) * time.Second)
	}
	return quota
}

func validUsagePercent(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func QuotaCooldownUntil(info QuotaInfo) (time.Time, bool) {
	base := info.FetchedAt
	if base.IsZero() {
		base = time.Now()
	}
	var (
		exhausted bool
		until     time.Time
	)
	for _, window := range []*QuotaWindow{info.Primary, info.Secondary} {
		if window == nil || window.UsedPercent < 100 {
			continue
		}
		exhausted = true
		resetAt := window.ResetAt
		if resetAt.IsZero() && window.ResetAfterSecs > 0 {
			resetAt = base.Add(time.Duration(window.ResetAfterSecs) * time.Second)
		}
		if resetAt.After(until) {
			until = resetAt
		}
	}
	if !exhausted {
		return time.Time{}, false
	}
	if !until.After(base) {
		until = base.Add(time.Minute)
	}
	return until, true
}

func parseQuotaWindow(h http.Header, prefix string, now time.Time) *QuotaWindow {
	usedStr := h.Get(prefix + "-used-percent")
	windowStr := h.Get(prefix + "-window-minutes")
	resetStr := h.Get(prefix + "-reset-after-seconds")
	resetAtStr := h.Get(prefix + "-reset-at")
	if usedStr == "" && windowStr == "" && resetStr == "" && resetAtStr == "" {
		return nil
	}
	w := &QuotaWindow{}
	if v, err := strconv.ParseFloat(usedStr, 64); err == nil {
		w.UsedPercent = math.Round(v)
	}
	if v, err := strconv.Atoi(windowStr); err == nil {
		w.WindowMinutes = v
	}
	if v, err := strconv.ParseInt(resetAtStr, 10, 64); err == nil && v > 0 {
		w.ResetAt = time.Unix(v, 0)
		w.ResetAfterSecs = int(w.ResetAt.Sub(now).Seconds())
	} else if v, err := strconv.Atoi(resetStr); err == nil {
		w.ResetAfterSecs = v
		w.ResetAt = now.Add(time.Duration(v) * time.Second)
	}
	return w
}

func FormatQuotaWindow(w *QuotaWindow) string {
	if w == nil {
		return ""
	}
	var label string
	switch {
	case w.WindowMinutes == 300:
		label = "5h"
	case w.WindowMinutes == 10080:
		label = "weekly"
	case w.WindowMinutes > 0:
		label = fmt.Sprintf("%dm", w.WindowMinutes)
	default:
		label = "?"
	}
	pct := fmt.Sprintf("%d%%", int(w.UsedPercent))
	var resetStr string
	if !w.ResetAt.IsZero() {
		local := w.ResetAt.Local()
		now := time.Now()
		if local.Sub(now) < 24*time.Hour && local.Day() == now.Local().Day() {
			resetStr = "reset " + local.Format("15:04")
		} else {
			resetStr = "resets " + local.Format("15:04") + " on " + local.Format("2 Jan")
		}
	}
	if resetStr != "" {
		return strings.Join([]string{label, "(" + pct + ")", "—", resetStr}, " ")
	}
	return label + " (" + pct + ")"
}
