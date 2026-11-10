# Changelog

## 0.1.0 (2026-11-10)

First release.

- Turns a Claude Code session into notes, one per topic: a decision diary Claude drafts, and a
  short `.prompt.md` transcript with your prompts, a count of Claude's tool calls and the questions
  it asked you.
- Reads a project's newest session, a `.jsonl` file, or your newest session anywhere.
- Redacts your home folder, email addresses, common secret formats and the names in
  `~/.config/vibelog/redact.txt` before anything goes to Claude.
- `--dry-run` prints everything and writes nothing; `--transcript-only` makes no calls to Claude;
  `--json` prints the written files.
- Never overwrites a file: if one exists, nothing is written.
