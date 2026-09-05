# GitHub Actions integration (beta)

This integration authenticates a trusted CI job and governs commands explicitly
invoked through `rein ci run`. It is not a sandbox for an AI harness and does not
make every workflow step travel through Rein.

## Configure trust

Create a workload in Rein Control with provider `github-actions`, audience `rein`,
issuer `https://token.actions.githubusercontent.com`, and JWKS URL
`https://token.actions.githubusercontent.com/.well-known/jwks.json`. Set the
subject to the **exact** OIDC subject used by your job. For a legacy-named
repository using the environment in this example it is
`repo:OWNER/REPOSITORY:environment:rein-ci`. Repositories using immutable IDs or
custom subjects have a different value; inspect your configured subject and do
not assume the legacy example matches. No wildcard subject is accepted.

Protect the GitHub environment and permitted branches. Do not grant this
credential to untrusted pull-request code or execute PR code with
`pull_request_target`. No target-system credentials should be provided to
untrusted agents. A workload credential grants team-level workload API access;
it is not a narrowly scoped deployment credential.

Publish a reviewed policy before running CI, for example:
`caller=github-actions, tool=git, command=status, environment=rein-ci, effect=allow`.
Other operations are denied unless another matching rule permits them. CI
requires a published, unexpired bundle and an explicit matching rule, unlike
the interactive policy's default-allow behavior. Environment matching in CI is
exact, from `--environment`. Policies are fetched live over HTTPS; this CI
path does not independently verify their cryptographic signatures offline.

## Workflow

See [the example](examples/github-actions.yml). Replace both
`REIN_COMMIT_SHA` placeholders with a reviewed, published commit containing
this integration. Pin third-party actions to reviewed full SHAs for production.
The authentication action does not install or download Rein itself.

The job needs `contents: read` and `id-token: write`. Configure the repository
variable `REIN_WORKLOAD_ID`; it is an identifier, not a secret. The action obtains
a GitHub assertion, exchanges it for a Rein token, masks the token, and exports
`REIN_WORKLOAD_TOKEN` and `REIN_CONTROL_URL` for later steps. Its only output is
`expires-at`, never the credential. Credentials expire within 15 minutes;
repeat authentication for a longer job. Each exchange creates an independent
credential, so concurrent jobs no longer invalidate each other. Revoking the
workload invalidates all its credentials.

For another CI provider, securely inject a workload secret as
`REIN_WORKLOAD_TOKEN` and set `REIN_CONTROL_URL` to your trusted HTTPS origin.
No browser login, saved device profile, or operating-system keychain is used.
The normal `rein login`, `rein mcp`, and interactive CLI paths are unchanged;
this authentication path is specifically `rein ci`.

## Execution and approvals

```sh
rein ci check
rein ci run --environment rein-ci -- git status
rein ci run --environment rein-ci --approval-timeout 2m --timeout 5m -- TOOL ARGS
```

Arguments run directly, not through an implicit shell, with no interactive
stdin. Policy-required approvals and commands classified as mutating/unknown
request human review. The default approval timeout is zero: request approval
and exit nonzero without execution. Set a wait duration (maximum ten minutes)
to let a reviewer approve that invocation. A denied or expired request never
executes; denied requests currently wait until the deadline. A rerun creates a
new approval, rather than reusing another job's decision. Never put an
approval-review token in the same job.

Authentication, missing/invalid/expired policy, policy denial, pre-execution
audit failure, approval timeout, and command failure all fail the step.
The CLI checks policy again after approval. A changed policy requires a new
invocation. It emits `execution.started` before execution, then completed/failed.
An outcome-audit failure fails the step but cannot undo an already-run command.
Do not automatically retry mutating commands after an ambiguous outcome.

Commands and arguments are included in audit records. Pass application secrets
through appropriately scoped secret mechanisms, not command-line arguments.
Rein's workload/OIDC bearer values are omitted from the executed child's
environment, but this is not process isolation. Other steps and same-user
processes on a runner may still access job credentials. Use ephemeral,
administrator-controlled runners and isolated execution for stronger boundaries.
Pipeline setup steps, installed scripts, and commands outside `rein ci run`
are not governed by this integration.

## Verification and rollout

Local Go HTTP integration tests cover workload authentication, explicit policy
allow/deny, approval consumption, unavailable policy, pre/post-execution audit
failure, and command failure. Node tests cover OIDC exchange and unsafe response
handling. Laravel tests cover parallel credentials, replay rejection, expiration,
revocation, policy access, approval creation, and audit attribution.

A real GitHub-hosted run has not been executed as part of this implementation.
Before beta release, publish the reviewed CLI/action commit, deploy the workload
credential migration, run the example against a non-production workspace,
exercise denial and human approval, and inspect Activity. Ensure the Laravel
scheduler runs for expired credential cleanup. Then pin the validated commit
for customers.

References:
[GitHub OIDC reference](https://docs.github.com/en/actions/reference/security/oidc)
and [action metadata](https://docs.github.com/en/actions/reference/workflows-and-actions/metadata-syntax).
