# How Rein works

[Back to README](../README.md)

The interesting part isn't the loop — it's the **capability map**.

A model has never seen your company's internal `acme-deploy` binary. So before
planning anything, rein teaches itself the tool: it crawls `--help`
recursively, and for Cobra-based CLIs it asks the binary directly via its
hidden `__complete` endpoint, which is machine-readable and beats parsing
prose. The result is cached in `~/.rein/specs/<tool>.json`, keyed to the
tool version. Discovery is reused for subsequent runs against the same tool version.
Planning still takes time and may incur model-provider charges.

That cached map is what makes a *per-CLI* wrapper different from a general
coding agent: a small, durable, shareable description of one tool, including
the ones no model has ever heard of.

## Layout

```
cmd/rein           CLI entry point
internal/spec      capability discovery + cache      (help crawl, __complete)
internal/planner   model backends + plan schema      (claude CLI, Messages API)
internal/risk      static argv risk classifier
internal/runner    sanitised exec + output elision
internal/loop      plan → gate → execute → observe
```

`internal/mcp` implements the MCP transport; `internal/policy` evaluates cached
organization policy. Cloud enrollment, approvals, and caller registration live
in `cmd/rein`.
