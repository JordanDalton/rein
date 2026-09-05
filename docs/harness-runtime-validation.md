# Harness runtime validation

Tested on macOS, September 5, 2026, with Claude Code **2.1.261** and Codex CLI
**0.153.4**. These are version-specific integration tests, not a certification of
every installation or complete host isolation.

## Results

| Configuration | Positive routing | Negative routing |
| --- | --- | --- |
| Claude generated persistent settings, explicitly loaded | All three Rein tools dispatched and `rein_list` result returned to the scripted model | Native Bash/Write left no marker; Read did not reveal the fixture sentinel; unapproved MCP tool was denied by Rein's hook despite host permission consent |
| Claude exact generated strict-launch arguments | All three Rein tools available upfront and dispatched | Native tools were removed; forced Bash/Write/Read calls did not operate; nonexistent MCP tool did not dispatch |
| Codex generated settings, passed as invocation overrides with vetted hook trust | All three Rein tools dispatched after native metadata discovery | Shell unavailable; `apply_patch` blocked by Rein's hook; unapproved MCP tool blocked despite explicit fixture consent |

Claude tests deliberately enable deferred tool search. `alwaysLoad: true` on
Rein's registration keeps its tools usable without weakening the guard to permit
`ToolSearch`. Existing installations need configuration reapplied to receive this
field; preview first and use the normal receipt/undo workflow if drift is reported.

Codex uses its native client-side tool search to discover the MCP namespace. That
discovery was not intercepted by the default-deny hook. No exception was added to
Rein's guard. Actual MCP invocations were intercepted using their fully qualified
names, and only the three permitted names passed.

## Reproduce

Both CLIs must be installed on PATH:

```sh
REIN_RUNTIME_TESTS=1 go test ./cmd/rein -run 'Test(Claude|Codex)RuntimeGuard' -v -count=1
```

Regular `go test ./...` skips these opt-in tests. The runtime suite creates
temporary working directories, generated settings, a fixture MCP server, and a
scripted loopback HTTP model. It does not make paid inference requests, supply
real API credentials, or edit active harness settings. Installed CLIs can still
read system-managed configuration and maintain their own runtime caches; this
is not a fresh-OS isolation test. Run on a disposable host for release qualification.

The fixture MCP server implements Rein's protocol but does not execute real
commands. It includes an additional forbidden tool as a negative control in
the persistent tests. Tests check dispatch records, returned results, denial
messages, and filesystem markers rather than trusting a model's self-report.

Codex receives generated config as explicit overrides with user config ignored.
Only the vetted fixture uses `--dangerously-bypass-hook-trust` and per-tool MCP
consent. Those test options are **not** installed by `rein configure`. A first
test using only the project file did not load the guard or MCP registration;
explicit loading was necessary to separate dispatch behavior from project trust.
Project discovery and the normal interactive `/hooks` trust flow remain unverified.

## Remaining release checks

### Live control-plane check — September 5, 2026

With explicit approval, published the three-rule validation policy (allow `true`,
require approval for `printf`, deny everything else) as v1 in the designated live
test team. The real compiled Rein binary was driven through MCP stdio using a
scripted local planner. `/usr/bin/true` executed successfully; `/usr/bin/false`
was rejected; `/usr/bin/printf` stopped and created a pending approval visible
in Activity. Activity also showed pre-execution and completion records for `true`.
The human-approved retry completed successfully: `printf` returned exit code 0
and printed the validation marker. A subsequent read confirmed the approval's
status was `consumed`, with both execution audit records and exit/timeout metadata.

The check also exposed reporting gaps: Activity's executed counter remained zero
despite displaying the completion event, and the denied command had no audit
event. These should be addressed before claiming complete activity reporting.
The API also stored the format argument `%s\n` as `%s` in both the approval and
audit operation arrays, while the runner used the original newline-bearing
argument. Stored operation data therefore is not yet byte-exact; investigate
request string trimming before claiming exact-argument approval/audit fidelity.

The explicit live runner is `scripts/validate-live-mcp.mjs`. It requires a
separate enrolled validation profile and compiled Rein binary; its `initial`
phase creates live audit events and an approval request. Its `approved` phase
must only be used after a human approves that exact pending operation. Neither
phase modifies policy or approves its own requests.

- Validate normal project settings and hook trust in fresh interactive sessions.
- Exercise actual `rein mcp --agent ...` against a staging control plane, including
  expired policy, approval retry, audit outage, and missing-server behavior.
- Audit plugins, managed settings, other hooks, hosted tools, existing execution
  sessions, and configuration/binary tampering. Native metadata discovery is not
  the only possible exception to hook coverage.
- Repeat on every supported OS and harness version. No other harness has been
  runtime-qualified by this suite.

OpenAI's [hook coverage and trust documentation](https://learn.chatgpt.com/docs/hooks)
informed the limited Codex claims and explicit-trust test setup. Claude's
[MCP deferral documentation](https://code.claude.com/docs/en/mcp#exempt-a-server-from-deferral)
informed the upfront-loading fix. See also [governed MCP behavior](governed-mcp.md).
