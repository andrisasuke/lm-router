package codex

import (
	"net/http"
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
