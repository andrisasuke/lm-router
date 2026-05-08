package main

import "testing"

func TestVersionTextIncludesVersionCommitAndBuildDate(t *testing.T) {
	got := versionText("0.0.1", "abc123", "2026-05-08T00:00:00Z")
	want := "Version: 0.0.1\nCommit: abc123\nBuildDate: 2026-05-08T00:00:00Z\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSanitizeTerminalInputStripsArrowSequences(t *testing.T) {
	raw := "http://localhost:1455/auth/callback?code=abc&state=ok\x1b[D\x1b[D\n"
	got := sanitizeTerminalInput(raw)
	want := "http://localhost:1455/auth/callback?code=abc&state=ok"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSanitizeTerminalInputStripsBracketedPasteMarkers(t *testing.T) {
	raw := "\x1b[200~http://localhost:1455/auth/callback?code=abc&state=ok\x1b[201~\r\n"
	got := sanitizeTerminalInput(raw)
	want := "http://localhost:1455/auth/callback?code=abc&state=ok"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRandomBase64URLStringMatchesCodexPKCELength(t *testing.T) {
	got := mustRandomBase64URLString(32)
	if len(got) != 43 {
		t.Fatalf("len=%d want 43", len(got))
	}
	for _, ch := range got {
		if !(ch >= 'a' && ch <= 'z') && !(ch >= 'A' && ch <= 'Z') && !(ch >= '0' && ch <= '9') && ch != '-' && ch != '_' {
			t.Fatalf("unexpected base64url char %q in %q", ch, got)
		}
	}
}

func TestHumanErrorExtractsJSONErrorMessage(t *testing.T) {
	raw := `{"error":{"message":"Invalid request. Please try again later.","type":"invalid_request_error"}}`
	got := humanError(raw)
	want := "Invalid request. Please try again later."
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHumanErrorExtractsDetail(t *testing.T) {
	raw := `{"detail":"Instructions are required"}`
	got := humanError(raw)
	want := "Instructions are required"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestLineEditorMovesCursorAndInsertsInMiddle(t *testing.T) {
	var editor lineEditor
	for _, ch := range "abcd" {
		editor.insert(ch)
	}
	editor.moveLeft()
	editor.moveLeft()
	editor.insert('X')

	if got, want := editor.String(), "abXcd"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if editor.cursor != 3 {
		t.Fatalf("cursor=%d want 3", editor.cursor)
	}
}

func TestLineEditorBackspaceDeletesBeforeCursor(t *testing.T) {
	var editor lineEditor
	for _, ch := range "abcd" {
		editor.insert(ch)
	}
	editor.moveLeft()
	editor.moveLeft()
	editor.backspace()

	if got, want := editor.String(), "acd"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if editor.cursor != 1 {
		t.Fatalf("cursor=%d want 1", editor.cursor)
	}
}

func TestLineEditorDeleteRemovesAtCursor(t *testing.T) {
	var editor lineEditor
	for _, ch := range "abcd" {
		editor.insert(ch)
	}
	editor.moveLeft()
	editor.moveLeft()
	editor.delete()

	if got, want := editor.String(), "abd"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if editor.cursor != 2 {
		t.Fatalf("cursor=%d want 2", editor.cursor)
	}
}
