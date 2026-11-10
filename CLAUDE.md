# vibelog

A CLI that turns a Claude Code session into publishable notes, one per topic, each with a
`.prompt.md` transcript. Pure Go, no dependencies.

- `main.go`: flags, the output folder, writing the files.
- `session.go`: reading the session JSONL, the transcript format, redaction, and the two calls to
  `claude -p` (split into topics, draft a note).

The `.prompt.md` format is what the note viewer on https://alen.andronewille.eu, the author's own
site, reads: `> ` a prompt, `⏺ ` Claude's text or a tool call, `⎿` its output, indented lines
continue the block above. Change it in both places or not at all.

## Commands

- `go install .`: build and install to `~/go/bin/vibelog`.
- `vibelog PROJECT --dry-run`: try it on a real session without writing.
- `go test ./...`: the transcript format and redaction.

## MIG

MIG is the author's own guideline for UI and copy; contributors can skip this section.

Skin: alen (terminal: no colour)

Exceptions: none yet.
