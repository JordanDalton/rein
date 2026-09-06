# Safety and data handling

[Back to README](../README.md)

## Where data goes

- **Your machine:** tool discovery and command execution run locally. Capability
  maps and run archives are stored under `REIN_HOME` (default `~/.rein`).
- **Model backend:** the planner receives your intent, a capability digest, and
  command observations after output redaction and elision. A hosted backend sends
  these to its provider; a local endpoint keeps inference at that endpoint.
  Agent CLI backends use the selected CLI and its provider configuration.
- **Enterprise control plane:** enrollment exchanges device and account identity.
  Registered MCP callers send audit events containing caller, tool, and intent.
  Approval requests can additionally include command text and policy metadata.
  These payloads are separate from command-output redaction; avoid secrets in
  intent or command arguments. Cloud credentials use macOS Keychain when
  available; on Unix hosts without Keychain support, Rein uses the mode-0600
  `~/.rein/credentials.json` store. Protect the account and home directory.

## Execution safeguards

**No shell, ever.** The planner emits an argv array and rein `exec`s it
directly. There are no pipes, no globs, no `$VARS`, no `&&`. Rein does not interpret shell syntax, and `argv[0]` is checked against the
wrapped tool on every step — a plan naming any other binary is rejected and fed
back to the planner as a failed step.

**Two independent risk opinions.** The model labels each command
`safe` / `caution` / `danger`, and a static classifier
(`internal/risk`) does the same from the argv alone. The gate takes the
**higher** of the two, so a model that under-reports risk cannot talk its way
past the prompt. Unknown commands classify as `caution`, never `safe`. The
binary name is part of the verb path — wrapping `rm` means every invocation is
destructive, subcommand or not.

| mode | read-only | mutating | destructive |
|---|---|---|---|
| default | runs | asks | asks |
| `--yes` | runs | runs | asks |
| `--auto` | runs | runs | runs |

Destructive commands stop for a human under `--yes`, and with no terminal
available the loop fails closed rather than assuming consent.

**The gate explains itself.** A prompt is only a real check if the person
reading it can tell what they are agreeing to — which is not the same as being
able to read the command. So every mutating or destructive plan carries a
plain-language `consequence`, rendered above the prompt:

```
  $ git reset --hard 'HEAD~1'
  Discard the most recent commit (c483a21 "tweak alpha") · destructive

  !!  The most recent saved snapshot of your work, "tweak alpha", and every
      edit it contained will be erased from your project, leaving it as it was
      at "add beta module". Any uncommitted edits sitting in your files right
      now will also be wiped out. This cannot be undone.

  run this? [y]es / [n]o / [q]uit:
```

That warning names a consequence the command text does not: `--hard` also
discards uncommitted work, which you only know if you already know git. Read-only
commands get no warning, so the ones that appear still mean something, and a plan
that omits the field still runs — losing the command would be worse than losing
the warning.

**Reading a secret is not "safe".** Disclosure is its own axis. A read of a
file that conventionally holds credentials — `.env` and its variants, `*.pem`,
`*.key`, `id_rsa`, `.netrc`, `credentials`, and so on — classifies as
`caution` even under a read-only verb, so `cat .env` stops for a human in the
default mode with a warning that says where the contents will go. And whatever
any command prints is scanned before anyone sees it: values under names like
`TOKEN`, `PASSWORD`, `SECRET`, `API_KEY`, well-known token shapes (Slack,
GitHub, AWS, OpenAI/Anthropic, JWTs, passwords in URLs) and PEM private keys
are masked down to a short prefix and a length —
`SLACK_TOKEN=xoxb…[redacted, 32 chars]` — in the transcript sent to the
model and in the run log under `~/.rein/runs/`, which is written owner-only.
The prefix is kept so "which token is this?" is still answerable. The terminal
gets less still: when the command read a secret file, the contents are not
echoed at all, only a one-line note of how many lines and masked values the
model was shown. You asked a question about the file, not to see it.
This is a heuristic: an unlabelled secret in free text gets through, and a
harmless value under a suspicious name gets masked.

**CLIs are hostile to programs.** The runner forces `TERM=dumb`, `NO_COLOR`,
`PAGER=cat` and friends, strips ANSI escapes, and closes stdin so a tool that
decides to prompt fails fast instead of hanging. Long output is elided
head-and-tail — the head carries the column headers, the tail carries the
errors and totals, and the middle is rarely what you needed. The full,
untruncated output is archived under `~/.rein/runs/` for grepping.

**Prompt caching.** The system prompt and capability digest are byte-identical
on every step, so the API backend marks them cacheable: a multi-step run costs
one full-price request plus cheap reads.

## Known limits

- **Pipes only, no PTY.** Interactive tools (`psql`, `ssh`, anything curses)
  are out of scope. Adding a PTY means writing a terminal emulator to read the
  screen; worth doing only once the loop has proven itself.
- **Help-text parsing is heuristic.** The Cobra `__complete` path is exact; the
  prose parser handles Cobra, kubectl, and git shapes and will miss unusual
  layouts. `rein spec <tool> --show` shows what was actually learned.
- **Redaction is pattern-based.** It catches labelled values and well-known
  token shapes, not every secret. Treat the gate as the real protection and
  the masking as a second layer.
- **The risk classifier is a gate, not an oracle.** It is deliberately
  pessimistic, and it does not understand your tool's semantics. Don't run
  `--auto` anywhere you'd mind losing.

Rein governs operations routed through it. It does not prevent another program
or an agent with separate execution tools from running commands directly.
See [Enterprise setup](enterprise.md) for the scope of centralized controls.
