package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const version = "0.1.0"

const usage = `vibelog turns a Claude Code session into notes, one per topic, with the
session beside each.

Examples:
  vibelog ~/code/acme/api              Notes from that project's newest session
  vibelog --dry-run                    Show the notes for your newest session,
                                       write nothing
  vibelog ~/code/acme/api --out notes  Write them into ./notes

Usage:
  vibelog [PROJECT | SESSION.jsonl] [--out DIR] [--dry-run] [--transcript-only]
          [--json]

Flags:
  --out DIR          Folder for the notes. Default: $VIBELOG_OUT, else "out" in
                     ~/.config/vibelog/config, else ./notes when it exists,
                     else the current folder
  --dry-run          Print the notes and transcripts, write nothing
  --transcript-only  Write one transcript for the whole session; no split, no
                     draft
  --json             Print the written files as JSON on stdout
  -h, --help         Show this help
  --version          Show the version

Each topic becomes two files, named for the day and time of its first prompt:
  DATE-HHMM-SLUG.md         A decision diary Claude drafts, in the voice of the
                            newest note in the folder
  DATE-HHMM-SLUG.prompt.md  Its part of the session: your prompts verbatim,
                            Claude's work as a count of its tool calls, and the
                            questions it asked you

vibelog never overwrites a file. The drafts need the claude command on your
PATH, and the redacted session goes to Claude through it.

Names to hide go in ~/.config/vibelog/redact.txt, one "name => [replacement]"
per line. Your home folder, email addresses and common secret formats are
always replaced.
`

