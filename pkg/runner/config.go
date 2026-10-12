// Package runner turns a run configuration into results.
//
// It owns three things workloads must not decide for themselves: how many
// sandboxes exist, when a benchmark is skipped as unsupported, and what gets
// recorded about the environment the run happened in.
package runner

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
	"github.com/jiweiyuan/sandboxbench/pkg/workload"
)

// Config is a run configuration file.
type Config struct {
	// ID names the run. It ends up in the result filename.
	ID string `yaml:"id"`
	// Notes records anything a reader of the results needs that the schema
	// does not capture, such as the client's network location.
	Notes string `yaml:"notes"`

	Run        RunParams        `yaml:"run"`
	Create     CreateParams     `yaml:"create"`
	Providers  []ProviderConfig `yaml:"providers"`
	Benchmarks []BenchConfig    `yaml:"benchmarks"`
}

// RunParams are the load parameters shared by every benchmark in the run.
type RunParams struct {
	Repetitions int           `yaml:"repetitions"`
	Concurrency int           `yaml:"concurrency"`
	Rate        float64       `yaml:"rate"`
	Duration    time.Duration `yaml:"duration"`
	// Timeout bounds one benchmark against one provider. A hung provider must
	// not stall the whole run.
	Timeout time.Duration `yaml:"timeout"`
}

// CreateParams is the sandbox shape requested from every provider. Comparing
// providers on different shapes compares nothing, so these values are recorded
// with the results and a reader can check they match.
type CreateParams struct {
	Image    string            `yaml:"image"`
	VCPUs    int               `yaml:"vcpus"`
	MemoryMB int               `yaml:"memory_mib"`
	DiskGB   int               `yaml:"disk_gb"`
	TTL      time.Duration     `yaml:"ttl"`
	Env      map[string]string `yaml:"env"`
}

// ProviderConfig selects and configures one adapter.
type ProviderConfig struct {
	Name   string         `yaml:"name"`
	Config map[string]any `yaml:"config"`
	// Create overrides the run-wide create parameters for this provider only.
	// Use it when two providers express the same shape differently, and say so
	// in Notes.
	Create *CreateParams `yaml:"create"`
}

// BenchConfig selects one benchmark and its parameters.
type BenchConfig struct {
	Name string         `yaml:"name"`
	With map[string]any `yaml:"with"`
	// Repetitions overrides the run-wide value for this benchmark only.
	Repetitions int `yaml:"repetitions"`
	// Concurrency overrides the run-wide value for this benchmark only.
	Concurrency int `yaml:"concurrency"`
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.ID == "" {
		c.ID = "run"
	}
	if c.Run.Repetitions == 0 {
		c.Run.Repetitions = 10
	}
	if c.Run.Concurrency == 0 {
		c.Run.Concurrency = 8
	}
	if c.Run.Duration == 0 {
		c.Run.Duration = 60 * time.Second
	}
	if c.Run.Timeout == 0 {
		c.Run.Timeout = 30 * time.Minute
	}
}

// Validate rejects a configuration that would produce results nobody can
// interpret.
func (c *Config) Validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("no providers configured")
	}
	if len(c.Benchmarks) == 0 {
		return fmt.Errorf("no benchmarks configured")
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("a provider entry has no name")
		}
		if seen[p.Name] {
			return fmt.Errorf("provider %q listed twice", p.Name)
		}
		seen[p.Name] = true
	}
	for _, b := range c.Benchmarks {
		if b.Name == "" {
			return fmt.Errorf("a benchmark entry has no name")
		}
		if _, err := workload.Lookup(b.Name, sandbox.Cfg(b.With)); err != nil {
			return err
		}
	}
	return nil
}

// createOptions merges the run-wide create parameters with a provider's
// override.
func (c *Config) createOptions(p ProviderConfig) sandbox.CreateOptions {
	cp := c.Create
	if p.Create != nil {
		cp = *p.Create
	}
	return sandbox.CreateOptions{
		Image:    cp.Image,
		VCPUs:    cp.VCPUs,
		MemoryMB: cp.MemoryMB,
		DiskGB:   cp.DiskGB,
		TTL:      cp.TTL,
		Env:      cp.Env,
		Labels:   map[string]string{"sandboxbench.run": c.ID},
	}
}

// env builds the workload environment for one benchmark entry.
func (c *Config) env(b BenchConfig, p ProviderConfig, logf func(string, ...any)) workload.Env {
	reps := c.Run.Repetitions
	if b.Repetitions > 0 {
		reps = b.Repetitions
	}
	conc := c.Run.Concurrency
	if b.Concurrency > 0 {
		conc = b.Concurrency
	}
	return workload.Env{
		Repetitions: reps,
		Concurrency: conc,
		Rate:        c.Run.Rate,
		Duration:    c.Run.Duration,
		Create:      c.createOptions(p),
		Logf:        logf,
	}
}
