# LogPilot

LogPilot is a Kubernetes operator that periodically queries Loki, summarizes recent logs, asks an LLM for root-cause analysis, and sends alerts to a webhook endpoint.

It is designed for a simple flow:

1. A `LogPilot` custom resource defines a Loki query, schedule, LLM provider, and webhook secret.
2. The controller reads the referenced secrets.
3. It queries Loki for the latest time window.
4. It classifies, trims, clusters, and summarizes logs locally.
5. It optionally asks OpenAI or Gemini for a short diagnosis.
6. It writes the latest result to CR status and sends an alert when error or critical logs are detected.

## Features

- Periodic Loki `query_range` polling
- Deterministic local log processing before LLM calls
- OpenAI and Gemini support
- Webhook URL stored in Kubernetes Secret
- Latest analysis persisted in `status`
- Controller-runtime metrics, health probes, and leader election

## Architecture

For each `LogPilot` resource, the controller:

- parses `spec.interval`
- reads the LLM API key from `spec.llmAPIKeySecret`
- reads the alert webhook URL from `spec.webhookSecret`
- queries Loki with `spec.logQL`
- extracts log entries and infers severity
- clusters similar messages and detects common patterns
- builds a compact JSON summary for the LLM
- updates:
  - `status.lastAttemptTime`
  - `status.lastSuccessTime`
  - `status.lastAnalysis`
  - `status.lastError`

Current alerting behavior:

- if no logs are returned, status is set to `No logs found`
- if logs exist but none are `error` or `critical`, status is set to `No error/critical logs found`
- if error-like logs exist, the controller generates a summary and sends the result to the configured webhook

## CRD

`LogPilot` is namespaced and defined under:

```yaml
apiVersion: log.aiops.com/v1
kind: LogPilot
```

### Spec

| Field | Required | Description |
| --- | --- | --- |
| `lokiURL` | yes | Base URL of Loki |
| `logQL` | yes | Loki LogQL query |
| `interval` | yes | Poll interval, default `1m`, pattern `^([0-9]+(ns|us|ms|s|m|h))+$` |
| `llmProvider` | yes | `OpenAI` or `Gemini` |
| `llmModel` | yes | Model name used by the selected provider |
| `llmAPIKeySecret` | yes | Secret name holding the LLM API key |
| `llmAPIKeySecretKey` | yes | Secret key for the API key |
| `webhookSecret` | yes | Secret name holding the alert webhook URL |
| `webhookSecretKey` | yes | Secret key for the webhook URL |
| `openAI.baseURL` | no | Custom OpenAI-compatible endpoint |
| `gemini` | no | Reserved provider-specific config |

### Status

| Field | Meaning |
| --- | --- |
| `lastAttemptTime` | Last time a reconcile attempted log analysis |
| `lastSuccessTime` | Last successful analysis time |
| `lastAnalysis` | Latest stored summary / LLM output |
| `lastError` | Latest error message |

## Prerequisites

- Go `1.23+`
- A Kubernetes cluster
- `kubectl`
- A reachable Loki instance
- An OpenAI-compatible API or Gemini API key
- A webhook endpoint for alerts

For local development:

- `kind` for e2e tests
- container runtime compatible with `nerdctl` or override `CONTAINER_TOOL`

## Quick Start

### 1. Install the CRD

```sh
make install
```

### 2. Create required secrets

Create the LLM API key secret:

```sh
kubectl create secret generic logpilot-llm \
  --from-literal=api-key='<YOUR_LLM_API_KEY>' \
  -n default
```

Create the webhook secret:

```sh
kubectl create secret generic logpilot-webhook \
  --from-literal=url='<YOUR_WEBHOOK_URL>' \
  -n default
```

### 3. Apply the sample resource

```sh
kubectl apply -f config/samples/log_v1_logpilot.yaml
```

### 4. Check status

```sh
kubectl get logpilot sample-logpilot -n default -o yaml
```

## Sample

The repository includes a ready-to-apply sample in `config/samples/log_v1_logpilot.yaml`:

```yaml
apiVersion: log.aiops.com/v1
kind: LogPilot
metadata:
  name: sample-logpilot
  namespace: default
spec:
  lokiURL: http://loki-gateway.monitoring.svc.cluster.local
  logQL: '{namespace="default"} |= "error"'
  interval: 1m
  llmProvider: OpenAI
  llmModel: gpt-4o-mini
  llmAPIKeySecret: logpilot-llm
  llmAPIKeySecretKey: api-key
  webhookSecret: logpilot-webhook
  webhookSecretKey: url
  openAI:
    baseURL: https://api.openai.com/v1
```

If you want to use Gemini, change:

```yaml
llmProvider: Gemini
llmModel: gemini-1.5-pro
```

and point `llmAPIKeySecret` to a secret containing the Gemini API key.

## Running Locally

Run the controller against your current kubeconfig:

```sh
make run
```

This is useful when:

- Loki is already reachable from your machine or cluster network
- you want faster iteration than rebuilding the controller image

## Build and Deploy

Build the binary:

```sh
make build
```

Build the controller image:

```sh
make docker-build IMG=<registry>/logpilot:tag
```

Push the image:

```sh
make docker-push IMG=<registry>/logpilot:tag
```

Deploy to the cluster:

```sh
make deploy IMG=<registry>/logpilot:tag
```

By default, the deployment is installed into namespace `logpilot-system` with the `logpilot-` name prefix from `config/default/kustomization.yaml`.

## Testing

Run controller tests:

```sh
make test
```

Run end-to-end tests:

```sh
make test-e2e
```

Notes:

- `make test` uses `envtest`
- `make test-e2e` expects a running Kind cluster
- the controller test suite covers secret lookup, Loki success/failure paths, and status updates

## Useful Commands

Show available make targets:

```sh
make help
```

Run linters:

```sh
make lint
```

Auto-fix lint findings where supported:

```sh
make lint-fix
```

Generate install bundle:

```sh
make build-installer IMG=<registry>/logpilot:tag
```

This writes `dist/install.yaml`.

## Metrics and Health

The manager exposes:

- health probe on `:8081/healthz`
- ready probe on `:8081/readyz`
- metrics endpoint on `:8443` in the default deployment overlay

The default manifests enable secure metrics serving and authentication/authorization filtering.

## Current Limitations

- The controller only supports webhook-based alert delivery
- The LLM prompt and deterministic rules are embedded in controller code
- `GeminiConfig` exists but is currently empty
- The operator analyzes one Loki query per `LogPilot` resource

## Development Notes

When API fields change:

```sh
make manifests
make generate
```

When you update samples or deployment manifests, keep:

- `config/samples/`
- `config/crd/bases/`
- `dist/install.yaml`

in sync before release.

## License

Apache-2.0
