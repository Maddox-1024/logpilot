# Repository Guidelines

## Project Structure & Module Organization
- `cmd/` holds the entrypoint (`cmd/main.go`) for the controller manager.
- `internal/` contains core controller logic (reconciler, tests, helpers).
- `api/` defines the CRD API types (versioned). Generated code lands alongside.
- `config/` contains Kustomize bases/overlays, RBAC, CRDs, and samples.
- `test/` includes e2e tests and test utilities; unit/integration tests live under `internal/`.
- `bin/` is the default output for built binaries; `dist/` holds generated install artifacts.

## Build, Test, and Development Commands
- `make build`: generate manifests/code, format, vet, then build `bin/manager`.
- `make run`: run the controller locally against your kubeconfig.
- `make test`: run controller tests with envtest; writes `cover.out`.
- `make test-e2e`: run Ginkgo e2e tests in `test/e2e` (requires a running Kind cluster).
- `make lint` / `make lint-fix`: run `golangci-lint` (optionally auto-fix).
- `make docker-build` / `make docker-push IMG=...`: build and push the controller image.
- `make build-installer IMG=...`: generate `dist/install.yaml` from `config/`.
- `make help`: list all available targets.

## Coding Style & Naming Conventions
- Go formatting is enforced with `gofmt` (`make fmt`). Indentation is tabs per Go style.
- Keep exported types and fields in `CamelCase`; unexported in `camelCase`.
- Update CRD markers and run `make manifests` when API changes.

## Testing Guidelines
- Tests use Ginkgo/Gomega (`internal/controller/*_test.go`, `test/e2e/*_test.go`).
- Name tests with `*_test.go`; e2e specs live under `test/e2e`.
- Run `make test` for envtest-based suites; `make test-e2e` for cluster integration.

## Commit & Pull Request Guidelines
- Commit messages follow conventional prefixes seen in history: `feat:`, `fix:`, `update:`.
- PRs should include:
  - A short summary and rationale.
  - Test evidence (commands run, e.g. `make test`).
  - Updated generated files when APIs or manifests change (`make generate`, `make manifests`).

## Configuration & Deployment Notes
- Sample CRs live in `config/samples/`. Apply with `kubectl apply -k config/samples/`.
- Deploy to a cluster with `make install` (CRDs) and `make deploy IMG=...`.
