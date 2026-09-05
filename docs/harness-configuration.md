# Configure Claude Code and Codex

`rein configure` detects supported harnesses on PATH. Configuration is preview-only
unless you explicitly pass `--apply`.

Interactive setup starts the persistent local Rein Gateway and configures the
harness to use `rein gateway connect --agent HOST`. It is the recommended setup:

```sh
rein configure claude-code
```

For preview-first setup, start the gateway and select it explicitly when saving
the launch profile:

```sh
rein gateway start
rein agent register claude-code
rein configure claude-code --gateway --dry-run
rein configure claude-code --gateway --apply
rein configure claude-code --check
rein configure claude-code --launch
```

Explicit configuration without `--gateway` retains the standalone `rein mcp`
transport for compatibility.

Use an installed Rein binary, not `go run`: the launch profile stores the absolute
path to the running executable. If you move the binary, undo and configure again.
Run from the project root consistently; project profiles are not searched in parent
directories. Use `--scope user` on every command for a user-scoped launch profile.

## What gets changed

### Persistent project guardrails (macOS/Linux)

Use this mode when users should launch the harness normally from the configured
project, rather than opting into a Rein launcher each time:

```sh
rein agent register claude-code
rein gateway start
rein configure claude-code --gateway --persistent --dry-run
rein configure claude-code --gateway --persistent --apply
rein configure claude-code --persistent --check
# Restart Claude, review/trust project settings, hooks, and MCP, then test.
rein configure claude-code --persistent --undo
```

Replace `claude-code` with `codex` for Codex. Persistent mode currently supports
project scope only; it rejects user scope, `--launch`, and combined `--register`.
Backend/model options still configure only Rein's planner. For gateway-backed
configuration, pass them to `rein gateway start`; options saved in a standalone
profile apply to its dedicated `rein mcp` process. Use an installed Rein binary,
not `go run`, and keep it at the saved location while the guard is installed.

- Claude merges `.claude/settings.json` and `.mcp.json`, adding native-tool deny
  rules plus a catch-all `PreToolUse` hook. Rein's server uses `alwaysLoad: true`
  so its three tools are available without the blocked native `ToolSearch` tool.
- Codex merges `.codex/config.toml`, enables hooks, disables native shell and
  hosted web search, and installs the same catch-all hook and Rein MCP server.
  Apply/check refuse a Codex CLI that does not report hook feature support.
- The guard defers only `mcp__rein__rein_in`, `mcp__rein__rein_list`, and
  `mcp__rein__rein_spec`; all other intercepted calls and malformed input are
  denied. Deferring does not auto-approve Rein operations or bypass host rules.
- Existing settings, plugins, other servers, and hooks are preserved and must
  be audited separately. Rein's own MCP registration is replaced with the chosen
  binary and planner options. JSON/TOML formatting changes; TOML comments are
  recovered by undo, not preserved in the installed serialization.
- A private `.rein/harnesses/HOST.persistent.json` receipt stores original bytes
  and file permissions. Exclude `.rein/` from version control: originals may
  contain configuration secrets. Undo checks all targets before restoring and
  refuses to clobber subsequent edits. An interrupted setup retains its receipt.

**Installed is not verified.** `--check` validates installed file bytes, the Rein
binary, and local host prerequisites. It does not certify hook trust, effective
settings, tool coverage, or a live MCP connection. Start a fresh session, inspect
`/hooks`, review/trust the hook and project, approve Rein MCP where required, and
test native shell, file reads/patches, runtimes, other MCP, and delegation using
disposable fixtures. Confirm a positive Rein call appears in Activity. Test a
missing Rein server and confirm no fallback. Repeat after every harness upgrade.
If tool discovery cannot expose Rein without an unapproved tool, stop: do not
loosen the allowlist or report the setup as verified.

The [runtime validation report](harness-runtime-validation.md) records the
installed versions tested, reproducible loopback tests, and remaining gaps.
Codex's native metadata discovery ran outside the hook in that test; this is
another reason Codex remains partial coverage, not a claim that every operation
travels through Rein.

Codex's documented hook coverage excludes hosted and some specialized tools;
existing process input does not get a fresh pre-tool check. Untrusted hooks are
skipped. Hook timeouts, disabled hooks, other automation, and alternative
configuration scopes also require review. These are **project guardrails**, not
universal Rein-only enforcement. Windows and other harnesses are not supported
by this installer. Do not apply a Claude configuration to Cursor, Copilot, or
another harness and assume it has the same effect.

For an administrator-enforced boundary, isolate the harness from protected
credentials/resources, give those capabilities only to Rein, and protect Rein's
binary, policy, and harness settings from agent-side modification. Admin-managed
hooks and MCP allowlists can support that deployment, but this command does not
install device management policies or provision isolation.

