package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const splitPrompt = `Below are the numbered prompts of one Claude Code session, each with the start of
Claude's reply. Group them into topics: one topic per subject a reader would want as a separate
note, such as building a product versus building a tool to log the work. Most sessions are one
topic; split only when the subjects stand apart. Every prompt belongs to exactly one topic. A
short follow-up ("do that", "push again") belongs with the prompt it follows up. Keep topics in
the order they start.

Answer with JSON only, no other text:
{"topics": [{"title": "working title", "prompts": [1, 2, 3]}]}
`

func splitTopics(s *claudeSession, red *redactions) ([]topic, error) {
	if s.prompts == 1 {
		return []topic{{title: s.aiTitle, prompts: []int{1}}}, nil
	}
	var b strings.Builder
	b.WriteString(splitPrompt)
	n, replied := 0, true
	for _, st := range s.steps {
		switch {
		case st.kind == "prompt":
			n++
			replied = false
			fmt.Fprintf(&b, "\n%d. Prompt: %s\n", n, clip(st.text, 600))
		case st.kind == "say" && !replied:
			replied = true
			fmt.Fprintf(&b, "   Claude: %s\n", clip(st.text, 300))
		}
	}
	out, err := askClaude(red.apply(b.String()))
	if err != nil {
		return nil, err
	}
	i, j := strings.Index(out, "{"), strings.LastIndex(out, "}")
	if i < 0 || j < i {
		return nil, errors.New("Claude's topic list isn't JSON")
	}
	var parsed struct {
		Topics []struct {
			Title   string `json:"title"`
			Prompts []int  `json:"prompts"`
		} `json:"topics"`
	}
	if err := json.Unmarshal([]byte(out[i:j+1]), &parsed); err != nil {
		return nil, fmt.Errorf("Claude's topic list isn't valid JSON: %w", err)
	}
	seen := map[int]bool{}
	var topics []topic
	for _, t := range parsed.Topics {
		if len(t.Prompts) == 0 {
			continue
		}
		for _, p := range t.Prompts {
			if p < 1 || p > s.prompts || seen[p] {
				return nil, fmt.Errorf("Claude's topic list puts prompt %d in the wrong place", p)
			}
			seen[p] = true
		}
		sort.Ints(t.Prompts)
		topics = append(topics, topic{title: t.Title, prompts: t.Prompts})
	}
	if len(seen) != s.prompts {
		return nil, fmt.Errorf("Claude's topic list covers %d of %d prompts", len(seen), s.prompts)
	}
	return topics, nil
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + " …"
	}
	return s
}

const notePrompt = `Write a note from the Claude Code session below. It is a working log for
teaching: what I built, how and why I made the choices, and how I used Claude.

Shape, a decision diary:
- First line "# Title": at most 7 words, sentence case, it states the point.
- One short intro paragraph: what the work is and where the day ended.
- "## What runs now": up to 4 bullets, then one line of exact numbers if the session has them.
- "## Five choices and why" (or fewer, then say how many): each as a bold sentence with the choice,
  then why, then "Not picked:" and the alternative.
- "## How I used Claude": up to 5 bullets with a bold lead-in. Quote my prompt where it teaches
  something. Say where I steered or corrected.
- "## What I'd tell someone starting the same": up to 3 numbered points.
- "## Next": up to 3 bullets.

Rules:
- English, first person as me. Address the reader as "you".
- Only facts from the session. No invented numbers, times or names. Exact numbers with units.
- Text in [brackets] is redacted: keep it generic and never guess what it hides.
- No emoji, no exclamation marks, no superlatives, no marketing words.
- Sentences under 25 words, paragraphs at most 3 sentences, headings in sentence case.
- Output only the markdown of the note, nothing before or after it.
`

func writeNote(out, transcript string) (string, error) {
	var prompt strings.Builder
	prompt.WriteString(notePrompt)
	if example := newestNote(out); example != "" {
		prompt.WriteString("\nAn earlier note, for the voice and the level of detail (not the content):\n\n<example>\n")
		prompt.WriteString(example)
		prompt.WriteString("\n</example>\n")
	}
	prompt.WriteString("\n<session>\n")
	prompt.WriteString(transcript)
	prompt.WriteString("\n</session>\n")

	out, err := askClaude(prompt.String())
	if err != nil {
		return "", err
	}
	note := strings.TrimSpace(out)
	if strings.HasPrefix(note, "```") && strings.HasSuffix(note, "```") {
		_, note, _ = strings.Cut(note, "\n")
		note = strings.TrimSuffix(note, "```")
	}
	return strings.TrimSpace(note) + "\n", nil
}

func askClaude(prompt string) (string, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return "", errors.New("the `claude` command isn't on your PATH. Install Claude Code, or run with --transcript-only")
	}
	cmd := exec.Command("claude", "-p", "--no-session-persistence", "--tools", "", "--output-format", "text")
	cmd.Stdin = strings.NewReader(prompt)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("claude failed: %s", msg)
	}
	return out.String(), nil
}

func newestNote(out string) string {
	entries, err := os.ReadDir(out)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if noteName.MatchString(e.Name()) && !strings.HasSuffix(e.Name(), ".prompt.md") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	raw, _ := os.ReadFile(filepath.Join(out, names[len(names)-1]))
	return string(raw)
}
