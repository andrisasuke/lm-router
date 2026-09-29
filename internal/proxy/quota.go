package proxy

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/andrisasuke/lm-router/internal/codex"
)

func (s *Server) observeCodexQuota(ctx context.Context, accountID string, header http.Header) {
	if accountID == "" || len(header) == 0 {
		return
	}
	info := codex.ParseQuotaHeaders(header, time.Now())
	if info.Primary == nil && info.Secondary == nil {
		return
	}
	if until, exhausted := codex.QuotaCooldownUntil(info); exhausted && s.store != nil {
		if err := s.store.SetCooldown(ctx, accountID, until); err != nil {
			log.Printf("[openai-api] persist quota cooldown account=%s error=%s", accountID, err)
		}
	}
	if s.onCodexQuota != nil {
		s.onCodexQuota(accountID, info)
	}
}
