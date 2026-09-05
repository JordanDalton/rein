# Model backends and configuration

[Back to README](../README.md)

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

Planner settings use flags and environment variables. Learned specs, run archives,
and optional Cloud profile and policy files live under `REIN_HOME`.

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
| `REIN_CONTROL_URL` | Rein Cloud control plane for `login` and `status` | `https://reincontrol.com` |

Flags always win over environment variables. Provider variables
(`OPENAI_API_KEY` and friends) are only consulted when neither `--api-key-env`
nor `REIN_API_KEY` is set.