var noteName = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})-([a-z0-9]+(?:-[a-z0-9]+)*)\.md$`)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	flags := flag.NewFlagSet("vibelog", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	outFlag := flags.String("out", "", "")
	dryRun := flags.Bool("dry-run", false, "")
	transcriptOnly := flags.Bool("transcript-only", false, "")
	asJSON := flags.Bool("json", false, "")
	showVersion := flags.Bool("version", false, "")
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Print(usage)
			return 0
		}
	}
	if err := flags.Parse(reorder(args)); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	if flags.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "vibelog: give one project folder or session file, or none for your newest session.")
		return 2
	}
	out, err := outDir(*outFlag)
	if err != nil {
		report(err)
		return 1
	}
	file, err := findSession(flags.Arg(0))
	if err != nil {
		report(err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "Session: %s\n", file)

	sess, err := readSession(file)
	if err != nil {
		report(err)
		return 1
	}
	red, err := loadRedactions()
	if err != nil {
		report(err)
		return 1
	}
	topics := pickTopics(sess, red, *transcriptOnly)
	outs, err := draftNotes(out, sess, red, topics, *transcriptOnly)
	if err != nil {
		report(err)
		return 1
	}
	nameFiles(out, sess, red, topics, outs)

	if *dryRun {
		printAll(outs)
		return 0
	}
	targets, err := writeFiles(out, outs)
	if err != nil {
		report(err)
		return 1
	}
	if *asJSON {
		var notes []map[string]any
		for _, o := range outs {
			notes = append(notes, map[string]any{"note": o.note, "prompt": o.prompt, "noteWritten": o.body != ""})
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"session": file, "notes": notes})
		return 0
	}
	for _, p := range targets {
		fmt.Println(p)
	}
	fmt.Fprintln(os.Stderr, "Read them before you publish: check names, numbers and the summary lines.")
	return 0
}

func pickTopics(sess *claudeSession, red *redactions, transcriptOnly bool) []topic {
	topics := []topic{{title: sess.aiTitle, prompts: sess.allPrompts()}}
	if transcriptOnly {
		return topics
	}
	fmt.Fprintln(os.Stderr, "Splitting the session into topics with Claude.")
	if split, err := splitTopics(sess, red); err != nil {
		fmt.Fprintf(os.Stderr, "Keeping one note: %v\n", err)
	} else {
		topics = split
	}
	fmt.Fprintf(os.Stderr, "Writing %d note(s) with Claude; this takes about 2 minutes.\n", len(topics))
	return topics
}

func draftNotes(out string, sess *claudeSession, red *redactions, topics []topic, transcriptOnly bool) ([]output, error) {
	outs := make([]output, len(topics))
	errs := make([]error, len(topics))
	var wg sync.WaitGroup
	for i, t := range topics {
		outs[i] = output{title: t.title, transcript: red.apply(sess.briefFor(t.prompts))}
		if transcriptOnly {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			note, err := writeNote(out, red.apply(sess.transcriptFor(t.prompts)))
			if err != nil {
				errs[i] = err
				return
			}
			note = red.apply(note)
			first, rest, _ := strings.Cut(note, "\n")
			title, ok := strings.CutPrefix(first, "# ")
			if !ok || strings.TrimSpace(title) == "" {
				errs[i] = errors.New("Claude's note doesn't start with \"# Its title\"; run again, or run with --transcript-only and write it yourself")
				return
			}
			outs[i].title, outs[i].body = strings.TrimSpace(title), first+"\n"+rest
		}()
	}
	wg.Wait()
	return outs, errors.Join(errs...)
}

func nameFiles(out string, sess *claudeSession, red *redactions, topics []topic, outs []output) {
	used := map[string]bool{}
	for i := range outs {
		slug := slugify(strings.NewReplacer("[", "", "]", "").Replace(red.apply(outs[i].title)))
		if slug == "" {
			slug = "session"
		}
		base := slug
		for n := 2; used[slug]; n++ {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		used[slug] = true
		stamp := sess.startOf(topics[i].prompts).Local().Format(time.DateOnly + "-1504")
		outs[i].note = filepath.Join(out, stamp+"-"+slug+".md")
		outs[i].prompt = filepath.Join(out, stamp+"-"+slug+".prompt.md")
	}
}

func printAll(outs []output) {
	for _, o := range outs {
		if o.body != "" {
			fmt.Printf("==> %s\n%s\n", o.note, o.body)
		}
		fmt.Printf("==> %s\n%s\n", o.prompt, o.transcript)
	}
}

func writeFiles(out string, outs []output) ([]string, error) {
	var targets []string
	for _, o := range outs {
		targets = append(targets, o.prompt)
		if o.body != "" {
			targets = append(targets, o.note)
		}
	}
	for _, p := range targets {
		if _, err := os.Stat(p); err == nil {
			return nil, fmt.Errorf("%s already exists, so nothing was written. Move it aside, or check the output first with --dry-run", p)
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	for _, o := range outs {
		if err := os.WriteFile(o.prompt, []byte(o.transcript), 0o644); err != nil {
			return nil, err
		}
		if o.body == "" {
			fmt.Fprintf(os.Stderr, "Write %s yourself.\n", o.note)
			continue
		}
		if err := os.WriteFile(o.note, []byte(o.body), 0o644); err != nil {
			return nil, err
		}
	}
	return targets, nil
}

func reorder(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--out" || a == "-out":
			flags = append(flags, a)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
		default:
			rest = append(rest, a)
		}
	}
	return append(flags, rest...)
}

func configDir() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "vibelog")
}

func outDir(flagValue string) (string, error) {
	if flagValue != "" {
		return expandHome(flagValue), nil
	}
	if env := os.Getenv("VIBELOG_OUT"); env != "" {
		return expandHome(env), nil
	}
	path := filepath.Join(configDir(), "config")
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "out" {
			return "", fmt.Errorf("%s:%d: vibelog only knows `out = DIR`", path, n+1)
		}
		return expandHome(strings.TrimSpace(value)), nil
	}
	if info, err := os.Stat("notes"); err == nil && info.IsDir() {
		return "notes", nil
	}
	return ".", nil
}

func report(err error) {
	for _, line := range strings.Split(err.Error(), "\n") {
		fmt.Fprintln(os.Stderr, "vibelog: "+line)
	}
}
