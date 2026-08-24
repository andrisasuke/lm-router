package app

import (
	"strings"
	"testing"

	"github.com/andrisasuke/lm-router/internal/codex"
)

func TestCodexConfigTextEnablesRouterNativeWebSearch(t *testing.T) {
	config := CodexConfigText(19090, "router-key", "gpt-5.6-sol")
	for _, want := range []string{
		`web_search = "live"`,
		`http_headers = { "` + codex.RouterWebSearchHeader + `" = "live" }`,
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("config missing %q:\n%s", want, config)
		}
	}
}
