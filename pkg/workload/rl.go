package workload

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/metrics"
	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
)

// The RL profile measures a sandbox as a reinforcement-learning environment
// rather than as a compute host. The policy stays outside; the sandbox only
// resets, steps and returns observations. That keeps the measurement about the
// infrastructure instead of about whichever accelerator the provider happens
// to attach.
//
// Three facts make this profile different from the other dimensions:
//
//  1. An environment reset is not necessarily a sandbox restart. Some
//     environments reset in-process, some need a fresh container, some restore
//     a snapshot. Reporting one number for all three compares nothing, so the
//     reset mechanism is recorded with the result.
//  2. Step latency is dominated by the exec transport, not by the step's own
//     compute. That is the number an RL training loop actually waits on.
//  3. Throughput matters per valid rollout, not per request. A rollout that
//     fails partway through costs the trainer the whole episode.

// envScript is the synthetic environment. Step compute is fixed and small, so
// the measured step latency is the sandbox's own overhead, which is what the
// RL profile is trying to isolate. Observation size is a parameter because a
// larger observation shifts the cost from scheduling to data transfer.
const envScript = `#!/bin/sh
# Synthetic RL environment. Usage: env.sh reset | step <obs_bytes>
STATE=/tmp/sandboxbench-rl.state
case "$1" in
  reset)
    echo 0 > "$STATE"
    ;;
  step)
    n=$(cat "$STATE" 2>/dev/null || echo 0)
    n=$((n+1))
    echo "$n" > "$STATE"
    i=0
    while [ $i -lt 2000 ]; do i=$((i+1)); done
    head -c "${2:-256}" /dev/zero | tr '\0' 'o'
    echo ""
    echo "step=$n"
    ;;
  *)
    echo "usage: env.sh reset|step [obs_bytes]" >&2
    exit 2
    ;;
esac
`

const envScriptPath = "/tmp/sandboxbench-env.sh"

// installEnv writes the synthetic environment into a sandbox.
func installEnv(ctx context.Context, box sandbox.Sandbox) error {
	if err := box.WriteFile(ctx, envScriptPath, []byte(envScript)); err != nil {
		return fmt.Errorf("write %s: %w", envScriptPath, err)
	}
	res, err := box.Exec(ctx, sandbox.Shell("chmod +x "+envScriptPath))
	if err != nil {
		return err
	}
	if !res.OK() {
		return errExit("chmod env script", res)
	}
	return nil
}

// EnvStep measures action-to-observation latency on a live environment.
type EnvStep struct {
	base
	ObsBytes int
}

// NewEnvStep builds the RL step benchmark.
func NewEnvStep(obsBytes int) *EnvStep {
	if obsBytes <= 0 {
		obsBytes = 256
	}
	return &EnvStep{
		base: base{
			name: "rl.env-step",
			dim:  DimExecution,
			sc:   ScaleSingle,
			desc: "Drive a synthetic RL environment one step at a time; reports action-to-observation latency.",
		},
		ObsBytes: obsBytes,
	}
}

// Run issues env.Repetitions steps against one environment.
func (w *EnvStep) Run(ctx context.Context, env Env, box sandbox.Sandbox) (Result, error) {
	r := newResult(w, env)
	r.Profile = ProfilePersistent
	r.Config = map[string]any{"obs_bytes": w.ObsBytes}

	if err := installEnv(ctx, box); err != nil {
		return fail(r, err)
	}
	if res, err := box.Exec(ctx, sandbox.Shell(envScriptPath+" reset")); err != nil {
		return fail(r, err)
	} else if !res.OK() {
		return fail(r, errExit("env reset", res))
	}

	step := metrics.NewSeries("step_latency", "ms")
	obs := strconv.Itoa(w.ObsBytes)
	start := time.Now()

	for i := 0; i < env.Repetitions; i++ {
		if err := ctx.Err(); err != nil {
			return fail(r, err)
		}
		t0 := time.Now()
		res, err := box.Exec(ctx, sandbox.Shell(envScriptPath+" step "+obs))
		if err != nil {
			r.Count("step_errors", 1)
			continue
		}
		if !res.OK() {
			r.Count("step_nonzero", 1)
			continue
		}
		step.AddDuration(time.Since(t0))
	}

	r.AddSeries(step)
	r.Count("steps", step.Len())
	r.SetScalar("steps_per_second", metrics.Throughput{Count: step.Len(), Elapsed: time.Since(start)}.PerSecond())
	r.Elapsed = time.Since(r.Started)
	if step.Len() == 0 {
		return fail(r, errors.New("no environment step completed"))
	}
	return r, nil
}

