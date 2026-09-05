# Rein

Turn intent into CLI commands. Rein learns your tools, plans commands, checks
their risk, and iterates on the output.

Use the open-source CLI on your machine, or connect to **Rein Enterprise** for
organization policies, approval workflows, and audit visibility through Rein Control.

```bash
rein in git "what changed in the last two commits, and who wrote them?"
rein in kubectl "which pods are crashlooping in staging?"
rein in gh "how many open PRs are assigned to me?"
```

Rein discovers commands and flags from the installed tool, so it can work with
internal CLIs a model has never seen. You see each proposed command and its
output; commands classified as mutating or destructive ask for approval by default.

## Open source and Enterprise

The CLI works without a Rein Control account. Enterprise connects that execution
layer to shared controls for teams and their agents.

| | Open-source CLI | Enterprise through Rein Control |
|---|---|---|
| Tool discovery | Local capability maps and caching | Uses the same CLI foundation |
| Models | Agent CLIs, hosted APIs, and local endpoints | Uses the same backend choices |
| Execution safeguards | Risk classification and local approval prompts | Organization policies for MCP operations |
| Agent integration | MCP server | Named caller registration and team profiles |
| Approvals | Terminal prompts and MCP approval ceilings | Central approval requests for registered MCP callers |
| Run visibility | Local transcripts and archives | Central audit events for registered MCP callers |

