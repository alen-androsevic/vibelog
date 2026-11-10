package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type redactions struct {
	pairs []redaction
}

type redaction struct {
	find *regexp.Regexp
	with string
}

func redactFile() string { return filepath.Join(configDir(), "redact.txt") }

func loadRedactions() (*redactions, error) {
	r := &redactions{}
	add := func(pattern, with string) {
		r.pairs = append(r.pairs, redaction{regexp.MustCompile(pattern), with})
	}
	add(`\b(?:ghp_|github_pat_|glpat-|gho_|xox[abp]-|sk-ant-|sk-)[A-Za-z0-9_\-]{16,}`, "[secret]")
	add(`\bAKIA[0-9A-Z]{16}`, "[secret]")
	add(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`, "[secret]")
	add(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`, "[email]")
	add(`(?:/private)?/tmp/claude-\d+/\S*`, "[tmp]")

	path := redactFile()
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var user []redaction
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
