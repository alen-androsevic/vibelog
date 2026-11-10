package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const outputLines = 6

var skippedTools = map[string]bool{"ToolSearch": true, "TodoWrite": true}

type output struct {
	title, body, transcript string
	note, prompt            string
}

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

func findSession(arg string) (string, error) {
	root := projectsDir()
	if strings.HasSuffix(arg, ".jsonl") {
		arg = expandHome(arg)
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

type step struct {
	kind   string
	text   string
	toolID string
	tool   string
	asks   []string
}

type claudeSession struct {
	started  time.Time
	promptAt []time.Time
	aiTitle  string
	steps    []step
	outputs  map[string]string
	prompts  int
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
	s.promptAt = append(s.promptAt, e.Timestamp)
	s.steps = append(s.steps, step{kind: "prompt", text: text})
}

func (s *claudeSession) startOf(prompts []int) time.Time {
	var at time.Time
	for _, n := range prompts {
		if n >= 1 && n <= len(s.promptAt) && (at.IsZero() || s.promptAt[n-1].Before(at)) {
			at = s.promptAt[n-1]
		}
	}
	if at.IsZero() {
		return s.started
	}
	return at
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

func workWord(prompt string, n int) string {
	h := fnv.New32a()
	fmt.Fprintf(h, "%d %s", n, prompt)
	return workWords[h.Sum32()%uint32(len(workWords))]
}

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

func askedLines(text string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasSuffix(strings.TrimRight(line, " *_)"), "?") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return strings.Join(out, "\n")
}

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

var dashes = regexp.MustCompile(`-+`)

func slugify(title string) string {
	s := strings.ToLower(title)
	s = nonAlnum.ReplaceAllString(s, "-")
	return strings.Trim(dashes.ReplaceAllString(s, "-"), "-")
}
