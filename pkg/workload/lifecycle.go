package workload

import (
	"context"
	"errors"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/metrics"
	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
)

// readyProbe is the cheapest command that proves a sandbox can run code. Cold
// create latency is measured to the completion of this probe, not to the
// moment the control plane reports "running", because a provider that returns
// early shifts its startup cost into the user's first command.
const readyProbe = "true"

// ColdCreate is B01. It measures create-to-first-exec latency.
type ColdCreate struct{ base }

// NewColdCreate builds B01.
func NewColdCreate() *ColdCreate {
	return &ColdCreate{base{
		name: "lifecycle.cold-create",
		dim:  DimLifecycle,
		sc:   ScaleSingle,
		desc: "Create a fresh sandbox and run the first command; reports create-to-first-exec latency.",
	}}
}

// RunProvider creates and destroys one sandbox per repetition.
func (w *ColdCreate) RunProvider(ctx context.Context, env Env, p sandbox.Provider) (Result, error) {
	r := newResult(w, env)
	create := metrics.NewSeries("create_to_ready", "ms")
	destroy := metrics.NewSeries("destroy", "ms")

	for i := 0; i < env.Repetitions; i++ {
		if err := ctx.Err(); err != nil {
			return fail(r, err)
		}
		t0 := time.Now()
		box, err := p.Create(ctx, env.Create)
		if err != nil {
			r.Count("create_errors", 1)
			env.Log("cold-create rep %d: create failed: %v", i, err)
			continue
		}
		if _, err := box.Exec(ctx, sandbox.Shell(readyProbe)); err != nil {
			r.Count("probe_errors", 1)
			env.Log("cold-create rep %d: ready probe failed: %v", i, err)
			_ = box.Destroy(ctx)
			continue
		}
		create.AddDuration(time.Since(t0))

		t1 := time.Now()
		if err := box.Destroy(ctx); err != nil {
			r.Count("destroy_errors", 1)
			env.Log("cold-create rep %d: destroy failed: %v", i, err)
			continue
		}
		destroy.AddDuration(time.Since(t1))
		r.Count("ok", 1)
	}

	r.AddSeries(create)
	r.AddSeries(destroy)
	if create.Len() == 0 {
		return fail(r, errors.New("no repetition reached first exec"))
	}
	r.Elapsed = time.Since(r.Started)
	return r, nil
}

// ResumeLatency is B02. It measures suspend-to-ready latency and checks that
// the filesystem survived the round trip.
type ResumeLatency struct{ base }

// NewResumeLatency builds B02.
func NewResumeLatency() *ResumeLatency {
	return &ResumeLatency{base{
		name: "lifecycle.resume",
		dim:  DimLifecycle,
		sc:   ScaleSingle,
		desc: "Suspend a sandbox and resume it; reports resume-to-ready latency and state correctness.",
		req:  Requirements{Suspend: true, Resume: true},
	}}
}

// sentinel is written before the suspend and read after the resume. Resume
// latency without a correctness check is not a useful number: a provider that
// silently cold-boots a fresh sandbox would score very well on it.
const sentinelPath = "/tmp/sandboxbench-sentinel"

