package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A session with one prompt: Claude says something, runs a command, and ends on a question.
const sessionJSONL = `{"type":"user","origin":{"kind":"human"},"timestamp":"2026-11-09T14:30:00Z","message":{"content":"fix the bug"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Looking."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok\nPASS"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Done. Push it?"}]}}
{"type":"user","isSidechain":true,"origin":{"kind":"human"},"message":{"content":"a subagent's prompt"}}
`

func testSession(t *testing.T) *claudeSession {
	t.Helper()
	file := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(file, []byte(sessionJSONL), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := readSession(file)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// afterHeader drops the lines every transcript starts with.
func afterHeader(t *testing.T, transcript string) string {
	t.Helper()
	_, body, ok := strings.Cut(transcript, "are redacted.\n")
	if !ok {
		t.Fatalf("no header in %q", transcript)
	}
	return body
}

func TestTranscriptFor(t *testing.T) {
	s := testSession(t)
	if s.prompts != 1 {
		t.Fatalf("prompts = %d, want 1 (sidechain prompts don't count)", s.prompts)
	}
	got := afterHeader(t, s.transcriptFor([]int{1}))
	want := "\n> fix the bug\n" +
		"\n⏺ Looking.\n" +
		"\n⏺ Bash(go test ./...)\n  ⎿  ok\n     PASS\n" +
		"\n⏺ Done. Push it?\n"
	if got != want {
		t.Errorf("transcript:\n%s\nwant:\n%s", got, want)
	}
}

func TestBriefFor(t *testing.T) {
	s := testSession(t)
	got := afterHeader(t, s.briefFor([]int{1}))
	want := "\n> fix the bug\n" +
		"\n⏺ Claude " + workWord("fix the bug", 1) + "\n  ⎿  1 command\n" +
		"\n⏺ Done. Push it?\n"
	if got != want {
		t.Errorf("brief:\n%s\nwant:\n%s", got, want)
	}
}

func TestCountLine(t *testing.T) {
	got := countLine([]string{"Bash", "Read", "Bash", "Edit", "Write", "WebSearch", "mcp__x__y"})
	want := "7 tool calls: 2 commands, 1 read, 2 edits, 1 web search, 1 other"
	if got != want {
		t.Errorf("countLine = %q, want %q", got, want)
	}
}

func TestRedactions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	if err := os.MkdirAll(configDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	list := "# clients\nacme => [client]\nacme cloud => [cloud]\n"
	if err := os.WriteFile(redactFile(), []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	red, err := loadRedactions()
	if err != nil {
		t.Fatal(err)
	}
	dashed := nonAlnum.ReplaceAllString(home, "-")
	in := "Acme Cloud and ACME in " + home + "/code and " + dashed + "-code, mail me@example.com, key ghp_abcdefghijklmnopqrstu"
	want := "[cloud] and [client] in ~/code and ~-code, mail [email], key [secret]"
	if got := red.apply(in); got != want {
		t.Errorf("apply:\n got %q\nwant %q", got, want)
	}
}

func TestRedactionsRejectsBadLine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(configDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(redactFile(), []byte("acme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRedactions(); err == nil || !strings.Contains(err.Error(), "redact.txt:1") {
		t.Errorf("err = %v, want one naming redact.txt:1", err)
	}
}
