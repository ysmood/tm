# Agent instructions

Guidance for AI coding agents working in this repo.

## Testing

Use [got](github.com/ysmood/got) for testing.

```bash
go test ./...
```

Most of tm's behavior is only observable through a terminal, so `pkg/app` drives the real
binary under a pseudo-terminal. Build those tests on the harness in
`pkg/app/harness_test.go` (`setupTM`, `term`) rather than starting PTYs by hand: it builds
the binary once, gives each test its own `TM_HOME`/`TM_RUNTIME`, and waits for what tm drew
instead of sleeping. Two rules keep the suite fast and non-flaky — never mutate this
process's environment (it forbids parallel tests; hand paths to children through their own
env), and synchronize on output (`term.must`, `term.mustFrom`) rather than on a timer.

## Linting

```bash
go run github.com/ysmood/golangci-lint@latest -v 2.12.2
```

It will use golangci-lint v2.12.2 to lint the project.