// RunProvider suspends and resumes one sandbox per repetition.
func (w *ResumeLatency) RunProvider(ctx context.Context, env Env, p sandbox.Provider) (Result, error) {
	r := newResult(w, env)
	r.Profile = ProfileRecoverable
	suspend := metrics.NewSeries("suspend", "ms")
	resume := metrics.NewSeries("resume_to_ready", "ms")

	for i := 0; i < env.Repetitions; i++ {
		if err := ctx.Err(); err != nil {
			return fail(r, err)
		}
		box, err := p.Create(ctx, env.Create)
		if err != nil {
			r.Count("create_errors", 1)
			continue
		}
		token := time.Now().UTC().Format(time.RFC3339Nano)
		if err := box.WriteFile(ctx, sentinelPath, []byte(token)); err != nil {
			r.Count("write_errors", 1)
			_ = box.Destroy(ctx)
			continue
		}

		t0 := time.Now()
		err = box.Suspend(ctx)
		if errors.Is(err, sandbox.ErrUnsupported) {
			_ = box.Destroy(ctx)
			r.Status = sandbox.StatusUnsupported
			r.Reason = "provider reported suspend unsupported at run time"
			r.Elapsed = time.Since(r.Started)
			return r, nil
		}
		if err != nil {
			r.Count("suspend_errors", 1)
			_ = box.Destroy(ctx)
			continue
		}
		suspend.AddDuration(time.Since(t0))

		t1 := time.Now()
		if err := box.Resume(ctx); err != nil {
			r.Count("resume_errors", 1)
			_ = box.Destroy(ctx)
			continue
		}
		if _, err := box.Exec(ctx, sandbox.Shell(readyProbe)); err != nil {
			r.Count("probe_errors", 1)
			_ = box.Destroy(ctx)
			continue
		}
		resume.AddDuration(time.Since(t1))

		got, err := box.ReadFile(ctx, sentinelPath)
		switch {
		case err != nil:
			r.Count("state_read_errors", 1)
		case string(got) != token:
			r.Count("state_mismatch", 1)
		default:
			r.Count("state_preserved", 1)
		}
		_ = box.Destroy(ctx)
		r.Count("ok", 1)
	}

	r.AddSeries(suspend)
	r.AddSeries(resume)
	if resume.Len() == 0 {
		return fail(r, errors.New("no repetition completed a resume"))
	}
	r.Elapsed = time.Since(r.Started)
	return r, nil
}

// LifecycleChurn is B03. It creates and destroys sandboxes back to back and
// watches for latency drift and leaked resources.
type LifecycleChurn struct{ base }

// NewLifecycleChurn builds B03.
func NewLifecycleChurn() *LifecycleChurn {
	return &LifecycleChurn{base{
		name: "lifecycle.churn",
		dim:  DimLifecycle,
		sc:   ScaleCluster,
		desc: "Create and destroy sandboxes continuously; reports latency drift and cleanup failures.",
	}}
}

// RunProvider holds env.Concurrency sandboxes alive and replaces each one as
// soon as it is destroyed, for env.Duration.
func (w *LifecycleChurn) RunProvider(ctx context.Context, env Env, p sandbox.Provider) (Result, error) {
	r := newResult(w, env)
	cycle := metrics.NewSeries("create_destroy_cycle", "ms")

	deadline := time.Now().Add(env.Duration)
	results := make(chan time.Duration, env.Concurrency)
	errs := make(chan error, env.Concurrency)
	done := make(chan struct{})

	for i := 0; i < env.Concurrency; i++ {
		go func() {
			for time.Now().Before(deadline) {
				select {
				case <-ctx.Done():
					return
				case <-done:
					return
				default:
				}
				t0 := time.Now()
				box, err := p.Create(ctx, env.Create)
				if err != nil {
					errs <- err
					continue
				}
				if _, err := box.Exec(ctx, sandbox.Shell(readyProbe)); err != nil {
					errs <- err
					_ = box.Destroy(ctx)
					continue
				}
				if err := box.Destroy(ctx); err != nil {
					errs <- err
					continue
				}
				results <- time.Since(t0)
			}
			errs <- nil
		}()
	}

	finished := 0
	for finished < env.Concurrency {
		select {
		case d := <-results:
			cycle.AddDuration(d)
		case err := <-errs:
			if err == nil {
				finished++
				continue
			}
			r.Count("cycle_errors", 1)
		case <-ctx.Done():
			close(done)
			return fail(r, ctx.Err())
		}
	}
	// Drain the cycles that landed after the last worker reported.
	for {
		select {
		case d := <-results:
			cycle.AddDuration(d)
			continue
		default:
		}
		break
	}

	r.AddSeries(cycle)
	r.Count("cycles", cycle.Len())
	r.Elapsed = time.Since(r.Started)
	r.SetScalar("cycles_per_second", metrics.Throughput{Count: cycle.Len(), Elapsed: r.Elapsed}.PerSecond())
	if cycle.Len() == 0 {
		return fail(r, errors.New("no create/destroy cycle completed"))
	}
	return r, nil
}
