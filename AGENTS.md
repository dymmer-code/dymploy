# AGENTS.md

Instructions for AI coding agents working in this repository (`dymploy`, a CI/CD daemon + CLI in Go —
module `github.com/dymmer-code/dymploy`). Design notes are in the `docs` directory; keep them in sync with the code.

## Working rules

- **TDD**: write or update the tests first, then the code. Coverage must not decrease with a commit.
  Run `go vet ./... && go test -race -cover ./...` before every commit.
- **Commits**: [Conventional Commits](https://www.conventionalcommits.org) (`feat:`, `fix:`, `docs:`,
  `test:`, `refactor:`, `chore:`), pushed straight to `main`.
- **Layout**: `main.go` (version only) · `cmd/` (cobra commands, thin) · `internal/<package>/` (logic).
- **Unix sockets**: paths are limited (~104 bytes on macOS). In tests create a short directory with
  `os.MkdirTemp("", "dp")`, not `t.TempDir()`.
- **Security by default**: the daemon never trusts the repository it builds. Allow-lists (organizations,
  repositories, branches, installations) live in the daemon configuration, never in the repository;
  secrets never go in logs, URLs or `.git/config`; files holding secrets are 0600.
- **Provider independence**: anything specific to GitHub (or any provider) lives behind the `Provider`
  interface; no other package may import provider types.
- **Strict configuration**: unknown keys are an error.

## Not in scope here

No graphical interface (a web view can be built on top of the API and the WebSocket channel). Installation-specific things (reverse proxy, secrets store, paths) belong to the deployment and to the configuration, not to this code.
