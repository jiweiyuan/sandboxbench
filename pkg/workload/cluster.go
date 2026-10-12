package workload

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/metrics"
	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
)

// BurstProvisioning is B14. It asks for env.Concurrency sandboxes at once and
// reports how many reach first exec, how fast, and how many fail.
//
// The headline number is not requests accepted per second. It is ready
// sandboxes per second: a control plane that accepts a thousand creates and
// then queues them has not provisioned a thousand sandboxes.
type BurstProvisioning struct{ base }

// NewBurstProvisioning builds B14.
func NewBurstProvisioning() *BurstProvisioning {
	return &BurstProvisioning{base{
		name: "scalability.burst",
		dim:  DimLifecycle,
		sc:   ScaleCluster,
		desc: "Request N sandboxes simultaneously; reports ready throughput, p99 ready latency and success rate.",
	}}
}

// RunProvider issues all creates at once and waits for every one to settle.
func (w *BurstProvisioning) RunProvider(ctx context.Context, env Env, p sandbox.Provider) (Result, error) {
	r := newResult(w, env)
	ready := metrics.NewSeries("burst_ready", "ms")

	var (
		mu     sync.Mutex
		boxes  []sandbox.Sandbox
		wg     sync.WaitGroup
		failed int
	)

	start := time.Now()
	for i := 0; i < env.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			box, err := p.Create(ctx, env.Create)
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			if _, err := box.Exec(ctx, sandbox.Shell(readyProbe)); err != nil {
				_ = box.Destroy(ctx)
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			d := time.Since(t0)
			mu.Lock()
			ready.AddDuration(d)
			boxes = append(boxes, box)
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// Teardown is measured too. A provider that leaks sandboxes after a burst
	// costs the user money, so cleanup failures belong in the result.
	var teardownErrors int
	var twg sync.WaitGroup
	for _, box := range boxes {
		twg.Add(1)
		go func(b sandbox.Sandbox) {
			defer twg.Done()
			// Teardown must run even when the run context is already done.
			tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			if err := b.Destroy(tctx); err != nil {
				mu.Lock()
				teardownErrors++
				mu.Unlock()
			}
		}(box)
	}
	twg.Wait()

	r.AddSeries(ready)
	r.Count("requested", env.Concurrency)
	r.Count("ready", ready.Len())
	r.Count("create_failures", failed)
	r.Count("teardown_failures", teardownErrors)
	r.SetScalar("ready_per_second", metrics.Throughput{Count: ready.Len(), Elapsed: elapsed}.PerSecond())
	if env.Concurrency > 0 {
		r.SetScalar("success_rate", float64(ready.Len())/float64(env.Concurrency))
	}
	r.Elapsed = time.Since(r.Started)
	if ready.Len() == 0 {
		return fail(r, errors.New("no sandbox in the burst reached first exec"))
	}
	return r, nil
}

// SustainedProvisioning is B14's open-loop counterpart. It drives a fixed
// arrival rate instead of a single burst, which is the shape that finds the
// maximum rate a provider sustains under an SLO.
type SustainedProvisioning struct{ base }

// NewSustainedProvisioning builds the open-loop provisioning benchmark.
func NewSustainedProvisioning() *SustainedProvisioning {
	return &SustainedProvisioning{base{
		name: "scalability.sustained",
		dim:  DimLifecycle,
		sc:   ScaleCluster,
		desc: "Create sandboxes at a fixed arrival rate for a fixed duration; reports sustained ready throughput.",
	}}
}

// RunProvider sends creates on a ticker and never waits for the previous one,
// so a slow provider shows up as a growing latency tail rather than as a
// quietly reduced offered load. That distinction is the whole point of
// open-loop generation.
func (w *SustainedProvisioning) RunProvider(ctx context.Context, env Env, p sandbox.Provider) (Result, error) {
	r := newResult(w, env)
	if env.Rate <= 0 {
		return fail(r, errors.New("sustained provisioning needs a positive rate"))
	}
	ready := metrics.NewSeries("sustained_ready", "ms")

	interval := time.Duration(float64(time.Second) / env.Rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.Now().Add(env.Duration)

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		issued   int
		failures int
		leaked   int
	)

	start := time.Now()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			wg.Wait()
			return fail(r, ctx.Err())
		case <-ticker.C:
		}
		issued++
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			box, err := p.Create(ctx, env.Create)
			if err != nil {
				mu.Lock()
				failures++
				mu.Unlock()
				return
			}
			_, execErr := box.Exec(ctx, sandbox.Shell(readyProbe))
			d := time.Since(t0)

			tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			destroyErr := box.Destroy(tctx)

			mu.Lock()
			defer mu.Unlock()
			if execErr != nil {
				failures++
			} else {
				ready.AddDuration(d)
			}
			if destroyErr != nil {
				leaked++
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	r.AddSeries(ready)
	r.Count("issued", issued)
	r.Count("ready", ready.Len())
	r.Count("failures", failures)
	r.Count("teardown_failures", leaked)
	r.SetScalar("offered_rate_per_second", env.Rate)
	r.SetScalar("achieved_ready_per_second", metrics.Throughput{Count: ready.Len(), Elapsed: elapsed}.PerSecond())
	if issued > 0 {
		r.SetScalar("success_rate", float64(ready.Len())/float64(issued))
	}
	r.Elapsed = time.Since(r.Started)
	if ready.Len() == 0 {
		return fail(r, errors.New("no sandbox reached first exec at the offered rate"))
	}
	return r, nil
}

// NoisyNeighbour is B16. It runs the same fixed CPU work alone and then beside
// interference, and reports the slowdown between the two.
type NoisyNeighbour struct {
	base
	// Interferers is how many extra sandboxes generate load. It comes from
	// env.Concurrency at construction time.
	Interferers int
}

// NewNoisyNeighbour builds B16 with a given number of interfering sandboxes.
func NewNoisyNeighbour(interferers int) *NoisyNeighbour {
	return &NoisyNeighbour{
		base: base{
			name: "scalability.isolation",
			dim:  DimCompute,
			sc:   ScaleCluster,
			desc: "Measure fixed CPU work alone, then beside N interfering sandboxes; reports slowdown.",
		},
		Interferers: interferers,
	}
}

// interferenceLine is a CPU and I/O load generator built only from shell
// builtins and coreutils, so it needs nothing installed in the image.
const interferenceLine = "end=$(( $(date +%s) + 600 )); " +
	"while [ $(date +%s) -lt $end ]; do i=0; while [ $i -lt 200000 ]; do i=$((i+1)); done; " +
	"dd if=/dev/zero of=/tmp/sbb-noise bs=1M count=32 2>/dev/null; done"

// RunProvider measures the baseline, starts the interference, measures again,
// and tears everything down.
func (w *NoisyNeighbour) RunProvider(ctx context.Context, env Env, p sandbox.Provider) (Result, error) {
	r := newResult(w, env)
	r.Config = map[string]any{"interferers": w.Interferers}

	victim, err := p.Create(ctx, env.Create)
	if err != nil {
		return fail(r, err)
	}
	defer func() {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		_ = victim.Destroy(tctx)
	}()

	cpu := NewCPUBuild()
	baselineEnv := env
	baselineEnv.Repetitions = max(1, env.Repetitions)
	baseline, err := cpu.Run(ctx, baselineEnv, victim)
	if err != nil {
		return fail(r, err)
	}
	if baseline.Status != sandbox.StatusCompleted {
		r.Status = baseline.Status
		r.Reason = "baseline compute measurement: " + baseline.Reason
		r.Elapsed = time.Since(r.Started)
		return r, nil
	}

	var (
		mu     sync.Mutex
		noise  []sandbox.Sandbox
		wg     sync.WaitGroup
		noiseF int
	)
	for i := 0; i < w.Interferers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			box, err := p.Create(ctx, env.Create)
			if err != nil {
				mu.Lock()
				noiseF++
				mu.Unlock()
				return
			}
			mu.Lock()
			noise = append(noise, box)
			mu.Unlock()
			// The interference command runs until its own timeout; the result
			// is discarded on purpose.
			_, _ = box.Exec(ctx, sandbox.Command{
				Cmd:     "/bin/sh",
				Args:    []string{"-lc", interferenceLine},
				Timeout: 10 * time.Minute,
			})
		}()
	}
	// Give the interference time to reach steady state before measuring again.
	time.Sleep(5 * time.Second)

	contended, cerr := cpu.Run(ctx, baselineEnv, victim)

	for _, box := range noise {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		_ = box.Destroy(tctx)
		cancel()
	}
	wg.Wait()

	if cerr != nil {
		return fail(r, cerr)
	}

	bp50 := seriesP50(baseline, "run")
	cp50 := seriesP50(contended, "run")
	r.SetScalar("baseline_run_p50_ms", bp50)
	r.SetScalar("contended_run_p50_ms", cp50)
	r.SetScalar("slowdown", metrics.Slowdown(bp50, cp50))
	r.Count("interferers_started", len(noise))
	r.Count("interferer_failures", noiseF)
	r.Series = append(r.Series, baseline.Series...)
	r.Series = append(r.Series, contended.Series...)
	r.Elapsed = time.Since(r.Started)
	if bp50 == 0 || cp50 == 0 {
		return fail(r, errors.New("missing baseline or contended compute sample"))
	}
	return r, nil
}

// seriesP50 reads one named series median out of a result, or 0 when absent.
func seriesP50(r Result, name string) float64 {
	for _, s := range r.Series {
		if s.Name == name {
			return s.P50
		}
	}
	return 0
}
