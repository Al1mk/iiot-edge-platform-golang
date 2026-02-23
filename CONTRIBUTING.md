# Contributing

## Branching

- Work on feature branches only (`feature/`, `fix/`, `chore/`).
- Do not push directly to `main`.

## CI

- All changes must pass `make ci` before merge.
- CI runs `go vet`, `go test -race`, and static builds for all binaries.

## Pull Requests

- A pull request is required to merge into `main`.
- At least one approval is required before merging.
