// Package workload defines what SandboxBench measures.
//
// The taxonomy is five base performance dimensions crossed with two execution
// scales, plus end-to-end application workloads that are reported separately
// and never folded into a dimension score. Scalability is deliberately not a
// sixth resource dimension: it is the cluster scale of the other five, and
// treating it as a peer dimension would score the same underlying behaviour
// twice. See docs/design.md.
package workload

import (
	"context"
	"fmt"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/metrics"
	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
)

// Dimension is the system capability a benchmark measures.
type Dimension string

const (
	DimLifecycle Dimension = "lifecycle"
	DimExecution Dimension = "execution"
	DimCompute   Dimension = "compute"
	DimStorage   Dimension = "storage"
	DimNetwork   Dimension = "network"
	// DimApplication marks an end-to-end workload. Its numbers describe user
	// experience and are not attributable to one resource, so they stay out of
	// the per-dimension tables.
	DimApplication Dimension = "application"
)

// Scale separates a measurement of one sandbox from a measurement of many.
type Scale string

const (
	ScaleSingle  Scale = "single"
	ScaleCluster Scale = "cluster"
)

// Profile is the lifecycle pattern a workload is driven under. The same
// workload can run under several profiles, which is why duration and
// concurrency are run parameters rather than separate benchmarks.
type Profile string

const (
	ProfileEphemeral   Profile = "ephemeral"   // create, run once, destroy
	ProfilePersistent  Profile = "persistent"  // create, run many times, destroy
	ProfileRecoverable Profile = "recoverable" // create, run, checkpoint, resume
	ProfilePooled      Profile = "pooled"      // prewarm, acquire, run, release
)

// Requirements states what a workload needs from a provider. The runner checks
// it before the workload runs and reports StatusUnsupported without creating
// any sandbox when it is not met, so an unsupported benchmark costs nothing.
type Requirements struct {
	Suspend    bool
	Resume     bool
	Snapshot   bool
	Network    bool
	PersistFS  bool
	PortExpose bool
}

// Unmet names the first requirement a provider does not meet, or "".
func (r Requirements) Unmet(c sandbox.Capabilities) string {
	switch {
	case r.Suspend && !c.Suspend:
		return "suspend"
	case r.Resume && !c.Resume:
		return "resume"
	case r.Snapshot && !c.Snapshot:
		return "snapshot"
	case r.PersistFS && !c.PersistentFilesystem:
		return "persistent filesystem"
	case r.PortExpose && !c.PortForwarding:
		return "port forwarding"
	}
	return ""
}

// Env is the run configuration handed to every workload.
type Env struct {
	// Repetitions is how many times a latency sample is taken.
	Repetitions int
	// Concurrency is the closed-loop worker count for cluster workloads.
	Concurrency int
	// Rate is the open-loop arrival rate in requests per second. Zero selects
	// closed-loop generation instead.
	Rate float64
	// Duration bounds a steady-state cluster workload.
	Duration time.Duration
	// Create is passed to Provider.Create for every sandbox in the run.
	Create sandbox.CreateOptions
	// Profile is the lifecycle pattern being exercised.
	Profile Profile
	// Logf receives progress lines. It is never nil inside a workload.
	Logf func(format string, args ...any)
}

// Log writes a progress line.
func (e Env) Log(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// Result is one benchmark's output.
type Result struct {
	Workload  string               `json:"workload"`
	Dimension Dimension            `json:"dimension"`
	Scale     Scale                `json:"scale"`
	Profile   Profile              `json:"profile,omitempty"`
	Status    sandbox.Status       `json:"status"`
	Reason    string               `json:"reason,omitempty"`
	Series    []metrics.Summary    `json:"series,omitempty"`
	Scalars   map[string]float64   `json:"scalars,omitempty"`
	Counters  map[string]int       `json:"counters,omitempty"`
	Config    map[string]any       `json:"config,omitempty"`
	Raw       map[string][]float64 `json:"raw,omitempty"`
	Started   time.Time            `json:"started"`
	Elapsed   time.Duration        `json:"elapsed_ns"`
}

// AddSeries summarizes a series into the result and keeps its raw samples so a
// reader can recompute any statistic from the result file.
func (r *Result) AddSeries(s *metrics.Series) {
	if s == nil || s.Len() == 0 {
		return
	}
	r.Series = append(r.Series, s.Summarize())
	if r.Raw == nil {
		r.Raw = map[string][]float64{}
	}
	r.Raw[s.Name] = s.Samples
}

// SetScalar records a single derived number, such as a throughput or a
// slowdown ratio.
func (r *Result) SetScalar(name string, v float64) {
	if r.Scalars == nil {
		r.Scalars = map[string]float64{}
	}
	r.Scalars[name] = v
}

// Count records a count, such as successes or cleanup failures.
func (r *Result) Count(name string, n int) {
	if r.Counters == nil {
		r.Counters = map[string]int{}
	}
	r.Counters[name] += n
}

// Workload is the metadata every benchmark carries.
type Workload interface {
	Name() string
	Dimension() Dimension
	Scale() Scale
	Describe() string
	Requires() Requirements
}

// SandboxWorkload runs inside one sandbox the runner has already created and
// confirmed ready. Its measurements exclude create latency.
type SandboxWorkload interface {
	Workload
	Run(ctx context.Context, env Env, box sandbox.Sandbox) (Result, error)
}

// ProviderWorkload owns sandbox lifecycle itself. Create latency, burst
// provisioning and noisy-neighbour contention can only be measured this way.
type ProviderWorkload interface {
	Workload
	RunProvider(ctx context.Context, env Env, p sandbox.Provider) (Result, error)
}

// base supplies the Workload metadata that every benchmark in this package
// shares, so each one only writes what makes it different.
type base struct {
	name string
	dim  Dimension
	sc   Scale
	desc string
	req  Requirements
}

func (b base) Name() string           { return b.name }
func (b base) Dimension() Dimension   { return b.dim }
func (b base) Scale() Scale           { return b.sc }
func (b base) Describe() string       { return b.desc }
func (b base) Requires() Requirements { return b.req }

// newResult starts a Result already stamped with the workload's identity.
func newResult(w Workload, env Env) Result {
	return Result{
		Workload:  w.Name(),
		Dimension: w.Dimension(),
		Scale:     w.Scale(),
		Profile:   env.Profile,
		Status:    sandbox.StatusCompleted,
		Started:   time.Now(),
	}
}

// fail marks a result failed and returns it, so a workload can end on one line.
func fail(r Result, err error) (Result, error) {
	r.Status = sandbox.StatusFailed
	r.Reason = err.Error()
	r.Elapsed = time.Since(r.Started)
	return r, err
}

// errExit turns a non-zero exit into an error that names the command, because
// a bare "exit 1" in a result file is not debuggable.
func errExit(cmd string, res sandbox.ExecResult) error {
	return fmt.Errorf("%s exited %d: %s", cmd, res.ExitCode, truncate(res.Stderr, 400))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
