package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Reading a Claude Code session, redacting it, and asking Claude for the notes.

// outputLines is how much of a tool's output the full transcript, the one Claude drafts from, keeps.
const outputLines = 6

// skippedTools are tool calls that only load other tools; they say nothing about the work.
var skippedTools = map[string]bool{"ToolSearch": true, "TodoWrite": true}

// output is one note of a run: its transcript and, unless --transcript-only, Claude's draft.
type output struct {
	title, body, transcript string
	note, prompt            string
}

// topic is a group of prompts, numbered from 1, that belong in one note.
type topic struct {
	title   string
	prompts []int
}

func projectsDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// findSession resolves the argument: a .jsonl file, a project folder (its newest session), or
// nothing (the newest session of any project).
func findSession(arg string) (string, error) {
	root := projectsDir()
	if strings.HasSuffix(arg, ".jsonl") {
		if _, err := os.Stat(arg); err != nil {
			return "", fmt.Errorf("can't read %s", arg)
		}
		return arg, nil
	}
	dir := root
	if arg != "" {
		abs, err := filepath.Abs(expandHome(arg))
		if err != nil {
			return "", err
		}
		dir = filepath.Join(root, nonAlnum.ReplaceAllString(abs, "-"))
		if _, err := os.Stat(dir); err != nil {
			return "", fmt.Errorf("no Claude Code sessions for %s. Start one there with `claude`, or pass the .jsonl file", abs)
		}
	}
	var newest string
	var newestAt time.Time
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		// Sessions sit directly in a project folder; deeper files belong to subagents.
		if d.IsDir() && p != dir && filepath.Dir(p) != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(newestAt) {
			newest, newestAt = p, info.ModTime()
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if newest == "" {
		return "", fmt.Errorf("no sessions in %s", dir)
	}
	return newest, nil
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, rest)
	}
	return p
}

