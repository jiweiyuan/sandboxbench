package runner

import (
	"context"
	"testing"

	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
	"github.com/jiweiyuan/sandboxbench/pkg/workload"
)

// TestExecuteAgainstLocalProvider drives the whole pipeline — config,
// capability gate, workload, teardown, result assembly — against the local
// adapter, which needs no credentials. A workload that stops producing
// samples fails here rather than on a paid provider.
func TestExecuteAgainstLocalProvider(t *testing.T) {
	cfg, err := Load("testdata/integration.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Cfg: cfg}
	run, err := r.Execute(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Providers) != 1 {
		t.Fatalf("got %d provider runs, want 1", len(run.Providers))
	}
	pr := run.Providers[0]
	if pr.Error != "" {
		t.Fatalf("provider error: %s", pr.Error)
	}
	if pr.Capabilities.ExecTransport != sandbox.TransportLocal {
		t.Errorf("exec transport = %q", pr.Capabilities.ExecTransport)
	}

	byName := map[string]workload.Result{}
	for _, res := range pr.Results {
		byName[res.Workload] = res
	}
	if len(byName) != 4 {
		t.Fatalf("got %d results, want 4: %v", len(byName), byName)
	}

	for _, name := range []string{"lifecycle.cold-create", "execution.short-command", "rl.env-step"} {
		res, ok := byName[name]
		if !ok {
			t.Fatalf("no result for %s", name)
		}
		if res.Status != sandbox.StatusCompleted {
			t.Fatalf("%s: status %s: %s", name, res.Status, res.Reason)
		}
		if len(res.Series) == 0 {
			t.Fatalf("%s: completed with no series", name)
		}
		if res.Series[0].N == 0 {
			t.Fatalf("%s: series %q has no samples", name, res.Series[0].Name)
		}
		if res.Raw[res.Series[0].Name] == nil {
			t.Fatalf("%s: raw samples were not archived", name)
		}
	}

	// Process mode has no suspend, so the resume benchmark must report
	// unsupported and must name the missing capability.
	resume := byName["lifecycle.resume"]
	if resume.Status != sandbox.StatusUnsupported {
		t.Fatalf("lifecycle.resume: status %s, want unsupported", resume.Status)
	}
	if resume.Reason == "" {
		t.Error("lifecycle.resume: unsupported without a reason")
	}
	if len(resume.Series) != 0 {
		t.Error("lifecycle.resume: unsupported result carries series data")
	}
}

func TestExecuteRecordsProviderConstructionFailure(t *testing.T) {
	cfg := &Config{
		ID:         "bad",
		Providers:  []ProviderConfig{{Name: "no-such-provider"}},
		Benchmarks: []BenchConfig{{Name: "execution.short-command"}},
	}
	cfg.applyDefaults()
	run, err := (&Runner{Cfg: cfg}).Execute(context.Background())
	if err != nil {
		t.Fatalf("a bad provider must not abort the run: %v", err)
	}
	if run.Providers[0].Error == "" {
		t.Error("provider construction failure was not recorded")
	}
	if len(run.Providers[0].Results) != 0 {
		t.Error("benchmarks ran against a provider that failed to construct")
	}
}
