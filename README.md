# rein

Wrap any CLI in an agentic loop. You type intent; rein figures out the
flags, runs the command, reads the output, and iterates.

```
$ rein in git "what changed in the last two commits, and who wrote them?"

── step 1/8 · thinking…
  $ git log -2 --stat '--pretty=format:%H%n%an <%ae>%n%ad%n%s%n%b'
  Show the last two commits with author, date, message, and changed-file stats · read-only
  exit 0 in 47ms
  │ c483a21… T <t@t> · tweak alpha
  │  a.txt | 2 +-
  …

Both commits were written by T <t@t>. c483a21 "tweak alpha" rewrote one line
in a.txt; 50fc6fa "add beta module" added a one-line b.txt.
```

This is the CLI-first prototype: no TUI yet, deliberately. If the loop doesn't
feel right in a plain terminal, a TUI won't save it.

## Install

You need Go 1.27+ and a model to plan with. By default the planner shells out
to your existing `claude` CLI, so if you already use Claude Code there is
nothing else to configure; otherwise see [Configuration](#configuration).

```bash
go install github.com/jordandalton/rein/cmd/rein@latest
```

That puts `rein` in `$(go env GOPATH)/bin` — usually `~/go/bin` — so make
sure that directory is on your `PATH`:

```bash
export PATH="$HOME/go/bin:$PATH"     # add to ~/.zshrc or ~/.bashrc
```

To hack on it instead, clone the repo and build into `bin/`, then symlink so
every rebuild is picked up without reinstalling:

```bash
git clone https://github.com/jordandalton/rein && cd rein
go build -o bin/rein ./cmd/rein
ln -s "$PWD/bin/rein" /usr/local/bin/rein
```

Check it worked:

```bash
rein in git "what changed in the last commit?"
```

The first run against any tool takes a few seconds while rein learns it (see
[The idea](#the-idea)); everything after that is instant.

## Model backends

The planner needs nothing but text in and text out — no tool-calling, no
provider-specific structured-output API — so any instruction-following model
works. `internal/planner.Backend` is a two-method interface.

**Agent CLIs** reuse credentials you already have, so there is no key to set:

```bash
rein in --backend claude-cli git "..."   # default
rein in --backend codex-cli  git "..."
rein in --backend grok-cli   git "..."
```

Each is pinned to the most restrictive mode it offers (`codex --sandbox
read-only`, `grok --max-turns 1 --disable-web-search`, and a replaced system
prompt for `claude`). The planner's job is to emit JSON, not to go exploring.

The `claude` backend holds one streaming session open for the whole run
instead of spawning a process per step, skips your configured MCP servers
and built-in tools, disables extended thinking, and defaults to Haiku —
picking the next argv from help text does not need an Opus-class model, and
Haiku answers in a fraction of the time. Pass `--model sonnet` (or `opus`)
for gnarlier tools.

Every one of those is a measured latency choice. A fresh `claude -p` process
costs ~4s before it makes its first API call, which is why the session is
persistent; the built-in tool schemas are ~26k tokens per request; and the
planner is only asked to explain its reasoning under `-v`, which is the only
time it is shown — dropping it saved ~0.7s per run.

**Hosted and local APIs** share one OpenAI-compatible chat-completions
implementation, so a preset is just a base URL plus a credential variable:

| `--backend` | endpoint | credential |
|---|---|---|
| `openai` | `api.openai.com/v1` | `OPENAI_API_KEY` |
| `openrouter` | `openrouter.ai/api/v1` | `OPENROUTER_API_KEY` |
| `xai` | `api.x.ai/v1` | `XAI_API_KEY` |
| `groq` | `api.groq.com/openai/v1` | `GROQ_API_KEY` |
| `deepseek` | `api.deepseek.com/v1` | `DEEPSEEK_API_KEY` |
| `mistral` | `api.mistral.ai/v1` | `MISTRAL_API_KEY` |
| `together` | `api.together.xyz/v1` | `TOGETHER_API_KEY` |
| `ollama` | `localhost:11434/v1` | none |
| `lmstudio` | `localhost:1234/v1` | none |
| `openai-compatible` | `--base-url` | `REIN_API_KEY` |
| `api` | Claude Messages API | `ANTHROPIC_API_KEY` / `ant auth login` |

```bash
rein in --backend openrouter --model x-ai/grok-4.6 git "..."
rein in --backend ollama --model llama3.2 git "..."
rein in --backend openai-compatible --base-url https://internal.corp/v1 \
            --api-key-env CORP_TOKEN --model our-model git "..."
```

`--model` is required for these: the right name depends on the endpoint, and
guessing produces a confusing 404 from the provider instead of a clear message
from here. Credentials resolve `--api-key-env` → `REIN_API_KEY` → the
provider's conventional variable. JSON mode and `temperature: 0` are requested
where supported, and endpoints that reject either are retried once without
them, so reasoning models and stricter gateways work with no extra flags.

**Adding a provider** that does not speak this wire format (Gemini's native
API, say) is one file implementing `Complete` and `Name`, plus a case in
`makeBackend`.

**On local models.** The capability digest is up to 24KB (~6k tokens) and is
part of every request (as a stable system-prompt prefix, so servers with
prefix caching reprocess it only once), so a small-context model will
struggle and prompt processing dominates the wall clock on CPU — raise `REIN_PLANNER_TIMEOUT` (default 5m)
if a model needs longer. Quality degrades in a specific way: smaller models
drift out of the plan schema. `ParsePlan` rejects a malformed plan and quotes
what came back rather than guessing, so the failure is loud. In testing,
`llama3.2` returned valid JSON that was not a plan, and `gemma4` proposed
running `grep` and a hallucinated `file_system_tool` — both were caught, the
latter by the argv[0] guard.

## Configuration

There is no config file. Everything is a flag or an environment variable, and
the defaults are chosen so that most people set nothing.

**Using Claude Code already?** You're done — the default `claude-cli` backend
reuses its login. The same goes for `codex-cli` and `grok-cli` if you have
those tools signed in.

**Using a hosted API?** Export the provider's usual key and name the backend
and model:

```bash
export OPENROUTER_API_KEY=sk-or-...           # put in ~/.zshrc to make it stick
rein in --backend openrouter --model anthropic/claude-sonnet-5 git "..."
```

Each preset reads the conventional variable from the table above
(`OPENAI_API_KEY`, `GROQ_API_KEY`, …). To point *every* preset at one
credential instead, set `REIN_API_KEY`; to read from some other variable —
say your company's `CORP_TOKEN` — pass `--api-key-env CORP_TOKEN`.

**Using the Claude Messages API directly?** `--backend api` looks for
`ANTHROPIC_API_KEY`, then falls back to an `ant auth login` profile on disk.

**Using a local model?** Nothing to export. Start Ollama or LM Studio and
pass `--model`:

```bash
rein in --backend ollama --model qwen2.5 git "..."
```

**Using a self-hosted or unlisted OpenAI-compatible endpoint?** Give the URL
and a key:

```bash
export REIN_BASE_URL=https://internal.corp/v1
export REIN_API_KEY=...
rein in --backend openai-compatible --model our-model git "..."
```

### Environment variables

| variable | purpose | default |
|---|---|---|
| `REIN_API_KEY` | credential for any hosted backend; outranks the provider's own variable | unset |
| `REIN_BASE_URL` | endpoint for `--backend openai-compatible` (same as `--base-url`) | unset |
| `REIN_PLANNER_TIMEOUT` | how long to wait for the model each step, e.g. `15m` for slow local inference | `5m` |
| `REIN_HOME` | where learned specs and run archives live | `~/.rein` |

Flags always win over environment variables. Provider variables
(`OPENAI_API_KEY` and friends) are only consulted when neither `--api-key-env`
nor `REIN_API_KEY` is set.

## Use

```bash
rein spec kubectl                  # learn the tool once, cache the result
rein in kubectl "which pods are crashlooping in staging?"
rein in gh "how many open PRs are assigned to me?"
rein list                          # what has been learned so far
```

The `in` is optional whenever the next word is a tool on your PATH, so the
short form works too:

```bash
rein ffmpeg "make a 3 second test video"
```

Rein's own commands (`spec`, `list`, `help`) always take precedence, so a
tool that happens to share one of those names needs the explicit `rein in`.

`rein in` discovers the tool automatically on first use, so `rein spec`
is only needed to pre-warm the cache or re-learn after an upgrade
(`rein in --refresh`).

Rein's own flags work before or after the intent. Any *other* dashed word
stays inside the intent, so you can ask about the wrapped tool's flags without
them being parsed away:

```bash
rein in --auto gh "sync my forks"
rein in gh "sync my forks" --auto            # identical
rein in gh "which commands support --json?"  # --json stays in the intent
rein in gh -- "what does --auto do?"         # "--" ends flag parsing
```

## Use from an agent (MCP)

Coding agents are bad at unfamiliar CLIs for exactly the reason rein exists.
`rein mcp` serves rein over the Model Context Protocol on stdio, so an agent
can hand a tool to rein instead of guessing at its flags:

```bash
claude mcp add rein -- rein mcp          # Claude Code
codex mcp add rein -- rein mcp           # Codex
```

Any other client takes the same shape in its config file:

```json
{ "mcpServers": { "rein": { "command": "rein", "args": ["mcp"] } } }
```

Three tools are exposed. `rein_in` takes a `tool` and an `intent` and returns
the answer plus a transcript of every command run. `rein_list` shows what has
been learned. `rein_spec` shows a tool's subcommands, for "can this tool even
do that?" questions.

Runs are headless, so the approval gate cannot ask anyone. Instead the
caller passes an `approval` level (`safe`, `yes`, `auto`, meaning the same as
the flags) and the server enforces a ceiling set at startup:

```bash
rein mcp              # read-only commands only (default)
rein mcp --yes        # callers may also be granted mutating commands
rein mcp --auto       # callers may be granted anything; sandboxes only
```

A run that reaches a command above its granted level stops *before* running
it and reports `status: needs-approval` with the transcript, so the calling
agent can ask its user and retry with a higher level. A request above the
server's ceiling is refused outright; the agent is told to ask the user
rather than retry. A planner question comes back as `status: needs-input` for
the same reason.

The backend flags (`--backend`, `--model`, and friends) work on `rein mcp`
too and apply to every call. With the default `claude-cli` backend inside
Claude Code that means Claude Code calling rein calling `claude -p`, which is
fine: the planner runs as a bare Haiku completion with no tools of its own.

## The idea

The interesting part isn't the loop — it's the **capability map**.

A model has never seen your company's internal `acme-deploy` binary. So before
planning anything, rein teaches itself the tool: it crawls `--help`
recursively, and for Cobra-based CLIs it asks the binary directly via its
hidden `__complete` endpoint, which is machine-readable and beats parsing
prose. The result is cached in `~/.rein/specs/<tool>.json`, keyed to the
tool version. Discovery costs a few seconds, once. Every run after that is
free.

That cached map is what makes a *per-CLI* wrapper different from a general
coding agent: a small, durable, shareable description of one tool, including
the ones no model has ever heard of.

## Design notes

**No shell, ever.** The planner emits an argv array and rein `exec`s it
directly. There are no pipes, no globs, no `$VARS`, no `&&`. Quoting bugs
therefore cannot become command injection, and `argv[0]` is checked against the
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

**CLIs are hostile to programs.** The runner forces `TERM=dumb`, `NO_COLOR`,
`PAGER=cat` and friends, strips ANSI escapes, and closes stdin so a tool that
decides to prompt fails fast instead of hanging. Long output is elided
head-and-tail — the head carries the column headers, the tail carries the
errors and totals, and the middle is rarely what you needed. The full,
untruncated output is archived under `~/.rein/runs/` for grepping.

**Prompt caching.** The system prompt and capability digest are byte-identical
on every step, so the API backend marks them cacheable: a multi-step run costs
one full-price request plus cheap reads.

## Layout

```
cmd/rein           CLI entry point
internal/spec      capability discovery + cache      (help crawl, __complete)
internal/planner   model backends + plan schema      (claude CLI, Messages API)
internal/risk      static argv risk classifier
internal/runner    sanitised exec + output elision
internal/loop      plan → gate → execute → observe
```

## Known limits

- **Pipes only, no PTY.** Interactive tools (`psql`, `ssh`, anything curses)
  are out of scope. Adding a PTY means writing a terminal emulator to read the
  screen; worth doing only once the loop has proven itself.
- **Help-text parsing is heuristic.** The Cobra `__complete` path is exact; the
  prose parser handles Cobra, kubectl, and git shapes and will miss unusual
  layouts. `rein spec <tool> --show` shows what was actually learned.
- **The risk classifier is a gate, not an oracle.** It is deliberately
  pessimistic, and it does not understand your tool's semantics. Don't run
  `--auto` anywhere you'd mind losing.

## Next

- TUI (Bubbletea): transcript pane, live command pane, approval bar.
- A spec registry — `rein pull acme-deploy` — so a team writes the map for
  its internal tool once.
- Learn from runs: keep the argv that satisfied an intent as an example for
  the next time a similar one comes in.
- Model-authored spec refinement on first wrap, checked in alongside the crawl.
