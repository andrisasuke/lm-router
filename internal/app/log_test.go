package app

import (
	"strings"
	"testing"
)

func TestRingLoggerKeepsRecentEntriesAndRedactsAuthorization(t *testing.T) {
	logger := NewRingLogger(2, nil)

	logger.Printf("[proxy] one Authorization: Bearer secret-1")
	logger.Printf("[openai-api] two")
	logger.Printf("[openai-api] three authorization=Bearer secret-2")

	entries := logger.Entries()
	if len(entries) != 2 {
		t.Fatalf("entry count=%d", len(entries))
	}
	joined := entries[0].Message + "\n" + entries[1].Message
	if strings.Contains(joined, "secret-1") || strings.Contains(joined, "secret-2") {
		t.Fatalf("authorization leaked: %s", joined)
	}
	if strings.Contains(joined, "one") {
		t.Fatalf("old entry was not evicted: %s", joined)
	}
	if !strings.Contains(joined, "two") || !strings.Contains(joined, "three") {
		t.Fatalf("missing expected entries: %s", joined)
	}
}
