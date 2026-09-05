# Contributing to Rein

Bug reports, compatibility examples for unfamiliar CLIs, documentation fixes,
and focused pull requests are welcome.

## Build locally

Install the Go version specified in `go.mod` (currently Go 1.27+), then:

```bash
git clone https://github.com/jordandalton/rein
cd rein
go build -o bin/rein ./cmd/rein
./bin/rein help
```

For a live run, configure a [model backend](docs/configuration.md) and make sure
the tool you want to wrap is on your `PATH`. Cloud access is optional for local
CLI development.

## Validate a change

```bash
go test ./...
go build ./cmd/rein
```

These match the repository's CI checks. Format changed Go files with `gofmt`.
For behavior changes, include focused tests where practical. For documentation
changes, check links and verify commands against the current CLI.

Describe the problem, resulting behavior, and validation in your pull request.
For discovery bugs, include the tool version, relevant help output, and what
`rein spec <tool> --show` learned. Remove credentials and private information
from reports and transcripts.

## Security reports

Do not include exploit details or credentials in public issues. If GitHub's
private vulnerability reporting is enabled for this repository, use its
**Security → Report a vulnerability** flow. A dedicated private reporting contact
has not yet been published.

## License

This repository does not yet include a license file. The maintainer needs to
publish the intended license terms; this guide does not grant a license.