This repository contains the CLI and its Cloud integration. The enterprise
control plane is a separate service. See [Rein Enterprise](https://reincontrol.com)
and the [enterprise setup guide](docs/enterprise.md) for the connection flow and
current integration limits.

## Quickstart

You need **Go 1.27+**, the tool you want to run on your `PATH`, and a model backend.
The default backend uses an installed, signed-in Claude Code CLI.

```bash
go install github.com/jordandalton/rein/cmd/rein@latest
export PATH="$HOME/go/bin:$PATH"
rein in git "what changed in the last commit?"
```

The install directory is `$(go env GOPATH)/bin`, usually `~/go/bin`. Add it to your
shell profile if needed. Run the Git example inside a Git repository.

Already use another supported agent CLI? Select it explicitly:

```bash
rein in --backend codex-cli git "what changed in the last commit?"
rein in --backend grok-cli git "what changed in the last commit?"
```

For a local model, start Ollama with your chosen model available:

```bash
rein in --backend ollama --model qwen2.5 git "summarize recent work"
```

Hosted APIs and custom endpoints are supported too. See
[model backends and configuration](docs/configuration.md).
Model-provider charges may apply; a Rein Control account is not required for local use.

## Everyday use

```bash
rein git "summarize recent work"                    # `in` is optional
rein in --dry-run git "create a branch called demo" # preview the first command
rein spec kubectl                                  # discover commands ahead of time
rein spec kubectl --show                           # inspect the capability map
rein list                                         # list learned tools
rein in --refresh gh "list my open pull requests"   # rediscover after an upgrade
```

Discovery runs automatically on first use and is cached by tool version.
Subsequent runs reuse the map; planning and execution still take time.
See the [CLI usage guide](docs/usage.md) for flag placement and approval options.

## Use with an agent

Rein exposes tool discovery and execution over MCP. For a registered Control
agent, run guided setup to install the persistent Gateway bridge:

```bash
rein configure codex
# or: rein configure claude-code
```

For a standalone local server, configure your MCP client to launch `rein mcp`:

```json
{ "mcpServers": { "rein": { "command": "rein", "args": ["mcp"] } } }
```

The default server permits only commands classified as read-only. Operations
requiring higher approval return a structured outcome to the calling agent.
See [MCP setup and approval levels](docs/mcp.md).

## Connect your organization

For an existing Rein Control organization, enroll your installation and download
its policy bundle:

```bash
rein login
rein status
rein sync
```

Cloud enrollment currently requires **macOS Keychain** for credential storage.
For named MCP callers and central approvals, follow the
[enterprise setup guide](docs/enterprise.md).

For guided project setup, run `rein configure claude-code` or `rein configure codex` in an
interactive terminal. It previews changes and asks for confirmation, guides login
if needed, registers the caller, starts the persistent local Rein Gateway, installs persistent MCP/settings with backups,
and saves the project launch profile (the same operation as `--apply`),
and verifies the MCP handshake without running tools or making model requests.
If a previously installed file is missing, guided setup offers to restore it
from its receipt after confirmation. Existing edited files are never overwritten,
and the original undo backups are preserved.
Both normal harness launches and `rein configure HOST --launch` are supported.
To undo both, run `rein configure HOST --persistent --undo`, then
`rein configure HOST --undo`. A conflicting saved launch profile is not replaced
automatically; undo it before changing its settings.
If verification fails, setup reports failure and leaves installed settings
recoverable with `rein configure claude-code --persistent --undo`.
The installed MCP bridge uses `rein gateway connect --agent HOST`; agent sessions
share the gateway daemon instead of launching a separate Rein MCP runtime. Manage
it with `rein gateway status`, `rein gateway stop`, and `rein gateway start`.
Then start `claude`, trust the project settings/MCP, inspect `/mcp` and `/hooks`,
and verify native-tool blocking. For Codex, start `codex` instead; setup checks
hook support and installs `.codex/config.toml`. Codex coverage remains partial:
trust the project configuration and hooks, and verify restrictions in-session.
Connectivity is not proof of runtime enforcement.

To require cached specs for your intended tools during guided setup, use
`rein configure codex --require-spec cat,git` (also supported for `claude-code`).
Add `--persistent --check` for a noninteractive configuration/spec check.
Missing specs fail with an explicit trusted-operator `rein spec TOOL` instruction;
setup never discovers tools automatically. Governed MCP records policy/spec and
authorization blocks as non-executed Activity events. If audit delivery fails,
the operation stays blocked and the error reports that Activity could not be updated.

Alternatively, preview and save a gateway-backed restricted launch profile
without rewriting existing Claude Code or Codex settings:

```bash
rein gateway start
rein agent register claude-code
rein configure claude-code --gateway --dry-run
rein configure claude-code --gateway --apply
rein configure claude-code --launch
```

See [harness configuration, coverage limits, and undo](docs/harness-configuration.md).
See also the [runtime validation results and test command](docs/harness-runtime-validation.md).
Restrictions apply only to this launcher; Codex coverage is explicitly partial.

For persistent project settings on macOS/Linux, use
`rein configure claude-code --gateway --persistent --dry-run`, then
`--gateway --persistent --apply` after starting the Gateway.
Replace `claude-code` with `codex` for Codex. This installs a default-deny tool
hook plus Rein MCP registration for normal project launches, with backups,
`--persistent --check`, and `--persistent --undo`. Review/trust the hooks in a
fresh session and run the guide's bypass tests before rollout. Codex coverage
remains partial; editable project hooks are not an administrator-enforced boundary.

## GitHub Actions (beta)

Use the [GitHub Actions guide](docs/github-actions.md) for OIDC workload
authentication and policy-governed commands through `rein ci check` and
`rein ci run -- TOOL ARGS`. Includes an authentication action, pinned-version
workflow template, approvals, and audit reporting. This is not runner isolation.

## Safety and data handling

Rein executes argument arrays directly and checks the target binary on every
step. A model assessment and a static classifier determine the approval level;
the higher risk assessment wins.

| Local approval mode | Classified read-only | Mutating | Destructive |
|---|---|---|---|
| Default | Runs | Asks | Asks |
| `--yes` | Runs | Runs | Asks |
| `--auto` | Runs | Runs | Runs |

Execution happens on your machine. Intent, tool capabilities, and redacted
command observations go to your selected model backend. Connected, registered
MCP callers also send audit and approval metadata to Rein Control.

Risk classification and redaction are heuristic. Rein is not an OS sandbox, and
controls apply to operations routed through it. Interactive tools requiring a
PTY are not supported. Read [safety, data flow, and limitations](docs/security.md)
before configuring unattended execution.

## Documentation

- [CLI usage](docs/usage.md)
- [Model backends and configuration](docs/configuration.md)
- [MCP integration](docs/mcp.md)
- [Persistent local gateway](docs/gateway.md)
- [Enterprise setup](docs/enterprise.md)
- [Safety and data handling](docs/security.md)
- [Architecture and capability discovery](docs/architecture.md)
- [Contributing](CONTRIBUTING.md)

## Contributing and license

Bug reports, CLI compatibility examples, and pull requests are welcome. See
[Contributing](CONTRIBUTING.md) for local build and validation commands.

A license file has not yet been added to this repository. Explicit open-source
license terms still need to be published.
