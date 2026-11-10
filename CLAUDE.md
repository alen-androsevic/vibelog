# vibelog

Go CLI, standard library only. README.md covers what it does, the flags and the transcript format.

- `main.go`: flags, the output folder, naming and writing the files.
- `session.go`: reading the session JSONL and the transcript format.
- `redact.go`: the built-in redactions and `~/.config/vibelog/redact.txt`.
- `claude.go`: the two calls to `claude -p` (split into topics, draft a note).

The code has no comments; names and tests carry the meaning. Keep it that way.

The `.prompt.md` format is also read by the note viewer on https://alen.andronewille.eu, the
author's site. Change it in both places or not at all.

A release gets a CHANGELOG.md entry, `version` in `main.go` to match, a `vX.Y.Z` tag and a GitHub
release with the entry as its notes.

## Commands

- `go install .`: build and install to `~/go/bin/vibelog`.
- `go run . PROJECT --dry-run`: try it on a real session without writing.
- `go test ./...`: the transcript format and redaction.

## MIG

MIG is the author's own guideline for UI and copy; contributors can skip this section.

Skin: alen (terminal: no colour)

Exceptions: none yet.