// A line of the session file. Only the fields the transcript needs.
type entry struct {
	Type        string    `json:"type"`
	IsSidechain bool      `json:"isSidechain"`
	IsMeta      bool      `json:"isMeta"`
	Timestamp   time.Time `json:"timestamp"`
	AITitle     string    `json:"aiTitle"`
	Origin      *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// step is one thing that happened, in order.
type step struct {
	kind   string // prompt, say, tool, out
	text   string
	toolID string
	tool   string   // the tool's name, for counting
	asks   []string // the questions of an AskUserQuestion call
}

type claudeSession struct {
	started time.Time
	aiTitle string
	steps   []step
	outputs map[string]string
	prompts int
}

func readSession(file string) (*claudeSession, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := &claudeSession{outputs: map[string]string{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.IsSidechain {
			continue
		}
		if e.Type == "ai-title" && e.AITitle != "" {
			s.aiTitle = e.AITitle
		}
		if e.Message == nil || (e.Type != "user" && e.Type != "assistant") {
			continue
		}
		var text string
		var blocks []block
		if json.Unmarshal(e.Message.Content, &text) != nil {
			if json.Unmarshal(e.Message.Content, &blocks) != nil {
				continue
			}
		}
		switch {
		case e.Type == "user" && text != "":
			s.addUser(e, text)
		case e.Type == "user":
			for _, b := range blocks {
				switch b.Type {
				case "tool_result":
					s.outputs[b.ToolUseID] = resultText(b)
				case "text":
					s.addUser(e, b.Text)
				}
			}
		default:
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if t := strings.TrimSpace(b.Text); t != "" {
						s.steps = append(s.steps, step{kind: "say", text: t})
					}
				case "tool_use":
					if !skippedTools[b.Name] {
						s.steps = append(s.steps, step{kind: "tool", text: toolLine(b), toolID: b.ID, tool: b.Name, asks: questions(b)})
					}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if s.prompts == 0 {
		return nil, fmt.Errorf("%s has no prompts", file)
	}
	return s, nil
}

var (
	commandName = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	commandArgs = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	pastedTag   = regexp.MustCompile(`</?pasted_content[^>]*>`)
	systemNote  = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
)

func (s *claudeSession) addUser(e entry, text string) {
	text = strings.TrimSpace(text)
	if e.IsMeta || text == "" {
		return
	}
	if strings.HasPrefix(text, "[Request interrupted") {
		s.steps = append(s.steps, step{kind: "out", text: text})
		return
	}
	if e.Origin == nil || e.Origin.Kind != "human" {
		return
	}
	if m := commandName.FindStringSubmatch(text); m != nil {
		cmd := strings.TrimSpace(m[1])
		if a := commandArgs.FindStringSubmatch(text); a != nil && strings.TrimSpace(a[1]) != "" {
			cmd += " " + strings.TrimSpace(a[1])
		}
		text = cmd
	}
	if text == "clear" || text == "/clear" {
		return
	}
	text = strings.TrimSpace(pastedTag.ReplaceAllString(text, ""))
	if s.prompts == 0 {
		s.started = e.Timestamp
	}
	s.prompts++
	s.steps = append(s.steps, step{kind: "prompt", text: text})
}

func resultText(b block) string {
	var text string
	if json.Unmarshal(b.Content, &text) != nil {
		var parts []block
		json.Unmarshal(b.Content, &parts)
		var out []string
		for _, p := range parts {
			switch p.Type {
			case "text":
				out = append(out, p.Text)
			case "image":
				out = append(out, "[screenshot]")
			}
		}
		text = strings.Join(out, "\n")
	}
	return strings.TrimSpace(systemNote.ReplaceAllString(text, ""))
}

// toolLine writes a tool call the way the terminal shows it: Name(the one argument that matters).
func toolLine(b block) string {
	var in map[string]any
	json.Unmarshal(b.Input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	name, arg := b.Name, ""
	if parts := strings.Split(b.Name, "__"); len(parts) == 3 && parts[0] == "mcp" {
		name = parts[2] + " (MCP)"
	}
	switch b.Name {
	case "Bash":
		arg = str("command")
	case "Read", "Write", "Edit", "NotebookEdit":
		arg = str("file_path")
	case "Skill":
		arg = str("skill")
	case "Agent":
		arg = str("description")
	case "AskUserQuestion":
		if qs, ok := in["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				arg, _ = q["question"].(string)
			}
		}
	default:
		for _, k := range []string{"url", "query", "action", "description", "prompt", "path"} {
			if arg = str(k); arg != "" {
				break
			}
		}
	}
	arg, _, cut := strings.Cut(strings.TrimSpace(arg), "\n")
	if r := []rune(arg); len(r) > 120 {
		arg, cut = string(r[:120]), true
	}
	if cut {
		arg += " …"
	}
	return name + "(" + arg + ")"
}

func (s *claudeSession) allPrompts() []int {
	all := make([]int, s.prompts)
	for i := range all {
		all[i] = i + 1
	}
	return all
}

// transcriptFor prints the given prompts, and everything after each up to the next prompt, in
// the format the note viewer reads: "> " a prompt, "⏺ " Claude's text or a tool call, "⎿" its
// output, indented lines continue the block above.
func (s *claudeSession) transcriptFor(prompts []int) string {
	keep := map[int]bool{}
	for _, n := range prompts {
		keep[n] = true
	}
	var b strings.Builder
	b.WriteString("Generated from a Claude Code session. Prompts are verbatim; tool output is cut to ")
	fmt.Fprintf(&b, "%d lines.\nNames in [brackets] are redacted.\n", outputLines)
	indent := func(text, first, rest string) {
		for i, line := range strings.Split(text, "\n") {
			switch {
			case strings.TrimSpace(line) == "":
				b.WriteString("\n")
			case i == 0:
				b.WriteString(first + line + "\n")
			default:
				b.WriteString(rest + line + "\n")
			}
		}
	}
	n := 0
	for _, st := range s.steps {
		if st.kind == "prompt" {
			n++
		}
		if !keep[max(n, 1)] {
			continue
		}
		b.WriteString("\n")
		switch st.kind {
		case "prompt":
			indent(st.text, "> ", "  ")
		case "say":
			indent(st.text, "⏺ ", "  ")
		case "out":
			indent(st.text, "  ⎿  ", "     ")
		case "tool":
			b.WriteString("⏺ " + st.text + "\n")
			out, ok := s.outputs[st.toolID]
			if !ok || out == "" {
				continue
			}
			lines := strings.Split(out, "\n")
			if len(lines) > outputLines {
				more := len(lines) - outputLines
				lines = append(lines[:outputLines], fmt.Sprintf("… +%d lines", more))
			}
			indent(strings.Join(lines, "\n"), "  ⎿  ", "     ")
		}
	}
	return b.String()
}

// questions are the questions of an AskUserQuestion call; nil for any other tool.
func questions(b block) []string {
	if b.Name != "AskUserQuestion" {
		return nil
	}
	var in struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
	}
	json.Unmarshal(b.Input, &in)
	var qs []string
	for _, q := range in.Questions {
		if q := strings.TrimSpace(q.Question); q != "" {
			qs = append(qs, q)
		}
	}
	return qs
}

// workWords are the words Claude Code shows while it works.
var workWords = []string{
	"accomplishing", "actualizing", "baking", "brewing", "calculating", "cerebrating", "churning",
	"clauding", "coalescing", "cogitating", "computing", "conjuring", "considering", "cooking",
	"crafting", "creating", "crunching", "deliberating", "determining", "doing", "effecting",
	"finagling", "forging", "forming", "generating", "hatching", "herding", "honking", "hustling",
	"ideating", "inferring", "manifesting", "marinating", "moseying", "mulling", "mustering",
	"musing", "noodling", "percolating", "pondering", "processing", "puttering", "reticulating",
	"ruminating", "schlepping", "shucking", "simmering", "smooshing", "spinning", "stewing",
	"synthesizing", "thinking", "transmuting", "vibing", "working",
}

// workWord picks a word from the prompt, so a dry run and the real run print the same one.
func workWord(prompt string, n int) string {
	h := fnv.New32a()
	fmt.Fprintf(h, "%d %s", n, prompt)
	return workWords[h.Sum32()%uint32(len(workWords))]
}

// toolKinds are the counts the brief transcript shows, in this order. Tools not listed count as
// "other".
var toolKinds = []struct {
	one, many string
	tools     []string
}{
	{"command", "commands", []string{"Bash"}},
	{"read", "reads", []string{"Read"}},
	{"search", "searches", []string{"Grep", "Glob"}},
	{"edit", "edits", []string{"Edit", "Write", "MultiEdit", "NotebookEdit"}},
	{"web search", "web searches", []string{"WebSearch"}},
	{"page fetched", "pages fetched", []string{"WebFetch"}},
	{"agent", "agents", []string{"Agent", "Task"}},
	{"skill", "skills", []string{"Skill"}},
	{"other", "other", nil},
}

// countLine sums up tool calls: "14 tool calls: 6 commands, 4 reads, 3 edits, 1 web search".
func countLine(tools []string) string {
	counts := make([]int, len(toolKinds))
	for _, t := range tools {
		k := len(toolKinds) - 1
		for i, kind := range toolKinds {
			if slices.Contains(kind.tools, t) {
				k = i
				break
			}
		}
		counts[k]++
	}
	var parts []string
	for i, n := range counts {
		switch {
		case n == 1:
			parts = append(parts, "1 "+toolKinds[i].one)
		case n > 1:
			parts = append(parts, fmt.Sprintf("%d %s", n, toolKinds[i].many))
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return fmt.Sprintf("%d tool calls: %s", len(tools), strings.Join(parts, ", "))
}

var answer = regexp.MustCompile(`"([^"]*)"="([^"]*)"`)

// askedLines are the lines of Claude's text that ask something: the questions it ends a turn on.
func askedLines(text string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasSuffix(strings.TrimRight(line, " *_)"), "?") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return strings.Join(out, "\n")
}

// briefFor is what the .prompt.md publishes: the given prompts verbatim and, after each, a line
// that Claude worked with the count of its tool calls. Claude's text is left out, except the
// questions it asked: an AskUserQuestion with its answer, and the questions it ended a turn on.
func (s *claudeSession) briefFor(prompts []int) string {
	keep := map[int]bool{}
	for _, n := range prompts {
		keep[n] = true
	}
	var b strings.Builder
	b.WriteString("Generated from a Claude Code session. Prompts are verbatim; Claude's work is counted,\n")
	b.WriteString("not shown, except the questions it asked. Names in [brackets] are redacted.\n")
	block := func(text, first, rest string) {
		b.WriteString("\n")
		for i, line := range strings.Split(text, "\n") {
			switch {
			case strings.TrimSpace(line) == "":
				b.WriteString("\n")
			case i == 0:
				b.WriteString(first + line + "\n")
			default:
				b.WriteString(rest + line + "\n")
			}
		}
	}

	n, segment := 0, 0
	var prompt, lastSay string
	var tools []string
	worked := false
	// flush writes the work since the prompt or the last question, if there was any.
	flush := func() {
		if worked {
			segment++
			block("Claude "+workWord(prompt, segment), "⏺ ", "  ")
			if len(tools) > 0 {
				b.WriteString("  ⎿  " + countLine(tools) + "\n")
			}
		}
		tools, worked = nil, false
	}
	// end closes a prompt's turn, with the questions Claude ended it on.
	end := func() {
		flush()
		if q := askedLines(lastSay); q != "" {
			block(q, "⏺ ", "  ")
		}
		lastSay = ""
	}
	for _, st := range s.steps {
		if st.kind == "prompt" {
			if keep[max(n, 1)] {
				end()
			}
			n++
			prompt, segment = st.text, 0
		}
		if !keep[max(n, 1)] {
			continue
		}
		switch {
		case st.kind == "prompt":
			block(st.text, "> ", "  ")
		case st.kind == "say":
			worked, lastSay = true, st.text
		case st.kind == "out":
			flush()
			lastSay = ""
			block(st.text, "  ⎿  ", "     ")
		case len(st.asks) > 0:
			flush()
			lastSay = ""
			block(strings.Join(st.asks, "\n"), "⏺ ", "  ")
			var got []string
			for _, m := range answer.FindAllStringSubmatch(s.outputs[st.toolID], -1) {
				got = append(got, m[2])
			}
			if len(got) > 0 {
				b.WriteString("  ⎿  " + strings.Join(got, "\n     ") + "\n")
			}
		default:
			worked, lastSay = true, ""
			tools = append(tools, st.tool)
		}
	}
	if keep[max(n, 1)] {
		end()
	}
	return b.String()
}

// redactions replace what must not be published. The built-in ones cover the home folder,
// email addresses and common secret formats; the file adds names, one per line.
type redactions struct {
	pairs []redaction
}

type redaction struct {
	find *regexp.Regexp
	with string
}

// redactFile is ~/.config/vibelog/redact.txt, outside any repo: the list itself names what must
// stay private.
func redactFile() string { return filepath.Join(configDir(), "redact.txt") }

func loadRedactions() (*redactions, error) {
	r := &redactions{}
	add := func(pattern, with string) {
		r.pairs = append(r.pairs, redaction{regexp.MustCompile(pattern), with})
	}
	add(`(?:ghp_|github_pat_|glpat-|gho_|xox[abp]-|sk-ant-|sk-)[A-Za-z0-9_\-]{16,}`, "[secret]")
	add(`AKIA[0-9A-Z]{16}`, "[secret]")
	add(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`, "[secret]")
	add(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`, "[email]")
	add(`/private/tmp/claude-\d+/\S*`, "[tmp]")

	var user []redaction
	if path := redactFile(); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for n, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			find, with, ok := strings.Cut(line, "=>")
			if !ok || strings.TrimSpace(find) == "" {
				return nil, fmt.Errorf("%s:%d: write it as `name => [replacement]`", path, n+1)
			}
			user = append(user, redaction{regexp.MustCompile(`(?i)` + regexp.QuoteMeta(strings.TrimSpace(find))), strings.TrimSpace(with)})
		}
	}
	// Longer names first, so "Acme Cloud" goes before "Acme".
	sort.SliceStable(user, func(i, j int) bool { return len(user[i].find.String()) > len(user[j].find.String()) })
	r.pairs = append(r.pairs, user...)

	if home, err := os.UserHomeDir(); err == nil {
		add(regexp.QuoteMeta(home), "~")
		add(regexp.QuoteMeta(nonAlnum.ReplaceAllString(home, "-")), "~")
	}
	return r, nil
}

func (r *redactions) apply(s string) string {
	for _, p := range r.pairs {
		s = p.find.ReplaceAllString(s, p.with)
	}
	return s
}

var dashes = regexp.MustCompile(`-+`)

func slugify(title string) string {
	s := strings.ToLower(title)
	s = nonAlnum.ReplaceAllString(s, "-")
	return strings.Trim(dashes.ReplaceAllString(s, "-"), "-")
}

const splitPrompt = `Below are the numbered prompts of one Claude Code session, each with the start of
Claude's reply. Group them into topics: one topic per subject a reader would want as a separate
note, such as building a product versus building a tool to log the work. Most sessions are one
topic; split only when the subjects stand apart. Every prompt belongs to exactly one topic. A
short follow-up ("do that", "push again") belongs with the prompt it follows up. Keep topics in
the order they start.

Answer with JSON only, no other text:
{"topics": [{"title": "working title", "prompts": [1, 2, 3]}]}
`

// splitTopics asks Claude which prompts belong together. Text in [brackets] is already redacted.
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

// writeNote asks Claude Code, without tools and without saving a session, for the note. The
// newest note on the site goes along as the example of the voice.
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
	// Drop a code fence around the whole note, if Claude added one.
	if strings.HasPrefix(note, "```") && strings.HasSuffix(note, "```") {
		_, note, _ = strings.Cut(note, "\n")
		note = strings.TrimSuffix(note, "```")
	}
	return strings.TrimSpace(note) + "\n", nil
}

// askClaude runs Claude Code headless, without tools and without saving a session, so a run
// doesn't become the newest session itself.
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

// newestNote is the newest note in the output folder, so every note sounds like the last one.
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
