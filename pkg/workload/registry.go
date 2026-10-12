package workload

import (
	"fmt"
	"sort"

	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
)

// Builder constructs a workload from the optional `with:` block of a run
// configuration entry.
type Builder func(with sandbox.Cfg) (Workload, error)

// registry is fixed at init time, so the benchmark set a binary can run is
// visible from `sandboxbench list`.
var registry = map[string]Builder{}

func register(name string, b Builder) {
	if _, dup := registry[name]; dup {
		panic("sandboxbench: duplicate workload " + name)
	}
	registry[name] = b
}

// Lookup builds a registered workload.
func Lookup(name string, with sandbox.Cfg) (Workload, error) {
	b, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("sandboxbench: unknown benchmark %q (have %v)", name, Names())
	}
	return b(with)
}

// Names lists every registered benchmark in sorted order.
func Names() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The v0.1 set. Ten benchmarks, chosen so that each one answers a question no
// other one answers. Browser automation, data processing, model inference and
// model training are deliberately out: the first two duplicate compute,
// storage and network, and the last two would make the accelerator, not the
// sandbox, the thing being compared.
func init() {
	register("lifecycle.cold-create", func(sandbox.Cfg) (Workload, error) {
		return NewColdCreate(), nil
	})
	register("lifecycle.resume", func(sandbox.Cfg) (Workload, error) {
		return NewResumeLatency(), nil
	})
	register("lifecycle.churn", func(sandbox.Cfg) (Workload, error) {
		return NewLifecycleChurn(), nil
	})
	register("execution.short-command", func(sandbox.Cfg) (Workload, error) {
		return NewShortCommand(), nil
	})
	register("compute.cpu-build", func(sandbox.Cfg) (Workload, error) {
		return NewCPUBuild(), nil
	})
	register("storage.small-files", func(sandbox.Cfg) (Workload, error) {
		return NewSmallFiles(), nil
	})
	register("network.egress", func(with sandbox.Cfg) (Workload, error) {
		endpoint := with.String("endpoint", "https://speed.cloudflare.com/__down?bytes=10485760")
		return NewNetworkEgress(endpoint), nil
	})
	register("scalability.burst", func(sandbox.Cfg) (Workload, error) {
		return NewBurstProvisioning(), nil
	})
	register("scalability.sustained", func(sandbox.Cfg) (Workload, error) {
		return NewSustainedProvisioning(), nil
	})
	register("scalability.isolation", func(with sandbox.Cfg) (Workload, error) {
		return NewNoisyNeighbour(with.Int("interferers", 8)), nil
	})

	// RL profile. Reset, step and rollout reuse the dimensions above rather
	// than adding new top-level ones: a reset is lifecycle plus storage, a
	// step is execution, a rollout is cluster scale.
	register("rl.env-step", func(with sandbox.Cfg) (Workload, error) {
		return NewEnvStep(with.Int("obs_bytes", 256)), nil
	})
	register("rl.env-reset.in-process", func(sandbox.Cfg) (Workload, error) {
		return NewEnvReset("in-process"), nil
	})
	register("rl.env-reset.recreate", func(sandbox.Cfg) (Workload, error) {
		return NewEnvReset("recreate"), nil
	})
	register("rl.env-reset.resume", func(sandbox.Cfg) (Workload, error) {
		return NewEnvReset("resume"), nil
	})
	register("rl.parallel-rollouts", func(with sandbox.Cfg) (Workload, error) {
		return NewParallelRollouts(with.Int("steps", 50), with.Int("obs_bytes", 256)), nil
	})

	// Application workloads. app.custom reads its whole definition from the
	// configuration file, because the repository and commit must be pinned by
	// whoever runs the benchmark.
	register("app.stateful-continue", func(sandbox.Cfg) (Workload, error) {
		return NewStatefulContinue(), nil
	})
	register("app.custom", func(with sandbox.Cfg) (Workload, error) {
		spec, err := appSpecFrom(with)
		if err != nil {
			return nil, err
		}
		return NewApplication(spec), nil
	})
}

// appSpecFrom reads an AppSpec out of a `with:` block.
func appSpecFrom(with sandbox.Cfg) (AppSpec, error) {
	spec := AppSpec{
		Name: with.String("name", "custom"),
		Repo: with.String("repo", ""),
		Ref:  with.String("ref", ""),
		Dir:  with.String("dir", ""),
	}
	raw, ok := with["steps"].([]any)
	if !ok {
		return spec, fmt.Errorf("app.custom needs a steps list in its with: block")
	}
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			return spec, fmt.Errorf("app.custom step %d is not a mapping", i)
		}
		label, _ := m["label"].(string)
		line, _ := m["line"].(string)
		if label == "" || line == "" {
			return spec, fmt.Errorf("app.custom step %d needs both label and line", i)
		}
		spec.Steps = append(spec.Steps, Step{Label: label, Line: line})
	}
	return spec, spec.Validate()
}
