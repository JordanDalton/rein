# Governed MCP execution

`rein gateway connect --agent NAME` and the legacy direct transport
`rein mcp --agent NAME` require a registered agent credential, an HTTPS control
plane, and a live published, unexpired policy. They fetch policy before planning
and before each command; network, authentication, malformed-policy, and
expiration errors stop execution. There is no offline cache fallback in this
mode.

This mode requires an explicit matching allow or approval rule. Unmatched operations are denied, which is stricter than the interactive default. Environment-specific rules do not match: MCP currently has no trusted environment selection. Use unconditional rules or CI's explicit environment option as appropriate.

Before starting the harness, a trusted operator must run `rein spec TOOL` for each approved executable. Discovery runs the executable's help/version probes, so it belongs outside the untrusted agent session. Governed `rein_in` and `rein_spec` refuse discovery and refresh requests. Cached specs and installed executables must be protected by host permissions.

Approval requests bind the exact argument array, working directory, caller, intent, tool, and policy version. Mutating commands and policy-required approvals must be approved in Rein Control; local `--yes`/`--auto` cannot bypass this gate. Retry the same operation after approval. A different model-generated command requires a different approval.

Each executed command has a pre-execution and completion audit event with its operation and exit/timeout status (not command output). A failed pre-execution audit blocks execution. A failed completion audit stops the loop and warns that execution may already have happened; inspect the result before retrying.

These guarantees apply to the registered-agent MCP path, not standalone `rein in` or agentless MCP. Harness tool restrictions remain a separate requirement. This is not a sandbox against modified binaries, user-writable configuration, malicious plugins, or other processes. The [runtime harness tests](harness-runtime-validation.md) verify selected restrictions with fixture MCP servers; full control-plane integration and complete mediation are not established by those tests.
