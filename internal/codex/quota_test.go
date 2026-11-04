package codex

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseQuotaHeaders_full(t *testing.T) {
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "40")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-after-seconds", "3600")
	h.Set("x-codex-secondary-used-percent", "25")
	h.Set("x-codex-secondary-window-minutes", "10080")
	h.Set("x-codex-secondary-reset-after-seconds", "86400")

	info := ParseQuotaHeaders(h, now)
	if info.Primary == nil {
		t.Fatal("expected primary window")
	}
	if info.Primary.UsedPercent != 40 {
		t.Errorf("primary UsedPercent: got %v, want 40", info.Primary.UsedPercent)
	}
	if info.Primary.WindowMinutes != 300 {
		t.Errorf("primary WindowMinutes: got %v, want 300", info.Primary.WindowMinutes)
	}
	if info.Primary.ResetAfterSecs != 3600 {
		t.Errorf("primary ResetAfterSecs: got %v, want 3600", info.Primary.ResetAfterSecs)
	}
	if info.Secondary == nil {
		t.Fatal("expected secondary window")
	}
	if info.Secondary.UsedPercent != 25 {
		t.Errorf("secondary UsedPercent: got %v, want 25", info.Secondary.UsedPercent)
	}
	if info.Secondary.WindowMinutes != 10080 {
		t.Errorf("secondary WindowMinutes: got %v, want 10080", info.Secondary.WindowMinutes)
	}
}

func TestParseQuotaHeaders_missing(t *testing.T) {
	now := time.Now()
	info := ParseQuotaHeaders(http.Header{}, now)
	if info.Primary != nil {
		t.Error("expected nil primary when headers absent")
	}
	if info.Secondary != nil {
		t.Error("expected nil secondary when headers absent")
	}
}

func TestParseQuotaHeaders_malformed(t *testing.T) {
	now := time.Now()
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "notanumber")
	h.Set("x-codex-primary-window-minutes", "abc")
	h.Set("x-codex-primary-reset-after-seconds", "!!")

	// Headers present but values unparseable — should return non-nil window with zero values
	info := ParseQuotaHeaders(h, now)
	if info.Primary == nil {
		t.Fatal("expected non-nil primary even when values are malformed")
	}
	if info.Primary.UsedPercent != 0 {
		t.Errorf("malformed UsedPercent should be 0, got %v", info.Primary.UsedPercent)
	}
	if info.Primary.WindowMinutes != 0 {
		t.Errorf("malformed WindowMinutes should be 0, got %v", info.Primary.WindowMinutes)
	}
}

func TestParseQuotaHeaders_resetAt(t *testing.T) {
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	resetAt := now.Add(90 * time.Minute)
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "40")
	h.Set("x-codex-primary-reset-at", strconv.FormatInt(resetAt.Unix(), 10))
	h.Set("x-codex-primary-reset-after-seconds", "60")

	info := ParseQuotaHeaders(h, now)
	if info.Primary == nil {
		t.Fatal("expected primary window")
	}
	if !info.Primary.ResetAt.Equal(resetAt) {
		t.Errorf("primary ResetAt: got %v, want %v", info.Primary.ResetAt, resetAt)
	}
	if info.Primary.ResetAfterSecs != 90*60 {
		t.Errorf("primary ResetAfterSecs: got %d, want %d", info.Primary.ResetAfterSecs, 90*60)
	}
}

func TestParseQuotaHeaders_collectsXHeaderKeysWhenQuotaMissing(t *testing.T) {
	h := http.Header{}
	h.Set("X-Request-Id", "req_123")
	h.Set("X-Ratelimit-Limit", "100")
	h.Set("Content-Type", "application/json")

	info := ParseQuotaHeaders(h, time.Now())
	if info.Primary != nil || info.Secondary != nil {
		t.Fatal("expected quota windows to be nil")
	}
	slices.Sort(info.HeaderKeys)
	want := []string{"x-ratelimit-limit", "x-request-id"}
	if !slices.Equal(info.HeaderKeys, want) {
		t.Errorf("HeaderKeys: got %v, want %v", info.HeaderKeys, want)
	}
}

func TestParseQuotaUsage(t *testing.T) {
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	info, err := ParseQuotaUsage(strings.NewReader(`{
		"rate_limit": {
			"primary_window": {
				"used_percent": 40,
				"limit_window_seconds": 18000,
				"reset_after_seconds": 3600,
				"reset_at": 1770000000
			},
			"secondary_window": {
				"used_percent": 25,
				"limit_window_seconds": 604800,
				"reset_after_seconds": 86400,
				"reset_at": 1770086400
			}
		}
	}`), now)
	if err != nil {
		t.Fatalf("parse usage: %v", err)
	}
	if !info.FetchedAt.Equal(now) || info.Primary == nil || info.Secondary == nil {
		t.Fatalf("info=%+v", info)
	}
	if got := info.Primary; got.UsedPercent != 40 || got.WindowMinutes != 300 || got.ResetAfterSecs != 3600 || got.ResetAt.Unix() != 1770000000 {
		t.Fatalf("primary=%+v", got)
	}
	if got := info.Secondary; got.UsedPercent != 25 || got.WindowMinutes != 10080 || got.ResetAfterSecs != 86400 || got.ResetAt.Unix() != 1770086400 {
		t.Fatalf("secondary=%+v", got)
	}
}

