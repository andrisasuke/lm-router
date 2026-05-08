package app

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Logger interface {
	Printf(format string, args ...any)
}

type LogEntry struct {
	Time    time.Time
	Source  string
	Message string
}

type RingLogger struct {
	mu      sync.Mutex
	limit   int
	entries []LogEntry
	stdout  io.Writer
}

func NewRingLogger(limit int, stdout io.Writer) *RingLogger {
	if limit <= 0 {
		limit = 500
	}
	return &RingLogger{limit: limit, stdout: stdout}
}

func (l *RingLogger) Printf(format string, args ...any) {
	if l == nil {
		return
	}
	msg := redactSecrets(fmt.Sprintf(format, args...))
	entry := LogEntry{Time: time.Now(), Source: detectSource(msg), Message: msg}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) == l.limit {
		copy(l.entries, l.entries[1:])
		l.entries[len(l.entries)-1] = entry
	} else {
		l.entries = append(l.entries, entry)
	}
	if l.stdout != nil {
		_, _ = fmt.Fprintf(l.stdout, "%s\n", msg)
	}
}

func (l *RingLogger) Entries() []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]LogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

func (l *RingLogger) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = nil
}

var authorizationPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*[:=]\s*Bearer\s+)[^\s,"\]}]+`),
	regexp.MustCompile(`(?i)("Authorization"\s*:\s*\["Bearer\s+)[^"\]]+`),
}

func redactSecrets(msg string) string {
	for _, pattern := range authorizationPatterns {
		msg = pattern.ReplaceAllString(msg, `${1}<redacted>`)
	}
	return msg
}

func detectSource(msg string) string {
	switch {
	case strings.Contains(msg, "[openai-api]"):
		return "openai"
	case strings.Contains(msg, "[request]"):
		return "proxy"
	default:
		return "app"
	}
}
