# Use from an agent (MCP)

[Back to README](../README.md)

For registered Rein Control agents, the preferred setup uses the persistent
[Rein Gateway](gateway.md). The existing `rein mcp` command remains the direct
stdio transport and works without a gateway.

After guided gateway setup, users can ask the agent for work normally; prompts
do not need to name Rein or MCP. Host restrictions and installed project
instructions are responsible for routing supported operations.

For preview-first Claude Code and Codex setup with restricted launch profiles,
start with [Harness configuration](harness-configuration.md): `rein configure`.
Adding MCP alone does not remove the host's native execution tools.

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

For registered callers, organization policies, and central approvals, see
[Enterprise setup](enterprise.md).
