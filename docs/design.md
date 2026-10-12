# Design notes

Why the benchmark set is shaped the way it is, and what was deliberately left
out. The paper argues this at length; this file is the short version for
somebody about to add a benchmark.

## Three concepts that were being conflated

An earlier draft of this suite listed twelve "real-world workload categories".
They overlapped badly: `npm install` appeared in both *fresh environment* and
*dependency installation*, and *agent coding workflow* contained nearly every
other category. The cause was mixing three different things under one word.

- **Dimension** — the system capability being measured.
- **Workload** — the task that generates the load.
- **Profile** — the lifecycle conditions it runs under.

Fix the dimensions first, then pick the smallest set of workloads that covers
them, then treat duration and concurrency as profile parameters rather than as
new benchmarks.

## Five dimensions

Lifecycle, execution, compute, storage, network.

**Scalability is not the sixth.** It is the cluster scale of the other five.
Keeping it as a peer row double-counts: a concurrent-I/O slowdown would be
scored once as storage and again as scalability, and any composite ranking
would weight that behaviour twice.

**Orthogonal does not mean uncorrelated.** `npm ci` depends on network, CPU
and filesystem together, so it cannot be a pure test of any one dimension.
What the design requires is weaker and achievable:

- each dimension measures a different thing;
- each microbenchmark isolates one dominant bottleneck;
- each result answers a question no other result answers;
- no single underlying behaviour is scored more than once.

## What was cut, and why

| Candidate | Decision | Reason |
|---|---|---|
| `npm install` / `pip install` / `cargo fetch` | merged | all environment preparation; v0.1 picks one |
| `node` / `python` / shell repeated exec | merged | one minimal command is enough to measure the exec path |
| `npm build` / `cargo build` | merged | both are compute-bound builds; v0.1 picks one |
| Browser automation | deferred | the target is general sandboxes, not a browser product |
| Data processing | deferred | re-measures compute, storage and network without isolating any |
| Parallel execution | removed as a category | it is a concurrency *parameter* |
| Long-running execution | removed as a category | it is a duration *parameter* |
| Model inference | out of scope | would rank the accelerator, not the sandbox |
| Model training | out of scope | same |
| Noisy neighbour | kept | the only multi-tenant isolation signal |
| Stateful continuation | kept, conditional | runs only where suspend and resume exist |

## Application workloads are reported separately

Four of them: prepare, build, develop, continue. They mix dimensions on
purpose — they describe what a user waits for. Their numbers never fold into a
per-dimension score, because a slow `npm ci` cannot be attributed to one
resource.

None of them calls a language model. Model latency and output
non-determinism would be added to a measurement of infrastructure, and would
dominate it.

## The RL profile reuses the dimensions

A reset is lifecycle plus storage. A step is execution. A rollout is cluster
scale. So the profile adds benchmarks, not dimensions.

Three facts make it worth having at all:

1. **A reset is not necessarily a restart.** It can be in-process, a sandbox
   recreate, or a snapshot restore. The suite runs all three as separate
   benchmarks and never averages them, because they are different operations
   with different costs.
2. **Step latency is transport-bound.** The synthetic environment does a fixed,
   tiny amount of work per step, so what the series measures is the sandbox's
   own scheduling and RPC overhead. That is what an RL training loop waits on.
3. **Throughput is per valid rollout.** An episode that breaks partway costs
   the trainer the whole episode, so the suite reports the intact-episode rate
   beside the transitions-per-second figure.

## Lifecycle profiles

| Profile | Shape | Typical use |
|---|---|---|
| ephemeral | create → run → destroy | one-shot code execution |
| persistent | create → run × n → destroy | agent session, dev server |
| recoverable | create → run → checkpoint → resume | stateful agent, RL environment |
| pooled | prewarm → acquire → run → release | high-frequency RL rollouts |

The same workload runs under several of these. That is why duration and
concurrency are run parameters.

## The metrics worth arguing about

Three carry more information than cold start or IOPS:

- **Cost per successfully completed task** — charges startup overhead,
  execution time and failed retries to the work they were spent on.
- **Sustained useful throughput** — operations per second a long-lived sandbox
  completes while still meeting a tail-latency bound. A provider that starts
  fast and degrades after thirty minutes scores badly here and well on cold
  start.
- **Cost per valid rollout** — the RL analogue.

## Adding a benchmark

Before you write it, answer these:

1. Which dimension does it measure, and what is its dominant bottleneck?
2. What question does it answer that no existing benchmark answers? If
   deleting it would lose nothing, do not add it.
3. Is it actually a parameter of an existing benchmark — a different
   concurrency, duration or observation size?
4. Which providers can run it, and does it need a `Requirements` entry so the
   rest report `unsupported` instead of failing?
5. Does it measure compute? Then it must use the in-sandbox timing helper, not
   the runner's stopwatch.
