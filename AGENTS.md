# Repository Guidelines

## Project Structure & Module Organization
`cmd/main.go` is the controller entrypoint. API definitions live in `api/v1/`; generated deepcopy code is committed alongside the types. Core reconciliation logic and controller tests live in `internal/controller/`. Kubernetes manifests, RBAC, CRDs, samples, and Kustomize overlays are under `config/`. End-to-end tests live in `test/e2e/`, with helpers in `test/utils/`. Built binaries go to `bin/`; generated install output goes to `dist/`.

## Build, Test, and Development Commands
Use `make build` to generate code/manifests, format, vet, and build `bin/manager`. Use `make run` to run the controller against the current kubeconfig. Use `make test` for envtest-based Go tests and `make test-e2e` for Kind-backed end-to-end coverage. Use `make lint` or `make lint-fix` for `golangci-lint`. Container workflows use `make docker-build IMG=<registry>/logpilot:tag`, `make docker-push IMG=...`, and `make build-installer IMG=...`.

## Coding Style & Naming Conventions
This repo follows standard Go conventions: tabs via `gofmt`, exported identifiers in `CamelCase`, unexported identifiers in `camelCase`, and test files named `*_test.go`. Run `make fmt` and `make vet` before submitting changes. When updating CRD types or kubebuilder markers in `api/v1/`, regenerate artifacts with `make generate` and `make manifests`.

## Testing Guidelines
Unit and integration tests use Ginkgo/Gomega under `internal/controller/`; e2e specs use the same stack under `test/e2e/`. Prefer focused reconciliation tests for controller behavior and reserve `test/e2e` for cluster integration paths. Run `make test` locally for normal validation; run `make test-e2e` only when a `kind` cluster is already running.

## Commit & Pull Request Guidelines
Recent history uses conventional prefixes such as `feat:`, `fix:`, and `update:`. Keep commit subjects short and imperative, for example `fix: handle missing webhook secret`. Pull requests should include a brief summary, the reason for the change, and test evidence such as `make test` or `make test-e2e`. If APIs or manifests changed, include regenerated files in the same PR.

## Configuration & Deployment Notes
Sample custom resources live in `config/samples/`; apply them with `kubectl apply -f config/samples/log_v1_logpilot.yaml`. Install CRDs with `make install` and deploy the controller with `make deploy IMG=<registry>/logpilot:tag`. The default container tool is `nerdctl`, but `CONTAINER_TOOL` can be overridden if needed.
