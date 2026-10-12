# Contributing

Two kinds of contribution matter most right now: provider adapters, and
verification of the adapters that exist.

## Verifying an adapter

Every hosted adapter here was written from a published API specification and
has never been run against a live service. `docs/providers.md` lists, per
adapter, which lines rest on a document and which rest on an inference.

If you have a key for one of them:

1. Run `configs/v01-single.yaml` with a low repetition count against that one
   provider.
2. Fix whatever breaks.
3. Move the affected row of `docs/providers.md` from "needs checking" to
   verified, and say what you ran.

That is the highest-value change anyone can make to this repository today.

## Adding a provider adapter

Implement `sandbox.Provider` and `sandbox.Sandbox`, register in an `init`, and
add a blank import to `cmd/sandboxbench/main.go`. Read
`providers/local/local.go` first; it is the shortest complete example.

Four rules the rest of the framework depends on:

1. **Return `sandbox.ErrUnsupported`** from `Suspend` and `Resume` when the
   provider has no such operation, and report it in `Capabilities`. Do not
   emulate a suspend with a destroy-and-recreate: the benchmark exists to tell
   those apart.
2. **`Create` must not return until the sandbox can run a command.** Poll
   inside `Create`. A provider that reports "running" before it can execute
   has moved its startup cost into the user's first command, and the
   measurement should contain it either way.
3. **A non-zero exit is a result, not an error.** Return it in
   `ExecResult.ExitCode` with a nil error. Reserve the error for a transport
   or API failure, because workloads branch on that difference.
4. **Destroy must work on a cancelled run.** The runner tears down on a
   context derived with `context.WithoutCancel`; do not hold state that makes
   that impossible.

State the exec transport in `Capabilities`. It is reported in every table, and
it is the single biggest determinant of per-command latency.

## Adding a benchmark

`docs/design.md` ends with five questions to answer before writing one. The
shortest version: if deleting the benchmark would lose no information, it does
not belong in the suite, and if it is only a different concurrency or duration
of an existing benchmark, it is a parameter rather than a benchmark.

## Before you open a pull request

```sh
make lint     # gofmt, go vet, go test
make smoke    # the whole suite against the local adapter, no credentials
```

## Results

Do not add benchmark results to this repository until the adapter that
produced them is verified and the run's client location, provider
configuration and date are recorded with them. A number without its
configuration is not comparable with anything, and publishing one is worse
than publishing nothing.
