# Contributing

Thanks for taking the time to contribute.

## Development

Requirements: Go (see `go.mod`) and optionally
[pre-commit](https://pre-commit.com/). `make lint` installs the pinned
`golangci-lint` into `./bin` automatically.

```bash
make check          # tidy, fmt, vet, lint, test, test-race
make test           # unit tests
make cover          # total coverage
make example        # run the example API
```

Install the git hooks once:

```bash
pre-commit install
```

## Guidelines

- Keep the public API small. `otelkit` is a bootstrap kit: prefer wiring
  existing OpenTelemetry packages over re-implementing them.
- Add table-driven tests with BDD-style names
  (`given ... when ... then ...`).
- Update `README.md` when behavior or configuration changes.
- Use [Conventional Commits](https://www.conventionalcommits.org/) so the
  generated changelog stays useful (`feat:`, `fix:`, `docs:`, `test:`,
  `chore:`, `ci:`).

## Pull requests

1. Fork the repository and create a branch.
2. Run `make check` and `pre-commit run --all-files`.
3. Describe the motivation and the behavior change in the pull request.

By participating you agree to keep discussions respectful and constructive.