// EnvReset measures how long it takes to return an environment to its initial
// state, and records which mechanism did it.
type EnvReset struct {
	base
	// Mechanism is recorded with the result. Values: "in-process" resets by
	// running the environment's own reset, "recreate" destroys and recreates
	// the sandbox, "resume" restores a suspended sandbox.
	Mechanism string
}

// NewEnvReset builds the RL reset benchmark for one mechanism.
func NewEnvReset(mechanism string) *EnvReset {
	req := Requirements{}
	if mechanism == "resume" {
		req = Requirements{Suspend: true, Resume: true}
	}
	return &EnvReset{
		base: base{
			name: "rl.env-reset." + mechanism,
			dim:  DimLifecycle,
			sc:   ScaleSingle,
			desc: "Return an RL environment to its initial state via " + mechanism + "; reports reset latency.",
			req:  req,
		},
		Mechanism: mechanism,
	}
}

// RunProvider owns the sandbox because two of the three mechanisms change its
// lifecycle.
func (w *EnvReset) RunProvider(ctx context.Context, env Env, p sandbox.Provider) (Result, error) {
	r := newResult(w, env)
	r.Config = map[string]any{"mechanism": w.Mechanism}
	reset := metrics.NewSeries("reset_latency", "ms")

	switch w.Mechanism {
	case "in-process":
		box, err := p.Create(ctx, env.Create)
		if err != nil {
			return fail(r, err)
		}
		defer destroyQuietly(ctx, box)
		if err := installEnv(ctx, box); err != nil {
			return fail(r, err)
		}
		for i := 0; i < env.Repetitions; i++ {
			t0 := time.Now()
			res, err := box.Exec(ctx, sandbox.Shell(envScriptPath+" reset"))
			if err != nil || !res.OK() {
				r.Count("reset_errors", 1)
				continue
			}
			reset.AddDuration(time.Since(t0))
		}

	case "recreate":
		for i := 0; i < env.Repetitions; i++ {
			t0 := time.Now()
			box, err := p.Create(ctx, env.Create)
			if err != nil {
				r.Count("reset_errors", 1)
				continue
			}
			if _, err := box.Exec(ctx, sandbox.Shell(readyProbe)); err != nil {
				r.Count("reset_errors", 1)
				destroyQuietly(ctx, box)
				continue
			}
			reset.AddDuration(time.Since(t0))
			destroyQuietly(ctx, box)
		}

	case "resume":
		box, err := p.Create(ctx, env.Create)
		if err != nil {
			return fail(r, err)
		}
		defer destroyQuietly(ctx, box)
		for i := 0; i < env.Repetitions; i++ {
			if err := box.Suspend(ctx); err != nil {
				if errors.Is(err, sandbox.ErrUnsupported) {
					r.Status = sandbox.StatusUnsupported
					r.Reason = "provider reported suspend unsupported at run time"
					r.Elapsed = time.Since(r.Started)
					return r, nil
				}
				r.Count("suspend_errors", 1)
				continue
			}
			t0 := time.Now()
			if err := box.Resume(ctx); err != nil {
				r.Count("resume_errors", 1)
				continue
			}
			if _, err := box.Exec(ctx, sandbox.Shell(readyProbe)); err != nil {
				r.Count("probe_errors", 1)
				continue
			}
			reset.AddDuration(time.Since(t0))
		}

	default:
		return fail(r, fmt.Errorf("unknown reset mechanism %q", w.Mechanism))
	}

	r.AddSeries(reset)
	r.Elapsed = time.Since(r.Started)
	if reset.Len() == 0 {
		return fail(r, errors.New("no reset completed"))
	}
	return r, nil
}

