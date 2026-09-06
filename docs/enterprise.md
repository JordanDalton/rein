# Enterprise setup

[Back to README](../README.md)

Rein Enterprise adds shared controls through Rein Control. This repository ships
client integration; the control plane is a separate service. Organization access
is required. Visit [Rein Enterprise](https://reincontrol.com) for the offering.

## Enroll an installation

Cloud credential storage currently supports macOS Keychain only.

```bash
rein login
rein status
rein sync
```

Login opens the browser to enroll the device. `status` shows its Cloud identity;
`sync` downloads the organization policy bundle to `~/.rein/policy.json`.
Set `REIN_CONTROL_URL` or use `rein login --control-url URL` to select a different
control endpoint provided by your organization.

### Login from an SSH session

When Rein runs on a remote OpenClaw, Hermes, or other agent server, use remote
mode so Rein prints the exact callback port and tunnel command instead of trying
to open a browser on the server:

```bash
rein login --remote --ssh-target user@agent-server
```

Keep Rein running, execute the printed `ssh -N -L ...` command on your local
machine, and open the printed URL locally. You can reserve a known callback
port with `--callback-port 45943` if your SSH workflow requires it.

## Connect a named MCP caller

Guided setup registers the caller, starts the local Gateway, installs the host
configuration, and verifies its MCP handshake:

```bash
rein configure claude-code
# or: rein configure codex
```

For manual setup, register the caller and start the Gateway before configuring
the bridge:

```bash
rein agent register claude-code
rein gateway start
```

Then configure your MCP client:

```json
{
  "mcpServers": {
    "rein": {
      "command": "rein",
      "args": ["gateway", "connect", "--agent", "claude-code"]
    }
  }
}
```

Use `codex` in both places for a Codex caller. The name must match a registered
provider. Backend configuration belongs to the Gateway; pass `--backend` and any
required `--model` to `rein gateway start`.

The default Gateway ceiling permits only commands classified as read-only. Start
it with `--yes` if callers should be able to request mutating operations. The
caller must also request the appropriate approval level.
Organization policy can deny operations or require central approval; a policy
allow rule does not bypass the existing risk gate.

Registered callers submit approval requests when execution reaches a human
approval gate and send run outcome audit events. Review requests through your
organization's control plane; list them locally with:

```bash
rein approval list
```

An approved operation can be retried by the caller. Approval is checked against
the proposed operation; it does not raise the Gateway or direct MCP runtime's
approval ceiling.

## Profiles and device management

```bash
rein login --profile work
rein team list
rein team use work
rein agent revoke claude-code
rein logout
```

Profiles select a local Cloud enrollment. The policy and agent caches currently
share `REIN_HOME`, so profiles are not isolated workspaces. Use separate
`REIN_HOME` directories when isolated local state is required.

## Current scope

- Central policy evaluation is wired into MCP execution. Direct `rein in` runs
  use the local risk and approval gate without the Cloud policy callbacks.
- Central approval requests and audit events require a named caller through the
  Gateway bridge or legacy direct `rein mcp --agent NAME`. Registering a caller
  alone does not configure a client to use it.
- Registered-agent MCP fetches a live policy before planning and before every
  command. Network, authentication, malformed-policy, and expiration failures
  stop execution; it does not use an offline policy fallback.
- A failed pre-execution audit blocks the command. A failed completion audit
  stops the planning loop and warns that the command may already have run.
  Activity contains operation metadata rather than the complete local transcript.
- Rein applies controls to commands routed through it. Configure the calling
  agent's execution permissions separately if it must not use other tools.

See [safety and data handling](security.md) for the information sent to the model
backend and control plane.
