# vibelog

Turns a Claude Code session into notes you can publish: one per topic, each with the session beside it.

A log of your vibe-coding sessions, kept the way `git log` keeps commits.

```sh
go install github.com/alen-androsevic/vibelog@latest  # puts vibelog in ~/go/bin
vibelog ~/code/acme/api --dry-run                     # show the notes for that project's newest session
```

It needs Go 1.22 or newer, and [Claude Code](https://claude.com/claude-code) for the drafts. From
a clone, `go install .` does the same.

## What it writes

Claude splits the session into topics, such as building a product and building a tool. Each topic
gets two files, named for the day and time of its first prompt (`2026-11-09-1430-a-slug`), so a
folder lists the notes of one day in order:

- `DATE-HHMM-SLUG.md`: a decision diary Claude drafts. What runs now, the choices and why, how Claude
  was used, what to tell someone starting the same, what's next. It copies the voice of the newest
  note in the folder.
- `DATE-HHMM-SLUG.prompt.md`: that part of the session, kept short. Your prompts are verbatim;
  Claude's work shows as a line like "Claude pondering" with a count of its tool calls ("14 tool
  calls: 6 commands, 4 reads, 3 edits, 1 web search"). Its text is left out, except the questions
  it asked you. The draft is still written from the full session.

It takes about 2 minutes and never overwrites a file. Read both files before you publish.

## What goes to Claude

vibelog runs `claude -p` once to split the session into topics, then once per topic to draft its
note. The split gets your redacted prompts and the start of each reply; a draft gets the redacted
part of the session and, as an example of the voice, the newest note in the output folder. The
calls run without tools and aren't saved as sessions. vibelog sends nothing else and has no
telemetry. `--transcript-only` makes no calls.

## Where it reads and writes

- **Session:** a project folder (its newest session), a `.jsonl` file, or nothing for your newest
  session anywhere. Sessions come from `~/.claude/projects`, or `$CLAUDE_CONFIG_DIR/projects`.
- **Notes:** `--out DIR`, else `VIBELOG_OUT`, else `out = DIR` in `~/.config/vibelog/config`, else
  `./notes` when it exists, else the current folder.

## Keeping names private

Put names to hide in `~/.config/vibelog/redact.txt`, one per line. The list lives outside your
repos, because it names what must stay private.

```
acme corp => [client]
gitlab.acme.example => [gitlab]
```

Matching ignores case, and longer names go first. Your home folder, email addresses and common
secret formats (GitHub, GitLab, Slack, AWS, Anthropic and OpenAI keys, and private keys) are always
replaced. Redaction runs before anything goes to Claude.

## Flags

| Flag | Does |
|---|---|
| `--out DIR` | Folder for the notes |
| `--dry-run` | Print everything, write nothing |
| `--transcript-only` | One transcript for the whole session; no split, no draft |
| `--json` | Print the written files as JSON on stdout |
| `--version` | Print the version |

## Transcript format

A `.prompt.md` is plain text that reads like the Claude Code terminal:

```
> a prompt, verbatim

⏺ Claude pondering
  ⎿  14 tool calls: 6 commands, 4 reads, 3 edits, 1 web search

⏺ A question Claude asked?
  ⎿  Your answer
```

`> ` starts a prompt, `⏺ ` starts Claude's work or a question, `⎿` starts its result, and indented
lines continue the block above.

## Uninstall

```sh
rm ~/go/bin/vibelog
rm -r ~/.config/vibelog  # your config and redaction list, if you made them
```

## Contributing

`go test ./...` covers the transcript format and redaction. Try a change on a real session with
`go run . PROJECT --dry-run`, which writes nothing.

## License

MIT, see [LICENSE](LICENSE).