// ParallelRollouts is the RL scalability benchmark. It runs env.Concurrency
// environments at once, each for a fixed episode, and reports transitions per
// second along with how many episodes finished intact.
type ParallelRollouts struct {
	base
	StepsPerEpisode int
	ObsBytes        int
}

// NewParallelRollouts builds the parallel rollout benchmark.
func NewParallelRollouts(steps, obsBytes int) *ParallelRollouts {
	if steps <= 0 {
		steps = 50
	}
	if obsBytes <= 0 {
		obsBytes = 256
	}
	return &ParallelRollouts{
		base: base{
			name: "rl.parallel-rollouts",
			dim:  DimExecution,
			sc:   ScaleCluster,
			desc: "Run N synthetic environments concurrently; reports transitions/s and valid-episode rate.",
		},
		StepsPerEpisode: steps,
		ObsBytes:        obsBytes,
	}
}

// RunProvider creates one sandbox per environment and runs one episode in each.
func (w *ParallelRollouts) RunProvider(ctx context.Context, env Env, p sandbox.Provider) (Result, error) {
	r := newResult(w, env)
	r.Config = map[string]any{
		"steps_per_episode": w.StepsPerEpisode,
		"obs_bytes":         w.ObsBytes,
		"environments":      env.Concurrency,
	}
	step := metrics.NewSeries("step_latency", "ms")
	episode := metrics.NewSeries("episode_duration", "ms")

	var (
		mu          sync.Mutex
		wg          sync.WaitGroup
		transitions int
		valid       int
		broken      int
	)
	obs := strconv.Itoa(w.ObsBytes)
	start := time.Now()

	for i := 0; i < env.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			box, err := p.Create(ctx, env.Create)
			if err != nil {
				mu.Lock()
				broken++
				mu.Unlock()
				return
			}
			defer destroyQuietly(ctx, box)

			if err := installEnv(ctx, box); err != nil {
				mu.Lock()
				broken++
				mu.Unlock()
				return
			}
			if res, err := box.Exec(ctx, sandbox.Shell(envScriptPath+" reset")); err != nil || !res.OK() {
				mu.Lock()
				broken++
				mu.Unlock()
				return
			}

			local := make([]time.Duration, 0, w.StepsPerEpisode)
			e0 := time.Now()
			intact := true
			for s := 0; s < w.StepsPerEpisode; s++ {
				if err := ctx.Err(); err != nil {
					intact = false
					break
				}
				t0 := time.Now()
				res, err := box.Exec(ctx, sandbox.Shell(envScriptPath+" step "+obs))
				if err != nil || !res.OK() {
					intact = false
					break
				}
				local = append(local, time.Since(t0))
			}
			d := time.Since(e0)

			mu.Lock()
			defer mu.Unlock()
			for _, l := range local {
				step.AddDuration(l)
			}
			transitions += len(local)
			if intact {
				valid++
				episode.AddDuration(d)
			} else {
				broken++
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	r.AddSeries(step)
	r.AddSeries(episode)
	r.Count("transitions", transitions)
	r.Count("valid_episodes", valid)
	r.Count("broken_episodes", broken)
	r.SetScalar("transitions_per_second", metrics.Throughput{Count: transitions, Elapsed: elapsed}.PerSecond())
	r.SetScalar("episodes_per_second", metrics.Throughput{Count: valid, Elapsed: elapsed}.PerSecond())
	if env.Concurrency > 0 {
		r.SetScalar("valid_episode_rate", float64(valid)/float64(env.Concurrency))
	}
	r.Elapsed = time.Since(r.Started)
	if transitions == 0 {
		return fail(r, errors.New("no transition completed"))
	}
	return r, nil
}

// destroyQuietly tears a sandbox down on a context that outlives the run, so a
// cancelled or timed-out run still releases what it allocated.
func destroyQuietly(ctx context.Context, box sandbox.Sandbox) {
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	_ = box.Destroy(tctx)
}
