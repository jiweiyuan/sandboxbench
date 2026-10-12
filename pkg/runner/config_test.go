package runner

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	_ "github.com/jiweiyuan/sandboxbench/providers/local"
)

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load("testdata/good.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ID != "unit" {
		t.Errorf("ID = %q", cfg.ID)
	}
	if cfg.Run.Repetitions != 3 || cfg.Run.Concurrency != 2 {
		t.Errorf("run params = %+v", cfg.Run)
	}
	if cfg.Run.Timeout != 30*time.Minute {
		t.Errorf("default timeout = %v, want 30m", cfg.Run.Timeout)
	}
}

func TestLoadRejectsUnknownBenchmark(t *testing.T) {
	_, err := Load("testdata/unknown-benchmark.yaml")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "unknown benchmark") {
		t.Errorf("error does not name the problem: %v", err)
	}
}

func TestLoadRejectsDuplicateProvider(t *testing.T) {
	_, err := Load("testdata/duplicate-provider.yaml")
	if err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("expected a duplicate-provider error, got %v", err)
	}
}

func TestPerBenchmarkOverrides(t *testing.T) {
	cfg, err := Load("testdata/good.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Entry 0 inherits the run-wide repetition count; entry 1 overrides it.
	if got := cfg.env(cfg.Benchmarks[0], cfg.Providers[0], nil).Repetitions; got != 3 {
		t.Errorf("inherited repetitions = %d, want 3", got)
	}
	if got := cfg.env(cfg.Benchmarks[1], cfg.Providers[0], nil).Repetitions; got != 1 {
		t.Errorf("overridden repetitions = %d, want 1", got)
	}
}

func TestEchoHidesCredentialValues(t *testing.T) {
	cfg := &Config{
		ID: "x",
		Providers: []ProviderConfig{{
			Name:   "e2b",
			Config: map[string]any{"api_key": "sk-secret-value"},
		}},
	}
	echo := cfg.echo()
	rendered := fmtAny(echo)
	if strings.Contains(rendered, "sk-secret-value") {
		t.Fatalf("echoed configuration leaked a credential: %s", rendered)
	}
	if !strings.Contains(rendered, "api_key") {
		t.Errorf("echoed configuration dropped the key name: %s", rendered)
	}
}

func TestCreateOptionsCarriesRunLabel(t *testing.T) {
	cfg, err := Load("testdata/good.yaml")
	if err != nil {
		t.Fatal(err)
	}
	opts := cfg.createOptions(cfg.Providers[0])
	if opts.Labels["sandboxbench.run"] != "unit" {
		t.Errorf("labels = %v", opts.Labels)
	}
	if opts.VCPUs != 1 || opts.MemoryMB != 512 {
		t.Errorf("shape = %+v", opts)
	}
}

// fmtAny renders a value the way the result file would, so the credential
// check looks at the bytes that are actually written to disk.
func fmtAny(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(b)
}
