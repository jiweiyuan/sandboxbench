# Provider adapters: what is verified and what is not

Every hosted adapter in this repository was written from the provider's own
published API specification. **None of them has been run against a live API.**
This file records, per adapter, which part rests on a published document and
which part rests on an inference that needs checking with a real key.

Update it when you run one. "Verified" means a command ran against the live
service and returned what this adapter expects.

## local

| Mode | Source | State |
|---|---|---|
| process | `os/exec` | **Ran.** Full v0.1 local config, nine benchmarks. |
| docker | `docker` CLI | Written, not run. No Docker daemon was available. |

`docker pause` and `docker unpause` are reported as suspend and resume. That is
a real freeze and thaw of the container's processes, which is what the
lifecycle benchmarks mean.

Numbers from process mode are a latency floor, not a competitor: there is no
isolation boundary and no control plane.

## e2b

Source: `https://api.e2b.dev/openapi.json`, plus `packages/envd/spec/envd.yaml`
and `packages/envd/spec/process/process.proto` in `e2b-dev/infra`.

| Part | Source | Confidence |
|---|---|---|
| `POST /v2/sandboxes`, `DELETE /sandboxes/{id}` | OpenAPI | from the document |
| `POST /sandboxes/{id}/pause`, `/resume` | OpenAPI | from the document |
| Response fields `sandboxID`, `domain`, `envdAccessToken` | OpenAPI | from the document |
| `GET /health`, `GET /files`, `POST /files` on envd | `envd.yaml` | from the document |
| `X-Access-Token` header for envd | `envd.yaml` security scheme | from the document |
| `process.Process/Start`, server-streaming | `process.proto` | from the document |
| **envd host format** `{port}-{sandboxID}.{domain}` | inferred | **needs checking** |
| **envd port 49983** | inferred | **needs checking** |
| Connect envelope framing and the `0x02` end-of-stream flag | Connect protocol | from the spec, not exercised |

Both inferred values are configuration keys (`envd_port`, `envd_scheme`), so
they can be corrected without touching code.

## daytona

Source: `https://api.daytona.io/openapi.json`.

| Part | Source | Confidence |
|---|---|---|
| `POST /sandbox`, `GET`/`DELETE /sandbox/{id}` | OpenAPI | from the document |
| `POST /sandbox/{id}/start`, `/stop`, `/pause` | OpenAPI | from the document |
| `state` enum values, `toolboxProxyUrl` field | OpenAPI | from the document |
| `memory` is GiB, not MiB | OpenAPI example | from the document |
| **`POST {toolboxProxyUrl}/{id}/toolbox/process/execute`** | not in the OpenAPI document | **needs checking** |
| **Request `{command, cwd, timeout}`, response `{exitCode, result}`** | SDK shape | **needs checking** |

The toolbox path is the `toolbox_path` config key, so a correction is a config
change. The adapter writes and reads files through the shell rather than a
dedicated upload endpoint, which keeps it to the one toolbox endpoint; no v0.1
benchmark writes a large file.

The toolbox returns one combined output stream. The adapter reports it as
stdout and leaves stderr empty rather than guessing at a split.

## runta

Source: `https://runta.com/openapi.yaml` (Runta Public REST API 0.1.0).

| Part | Source | Confidence |
|---|---|---|
| `POST /v2/runtimes`, `GET`/`DELETE /v2/runtimes/{id}` | OpenAPI | from the document |
| `POST /v2/runtimes/{id}/pause`, `/resume`, `/start`, `/stop` | OpenAPI | from the document |
| `GET`/`PUT /v2/runtimes/{id}/files?path=` (octet-stream) | OpenAPI | from the document |
| `status` enum, `ssh_enabled`, `degraded` fields | OpenAPI | from the document |
| `GET /v2/ssh-host-key` returning `known_hosts` | OpenAPI | from the document |
| `POST /v2/runtimes/{id}/checkpoints` | OpenAPI | from the document, unused in v0.1 |
| **SSH gateway hostname and port** | not in the document | **required configuration, no default** |
| **SSH username convention** | not in the document | **required configuration, no default** |

The notable finding: Runta's public REST API has **no execute endpoint**.
Commands go in over the SSH gateway, which is why this adapter is the only one
in the `ssh` exec transport class. It opens the SSH connection during `Create`,
not on first `Exec`, so create latency means create-to-first-exec for every
provider in a run. A pause drops the connection, so `Resume` reconnects and
that reconnection is inside the measured resume latency.

The adapter verifies the gateway host key against the `known_hosts` entry the
API serves. It never disables host key checking: an unauthenticated channel is
not the product anyone runs, so measuring one would not be measuring Runta.

Without `ssh_host` and `ssh_private_key` the adapter fails on the first
command with a message saying exactly that, rather than guessing at a hostname.

## Not implemented

Modal and Cloudflare were discussed and are not here. The provider contract is
eight methods; see `docs/design.md` and `providers/local/local.go`.
