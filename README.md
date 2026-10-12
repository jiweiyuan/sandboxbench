# SandboxBench

A benchmark suite for cloud-based code execution sandboxes.

Providers now sell the same five verbs — create a sandbox, run a command in
it, suspend it, resume it, destroy it. AI coding agents and reinforcement
learning loops buy them. Serverless benchmark suites do not describe that
workload: a function invocation is stateless and single-shot, and a sandbox
session is interactive, stateful, reset rather than re-invoked, and run
thousands at a time.

This repository holds two things:

- `pkg/`, `providers/`, `cmd/` — the benchmark framework, in Go.
- `paper/` — the research paper, in LaTeX.

## Status

Read this before you trust anything here.

| Part | State |
|---|---|
| Framework, workloads, metrics, reporting | Written. `go build`, `go vet` and `go test` pass. |
| `local` adapter, process mode | Ran. Full suite end to end, 9 benchmarks. |
| `local` adapter, docker mode | Written, not run. No Docker daemon was available. |
| `e2b`, `daytona`, `runta` adapters | Written from each provider's published API spec. **Never run against a live API.** |
| Paper | Structure and argument written. **No experiment has been run.** Every quantitative claim is marked `\needsdata`. |
| Bibliography | Starting point. Entries tagged `TO VERIFY` were written from memory and need checking. |

There are no benchmark results in this repository, and no leaderboard.

## Quick start

No credentials needed:

```sh
go build -o bin/sandboxbench ./cmd/sandboxbench
./bin/sandboxbench list
./bin/sandboxbench run --config configs/v01-local.yaml --region "describe where you are"
```

That runs the whole suite against the `local` adapter, which executes
workloads as processes on your own machine. Use it to check a workload and to
sanity-check the statistics. Do not put its numbers next to a hosted
provider's: there is no isolation boundary and no control plane, so it is a
floor on latency, not a competitor.

Against hosted providers:

```sh
export E2B_API_KEY=...
export DAYTONA_API_KEY=...
export RUNTA_API_KEY=... RUNTA_SSH_KEY_ID=... RUNTA_SSH_PRIVATE_KEY=~/.ssh/id_ed25519
./bin/sandboxbench run --config configs/v01-single.yaml --region "aws eu-central-1"
```

`configs/v01-single.yaml` and `configs/v01-cluster.yaml` both create and
destroy sandboxes on paid services. Read the comments in them first. Start at
a low repetition count and a low concurrency.

## What it measures

Five performance dimensions, crossed with two execution scales:

| Dimension | Single sandbox | Cluster |
|---|---|---|
| Lifecycle | create-to-first-exec | ready throughput, burst p99 |
| Execution | per-command latency | concurrent command throughput |
| Compute | build completion time | concurrent build slowdown |
| Storage | metadata ops/s, IOPS | concurrent I/O slowdown |
| Network | egress RTT, throughput | concurrent network slowdown |

Scalability is not a sixth dimension. It is the cluster column of the other
five. Making it a peer row would score the same underlying behaviour twice.

On top of that: four end-to-end application workloads (prepare, build,
develop, continue), and a reinforcement-learning profile that measures
environment step, environment reset and parallel rollouts.

`./bin/sandboxbench list` prints the whole set with descriptions.

## Three rules the framework enforces

**1. Unsupported is not zero.** Every result carries one of four statuses:
`completed`, `unsupported`, `failed`, `skipped`. A provider with no suspend
operation is reported as `unsupported` on the resume benchmark, never scored
zero. Scoring a missing feature as zero makes a leaderboard reward having
fewer features. Capabilities are checked before any sandbox is created, so an
unsupported benchmark costs nothing.

**2. The exec transport is not the sandbox.** Every command is timed twice:
once by the runner, which includes a control-plane round trip, and once inside
the sandbox by a shell wrapper. Compute, storage and application workloads
report the in-sandbox duration. This matters because the three providers do
not use the same mechanism:

| Provider | Transport | Per-command cost |
|---|---|---|
| Daytona | REST | one HTTP request to a per-sandbox toolbox proxy |
| E2B | Connect-RPC | a server-streaming RPC to `envd` inside the sandbox |
| Runta | SSH | a session on a multiplexed connection through an SSH gateway |
| local | process | `os/exec` on the runner host |

Runta's public REST API has no execute endpoint at all. A single "command
latency" column across these three would mostly be ranking transports.

**3. A quota is not a throughput ceiling.** HTTP 429 is surfaced as a typed
error, not retried away. And the HTTP transport uses a large connection pool,
because with Go's default pool a burst of a thousand creates queues on the
client's own connections and the benchmark then measures the runner.

## Repository layout

```
pkg/sandbox/      provider contract: Provider, Sandbox, Capabilities, Status
pkg/metrics/      percentiles, bootstrap intervals, throughput, slowdown
pkg/workload/     what gets measured; one file per dimension group
pkg/runner/       config, scheduling, capability gating, teardown
pkg/report/       JSON, Markdown and LaTeX renderers
providers/        local, e2b, daytona, runta
configs/          runnable configurations
paper/            the LaTeX paper
docs/             design notes and provider verification status
```

## Writing a provider adapter

Implement `sandbox.Provider` and `sandbox.Sandbox`, call `sandbox.Register` in
an `init`, and add a blank import to `cmd/sandboxbench/main.go`. The contract
is eight methods. Two rules:

- Return `sandbox.ErrUnsupported` from `Suspend` and `Resume` when the
  provider has no such operation. Do not fake it.
- `Create` must not return until the sandbox can run a command. A provider
  that returns early has moved its startup cost into the user's first command,
  and the benchmark should see that cost either way.

See `providers/local/local.go` for the simplest complete example and
`docs/providers.md` for what in each hosted adapter is verified and what is
not.

## Building the paper

```sh
make -C paper table     # regenerate the results table from the newest run in results/
make -C paper           # build main.pdf
```

The results table is generated by `sandboxbench report --format=latex`. No
number is retyped between the artefact and the paper.

## License

Apache 2.0.
