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
	if entries[0].ID == 0 || entries[1].ID != entries[0].ID+1 {
		t.Fatalf("unexpected entry IDs: %+v", entries)
	}
	if entry, ok := logger.Entry(entries[0].ID); !ok || entry.Message != entries[0].Message {
		t.Fatalf("entry lookup failed: ok=%t entry=%+v", ok, entry)
	}
}

func TestRingLoggerAcceptsStandardLoggerOutput(t *testing.T) {
	logger := NewRingLogger(2, nil)

	n, err := logger.Write([]byte("http: internal server error\n"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len("http: internal server error\n") {
		t.Fatalf("bytes written=%d", n)
	}
	entries := logger.Entries()
	if len(entries) != 1 || entries[0].Message != "http: internal server error" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestRingLoggerIDsRemainMonotonicAfterClear(t *testing.T) {
	logger := NewRingLogger(2, nil)
	logger.Printf("one")
	first := logger.Entries()[0].ID
	logger.Clear()
	logger.Printf("two")
	second := logger.Entries()[0].ID
	if second <= first {
		t.Fatalf("entry ID did not advance after clear: first=%d second=%d", first, second)
	}
	if _, ok := logger.Entry(first); ok {
		t.Fatal("cleared entry remained addressable")
	}
}
