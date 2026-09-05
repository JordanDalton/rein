# Rein Gateway

Rein Gateway is one persistent local process that serves Rein MCP sessions for
multiple registered agents. Agent hosts launch a small stdio bridge instead of a
separate planner and policy process:

```text
Codex ───────┐
Claude Code ─┼─ rein gateway connect ─ local Unix socket ─ Rein Gateway
Other host ──┘                                      │
                                                   ├─ policy and approval
                                                   ├─ local CLI execution
                                                   └─ Control audit events
```

The bridge sends the registered caller name and its current working directory.
The gateway validates both before starting MCP, loads that agent's dedicated
credential from secure storage, and carries the directory into command execution
and approval/audit records. Rein Control derives the caller from the credential,
so a client cannot claim a different agent identity. Each MCP connection has
isolated request cancellation and tool state while sharing the daemon lifecycle.

## Start and manage the gateway

Gateway connections require a Rein Control login and a registered caller. The
normal interactive setup starts the gateway and configures the selected host:

```sh
rein configure codex
# or
rein configure claude-code
```

Manage the process directly with:

```sh
rein gateway start
rein gateway status
rein gateway stop
```

`start` launches a detached process and writes owner-only logs to
`~/.rein/gateway.log`. The Unix socket is `~/.rein/gateway.sock`, is mode `0600`,
and sits inside the current user's Rein state directory. The daemon is not yet
supported on Windows.

Planner and command limits belong to the daemon rather than an individual host:

```sh
rein gateway start --backend ollama --model qwen2.5 --timeout 90s
```

Stop and restart the gateway to change those options. Its environment and PATH
are captured when it starts, so restart after changing credentials, PATH, or
installed tools.

## Explicit harness configuration

To preview or install gateway-backed configuration without interactive setup:

```sh
rein gateway start
rein agent register codex
rein configure codex --gateway --persistent --dry-run
rein configure codex --gateway --persistent --apply
```

The installed MCP command is:

```sh
rein gateway connect --agent codex
```

`connect` is a protocol bridge intended for MCP hosts. It does not run a second
gateway or planner.

## Security boundary

The gateway socket accepts connections only from processes running as the same
OS user. It also requires the caller name to match a locally registered Rein
agent. This prevents access from other local users, but an untrusted process
already running as the enrolled user can still impersonate another registered
caller.

The gateway strengthens lifecycle management and centralizes Rein execution. It
does not turn editable project settings into an operating-system security
boundary. For protected production access, keep credentials and resource network
paths out of the agent process and make them available only to an
administrator-controlled Rein runtime. Continue to verify native tools, other MCP
servers, plugins, hooks, and delegated-agent paths after every host upgrade.

The existing `rein mcp` command remains available for standalone and legacy
stdio configurations.