func TestParseQuotaUsageAllowsNullOrMissingWindows(t *testing.T) {
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	for _, payload := range []string{
		`{}`,
		`{"rate_limit":{"primary_window":null,"secondary_window":null}}`,
		`{"rate_limit":{"primary_window":null}}`,
	} {
		info, err := ParseQuotaUsage(strings.NewReader(payload), now)
		if err != nil {
			t.Fatalf("parse %s: %v", payload, err)
		}
		if !info.FetchedAt.Equal(now) || info.Primary != nil || info.Secondary != nil {
			t.Fatalf("payload=%s info=%+v", payload, info)
		}
	}
}

func TestParseQuotaUsageRequiresValidUsedPercent(t *testing.T) {
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name          string
		payload       string
		wantPrimary   bool
		wantSecondary bool
	}{
		{
			name:    "both windows missing used percent",
			payload: `{"rate_limit":{"primary_window":{"limit_window_seconds":18000},"secondary_window":{"limit_window_seconds":604800}}}`,
		},
		{
			name:    "both windows null used percent",
			payload: `{"rate_limit":{"primary_window":{"used_percent":null},"secondary_window":{"used_percent":null}}}`,
		},
		{
			name:          "partial payload",
			payload:       `{"rate_limit":{"primary_window":{"used_percent":0},"secondary_window":{"reset_after_seconds":60}}}`,
			wantPrimary:   true,
			wantSecondary: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			info, err := ParseQuotaUsage(strings.NewReader(test.payload), now)
			if err != nil {
				t.Fatalf("parse usage: %v", err)
			}
			if (info.Primary != nil) != test.wantPrimary || (info.Secondary != nil) != test.wantSecondary {
				t.Fatalf("info=%+v, want primary=%t secondary=%t", info, test.wantPrimary, test.wantSecondary)
			}
			if quotaWindowsRecovered(info) {
				t.Fatal("incomplete quota data must not count as recovered")
			}
		})
	}
}

func TestParseQuotaUsagePreservesPercentBelowExhaustion(t *testing.T) {
	info, err := ParseQuotaUsage(strings.NewReader(`{
		"rate_limit": {
			"primary_window": {"used_percent": 99.9},
			"secondary_window": {"used_percent": 0}
		}
	}`), time.Now())
	if err != nil {
		t.Fatalf("parse usage: %v", err)
	}
	if info.Primary == nil || info.Primary.UsedPercent != 99.9 {
		t.Fatalf("primary=%+v", info.Primary)
	}
	if _, exhausted := QuotaCooldownUntil(info); exhausted {
		t.Fatal("quota below 100% must not enter cooldown")
	}
}

func TestQuotaCooldownUntilUsesAnyExhaustedWindowAndLatestReset(t *testing.T) {
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	primaryReset := now.Add(2 * time.Hour)
	secondaryReset := now.Add(5 * 24 * time.Hour)
	until, exhausted := QuotaCooldownUntil(QuotaInfo{
		FetchedAt: now,
		Primary:   &QuotaWindow{UsedPercent: 100, ResetAt: primaryReset},
		Secondary: &QuotaWindow{UsedPercent: 100, ResetAt: secondaryReset},
	})
	if !exhausted {
		t.Fatal("expected exhausted quota")
	}
	if !until.Equal(secondaryReset) {
		t.Fatalf("cooldown=%v, want latest reset %v", until, secondaryReset)
	}
}

func TestQuotaCooldownUntilDetectsWeeklyExhaustionAlone(t *testing.T) {
	now := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	secondaryReset := now.Add(5 * 24 * time.Hour)
	until, exhausted := QuotaCooldownUntil(QuotaInfo{
		FetchedAt: now,
		Primary:   &QuotaWindow{UsedPercent: 0, ResetAt: now.Add(5 * time.Hour)},
		Secondary: &QuotaWindow{UsedPercent: 100, ResetAt: secondaryReset},
	})
	if !exhausted || !until.Equal(secondaryReset) {
		t.Fatalf("exhausted=%t cooldown=%v, want %v", exhausted, until, secondaryReset)
	}
}

func TestQuotaCooldownUntilAllowsBothAvailableWindows(t *testing.T) {
	_, exhausted := QuotaCooldownUntil(QuotaInfo{
		FetchedAt: time.Now(),
		Primary:   &QuotaWindow{UsedPercent: 99},
		Secondary: &QuotaWindow{UsedPercent: 99},
	})
	if exhausted {
		t.Fatal("quota below 100% must remain available")
	}
}

func TestFormatQuotaWindow_fiveHour(t *testing.T) {
	now := time.Now()
	w := &QuotaWindow{
		WindowMinutes:  300,
		UsedPercent:    40,
		ResetAfterSecs: 900,
		ResetAt:        now.Add(900 * time.Second),
	}
	s := FormatQuotaWindow(w)
	if s == "" {
		t.Fatal("expected non-empty format")
	}
	if len(s) < 4 {
		t.Errorf("format too short: %q", s)
	}
}

func TestFormatQuotaWindow_weekly(t *testing.T) {
	now := time.Now()
	w := &QuotaWindow{
		WindowMinutes:  10080,
		UsedPercent:    25,
		ResetAfterSecs: 7 * 24 * 3600,
		ResetAt:        now.Add(7 * 24 * time.Hour),
	}
	s := FormatQuotaWindow(w)
	if s == "" {
		t.Fatal("expected non-empty format")
	}
}

func TestFormatQuotaWindow_nil(t *testing.T) {
	if s := FormatQuotaWindow(nil); s != "" {
		t.Errorf("nil window should return empty string, got %q", s)
	}
}

func TestFormatQuotaWindow_noReset(t *testing.T) {
	w := &QuotaWindow{WindowMinutes: 300, UsedPercent: 50}
	s := FormatQuotaWindow(w)
	if s != "5h (50%)" {
		t.Errorf("no reset: got %q, want %q", s, "5h (50%)")
	}
}
