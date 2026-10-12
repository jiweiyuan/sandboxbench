package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
	"github.com/jiweiyuan/sandboxbench/pkg/workload"
)

// SchemaVersion is bumped whenever the result file layout changes in a way
// that an older reader would misinterpret.
const SchemaVersion = 1

// Run is the whole output of one invocation.
type Run struct {
	SchemaVersion int        `json:"schema_version"`
	ID            string     `json:"id"`
	Notes         string     `json:"notes,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    time.Time  `json:"finished_at"`
	Client        ClientInfo `json:"client"`
	Config        *Config    `json:"-"`
	// ConfigEcho is the configuration as it was actually applied, so a result
	// file is self-describing and a run can be repeated from it alone.
	ConfigEcho map[string]any `json:"config"`
	Providers  []ProviderRun  `json:"providers"`
}

// ClientInfo records where the load came from. Sandbox benchmarks measure a
// network path as much as a sandbox, so a result without it is not comparable
// to anything.
type ClientInfo struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	NumCPU   int    `json:"num_cpu"`
	Hostname string `json:"hostname,omitempty"`
	GoVer    string `json:"go_version"`
	// Region is free text the operator supplies, such as "aws eu-central-1"
	// or "home broadband, Shanghai".
	Region string `json:"region,omitempty"`
}

// ProviderRun is every benchmark result for one provider.
type ProviderRun struct {
	Provider     string               `json:"provider"`
	Capabilities sandbox.Capabilities `json:"capabilities"`
	Error        string               `json:"error,omitempty"`
	Results      []workload.Result    `json:"results"`
}

// Runner executes a configuration.
type Runner struct {
	Cfg *Config
	// Logf receives progress lines. Leave it nil to discard them.
	Logf func(format string, args ...any)
	// Region is copied into ClientInfo.
	Region string
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Execute runs every benchmark against every provider.
//
// One provider failing to construct, or one benchmark failing to run, never
// aborts the run: the failure is recorded in its place and the rest continues.
// A partial result file with explicit failures is more useful than no file.
func (r *Runner) Execute(ctx context.Context) (*Run, error) {
	if r.Cfg == nil {
		return nil, errors.New("runner: no configuration")
	}
	host, _ := os.Hostname()
	run := &Run{
		SchemaVersion: SchemaVersion,
		ID:            r.Cfg.ID,
		Notes:         r.Cfg.Notes,
		StartedAt:     time.Now().UTC(),
		Client: ClientInfo{
			OS:       runtime.GOOS,
			Arch:     runtime.GOARCH,
			NumCPU:   runtime.NumCPU(),
			Hostname: host,
			GoVer:    runtime.Version(),
			Region:   r.Region,
		},
		Config:     r.Cfg,
		ConfigEcho: r.Cfg.echo(),
	}

	for _, pc := range r.Cfg.Providers {
		pr := ProviderRun{Provider: pc.Name}
		prov, err := sandbox.New(pc.Name, pc.Config)
		if err != nil {
			pr.Error = err.Error()
			r.logf("provider %s: %v", pc.Name, err)
			run.Providers = append(run.Providers, pr)
			continue
		}
		pr.Capabilities = prov.Capabilities()
		r.logf("provider %s: exec transport %s", pc.Name, pr.Capabilities.ExecTransport)

		for _, bc := range r.Cfg.Benchmarks {
			res := r.runOne(ctx, prov, pc, bc)
			pr.Results = append(pr.Results, res)
			r.logf("  %-32s %-12s %s", res.Workload, res.Status, summarize(res))
		}
		if err := prov.Close(); err != nil {
			r.logf("provider %s: close: %v", pc.Name, err)
		}
		run.Providers = append(run.Providers, pr)
	}

	run.FinishedAt = time.Now().UTC()
	return run, nil
}

// runOne runs a single benchmark against a single provider and always returns
// a Result, never an error: an error is the Result.
func (r *Runner) runOne(ctx context.Context, prov sandbox.Provider, pc ProviderConfig, bc BenchConfig) workload.Result {
	w, err := workload.Lookup(bc.Name, sandbox.Cfg(bc.With))
	if err != nil {
		return workload.Result{
			Workload: bc.Name,
			Status:   sandbox.StatusFailed,
			Reason:   err.Error(),
			Started:  time.Now(),
		}
	}

	base := workload.Result{
		Workload:  w.Name(),
		Dimension: w.Dimension(),
		Scale:     w.Scale(),
		Started:   time.Now(),
	}

	// Capability gate. Checking before any sandbox exists is what makes an
	// unsupported benchmark free rather than a wasted create.
	if unmet := w.Requires().Unmet(prov.Capabilities()); unmet != "" {
		base.Status = sandbox.StatusUnsupported
		base.Reason = "provider does not support " + unmet
		return base
	}

	env := r.Cfg.env(bc, pc, r.Logf)
	ctx, cancel := context.WithTimeout(ctx, r.Cfg.Run.Timeout)
	defer cancel()

	var res workload.Result
	switch tw := w.(type) {
	case workload.ProviderWorkload:
		res, err = tw.RunProvider(ctx, env, prov)
	case workload.SandboxWorkload:
		res, err = r.runInOneSandbox(ctx, env, prov, tw)
	default:
		base.Status = sandbox.StatusFailed
		base.Reason = "workload implements neither SandboxWorkload nor ProviderWorkload"
		return base
	}
	if err != nil && res.Status == sandbox.StatusCompleted {
		// A workload that returned an error without marking its own result.
		res.Status = sandbox.StatusFailed
		res.Reason = err.Error()
	}
	if res.Workload == "" {
		res.Workload = w.Name()
		res.Dimension = w.Dimension()
		res.Scale = w.Scale()
	}
	return res
}

// runInOneSandbox creates one sandbox, confirms it can run a command, hands it
// to the workload, and tears it down whatever happens.
func (r *Runner) runInOneSandbox(ctx context.Context, env workload.Env, prov sandbox.Provider, w workload.SandboxWorkload) (workload.Result, error) {
	box, err := prov.Create(ctx, env.Create)
	if err != nil {
		return workload.Result{
			Workload:  w.Name(),
			Dimension: w.Dimension(),
			Scale:     w.Scale(),
			Status:    sandbox.StatusFailed,
			Reason:    fmt.Sprintf("create: %v", err),
			Started:   time.Now(),
		}, err
	}
	defer func() {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := box.Destroy(tctx); err != nil {
			r.logf("  destroy %s/%s: %v", prov.Name(), box.ID(), err)
		}
	}()

	if _, err := box.Exec(ctx, sandbox.Shell("true")); err != nil {
		return workload.Result{
			Workload:  w.Name(),
			Dimension: w.Dimension(),
			Scale:     w.Scale(),
			Status:    sandbox.StatusFailed,
			Reason:    fmt.Sprintf("ready probe: %v", err),
			Started:   time.Now(),
		}, err
	}
	return w.Run(ctx, env, box)
}

// summarize produces the one-line progress text for a finished benchmark.
func summarize(res workload.Result) string {
	if res.Status != sandbox.StatusCompleted {
		return res.Reason
	}
	for _, s := range res.Series {
		return fmt.Sprintf("%s p50=%.1f%s p99=%.1f%s n=%d", s.Name, s.P50, s.Unit, s.P99, s.Unit, s.N)
	}
	for k, v := range res.Scalars {
		return fmt.Sprintf("%s=%.3f", k, v)
	}
	return ""
}

// echo renders the applied configuration for the result file.
func (c *Config) echo() map[string]any {
	providers := make([]map[string]any, 0, len(c.Providers))
	for _, p := range c.Providers {
		// Adapter config blocks hold credentials, so only the keys are echoed.
		keys := make([]string, 0, len(p.Config))
		for k := range p.Config {
			keys = append(keys, k)
		}
		providers = append(providers, map[string]any{"name": p.Name, "config_keys": keys})
	}
	benches := make([]map[string]any, 0, len(c.Benchmarks))
	for _, b := range c.Benchmarks {
		benches = append(benches, map[string]any{"name": b.Name, "with": b.With})
	}
	return map[string]any{
		"id":    c.ID,
		"notes": c.Notes,
		"run": map[string]any{
			"repetitions": c.Run.Repetitions,
			"concurrency": c.Run.Concurrency,
			"rate":        c.Run.Rate,
			"duration":    c.Run.Duration.String(),
			"timeout":     c.Run.Timeout.String(),
		},
		"create": map[string]any{
			"image":      c.Create.Image,
			"vcpus":      c.Create.VCPUs,
			"memory_mib": c.Create.MemoryMB,
			"disk_gb":    c.Create.DiskGB,
			"ttl":        c.Create.TTL.String(),
		},
		"providers":  providers,
		"benchmarks": benches,
	}
}
