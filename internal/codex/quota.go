package codex

import (
	"fmt"
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