References checked September 5, 2026:
[Claude hooks](https://code.claude.com/docs/en/hooks),
[Claude settings](https://code.claude.com/docs/en/settings),
[Codex hook coverage and trust](https://learn.chatgpt.com/docs/hooks), and
[Codex configuration](https://learn.chatgpt.com/docs/config-file/config-reference).

The remainder of this guide describes the separate, opt-in launch-profile mode
(without `--persistent`).

### Use a different model inside Rein

The harness model and Rein's planning model are independent. For the preferred
gateway transport, choose Rein's model when starting the gateway:

```sh
rein gateway stop
rein gateway start --backend ollama --model YOUR_LOCAL_MODEL
rein configure claude-code --gateway --dry-run
rein configure claude-code --gateway --apply
rein configure claude-code --launch
```

Replace `YOUR_LOCAL_MODEL` with an installed model. For a hosted backend, use
`--backend openai --model YOUR_MODEL_ID` and configure that backend's credentials
in the environment. Configuration does not provision credentials or download models.
No API key values are stored in the harness profile. Gateway options apply to
every connected agent and never change the outer Claude Code or Codex model.
For the standalone transport, `--backend` and `--model` remain configuration-time
options that become arguments to `rein mcp`; `--launch` uses the saved values.
For an existing different profile, undo it before applying the replacement,
consistent with the single-undo-point workflow below.

See [backend configuration](configuration.md) for supported backends and credentials.

Project setup writes `.rein/harnesses/claude-code.json` (or `codex.json`). User
setup writes `harnesses/HOST.json` inside Rein's home directory. A private `.undo`
receipt holds the original bytes, if any. Keep machine-specific profiles and
receipts out of version control.

Your `.claude` and `.codex` configuration, instruction files, credentials, and
other MCP servers are not rewritten. This is a **launch-profile workflow**:
restrictions apply only to sessions started with `rein configure HOST --launch`.
Launching `claude` or `codex` directly is unchanged. This does not centrally enforce
an organization-wide policy or protect against someone changing the profile.

## Identity and approval

Enroll with `rein login` first and select the intended team profile. Register the
caller yourself with `rein agent register claude-code` (or `codex`), or explicitly
opt into registration while applying:

```sh
rein configure claude-code --apply --register
```

Registration uses the active cloud profile and persists a credential. It is **not**
undone by configuration undo. If registration succeeds but saving the launch profile
fails, the identity remains registered; inspect `rein agent list` before retrying.
Existing caller registrations are reused, not duplicated. Existing registration
and credential checks do not establish that the remote credential is still valid.

The selected Rein runtime uses a read-only approval ceiling by default. Start
the Gateway or standalone MCP server with `--yes` or `--auto` only when the
corresponding operation classes should be available. These flags do not publish
or bypass organization policy. Unsupported operations must stop rather than use
native tools as a fallback.

## Claude Code

The launcher supplies `--tools ""`, `--strict-mcp-config`, and a Rein-only MCP JSON
configuration as a process argument (not shell interpolation). This removes built-in
tool capabilities, including native shell and file tools. Review managed MCP,
plugins, hooks, and host automation separately. Coverage is reported as a strict
launch plan with **unverified enforcement**, not a security certification.

Reference: [Claude Code CLI](https://code.claude.com/docs/en/cli-reference).

## Codex CLI

```sh
rein configure codex --apply
rein configure codex --check
rein configure codex --launch
```

The launcher applies command-line overrides for `features.shell_tool=false` and
an enabled, required `mcp_servers.rein` stdio server. Existing unrelated Codex
configuration is preserved. Other servers and native tools may remain, so this is
explicitly **partial coverage**, not Rein-only enforcement. Check capabilities in
your installed version, including native patches, code runtimes, apps, plugins,
computer use, and delegation. The CLI does not silently disable undocumented tools.

Reference: [Codex configuration](https://learn.chatgpt.com/docs/config-file/config-reference).

## Verification

`--check` reads the saved profile, checks local executable availability and caller
registration, and reports limitations. It does not start a model, call MCP, verify
remote credentials, or certify the effective host tool set.

In a disposable repository, inspect the host's tools and request a harmless
`git status` through Rein. Match its device, caller, operation, and reported outcome
in Control activity. Try the same operation through a native tool: it must not
succeed if you require strict routing. Also test native file access, patches,
delegation, denied requests, and unavailable-server behavior. Never loosen a
production policy to make onboarding pass. Recheck after harness upgrades.

## Undo

```sh
rein configure claude-code --undo
rein configure codex --scope user --undo
```

Undo restores the previous profile or removes one newly created by configure.
It refuses to overwrite subsequent edits. Identical applies are no-ops; undo the
existing setup before replacing it with a different one. Undo does not stop an
already-running session or revoke credentials. Configuration paths containing
symlinks are refused; choose a real project directory and Rein home.
