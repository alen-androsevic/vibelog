# vibelog

Turns a Claude Code session into notes you can publish: one per topic, each with the session beside it.

A log of your vibe-coding sessions, kept the way `git log` keeps commits.

```sh
go install .                      # from this folder; puts vibelog in ~/go/bin
vibelog ~/code/acme/api --dry-run # show the notes for that project's newest session
```

## What it writes

Claude splits the session into topics, such as building a product and building a tool. Each topic
gets two files:

- `DATE-SLUG.md`: a decision diary Claude drafts. What runs now, the choices and why, how Claude
  was used, what to tell someone starting the same, what's next. It copies the voice of the newest
  note in the folder.
- `DATE-SLUG.prompt.md`: that part of the session as the terminal printed it. Prompts are verbatim;
  tool output is cut to 6 lines.

It takes about 2 minutes, never overwrites a file and needs the `claude` command on your PATH.
Read both files before you publish.

## Where it reads and writes

- **Session:** a project folder (its newest session), a `.jsonl` file, or nothing for your newest
  session anywhere. Sessions come from `~/.claude/projects`, or `$CLAUDE_CONFIG_DIR/projects`.
- **Notes:** `--out DIR`, else `VIBELOG_OUT`, else `out = DIR` in `~/.config/vibelog/config`, else
  `./notes` when it exists, else the current folder.

## Keeping names private

Put names to hide in `~/.config/vibelog/redact.txt`, one per line:

```
acme corp => [client]
gitlab.acme.example => [gitlab]
```

Matching ignores case, and longer names go first. Your home folder, email addresses and common
secret formats (GitHub, GitLab, Slack, AWS, Anthropic keys and private keys) are always replaced.

## Flags

| Flag | Does |
|---|---|
| `--out DIR` | Folder for the notes |
| `--dry-run` | Print everything, write nothing |
| `--transcript-only` | One transcript for the whole session; no split, no draft |
| `--json` | Print the written files as JSON on stdout |
