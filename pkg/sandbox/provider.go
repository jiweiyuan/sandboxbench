// Package sandbox defines the provider-facing contract of SandboxBench.
//
// The contract is deliberately small, in the spirit of a Terraform provider:
// an adapter owns resource lifecycle and command execution, and knows nothing
// about what is being measured. Workloads decide what to measure, the runner
// decides how to drive it, and pkg/metrics decides how to report it.
package sandbox

import (
	"context"
	"errors"
	"time"
)

// ErrUnsupported is returned by an adapter for a capability the provider does
// not offer. A workload that receives it must report StatusUnsupported, never
// a zero score. See docs/capabilities.md.
var ErrUnsupported = errors.New("sandboxbench: capability not supported by provider")

// ExecTransport names the channel an adapter uses to run a command inside the
// sandbox. The three hosted providers in v0.1 each use a different one, so the
// transport is part of the reported configuration, not an implementation
// detail: it sets the floor on per-command latency.
type ExecTransport string

const (
	TransportLocal      ExecTransport = "local"       // os/exec on the runner host
	TransportREST       ExecTransport = "rest"        // one HTTP request per command
	TransportConnectRPC ExecTransport = "connect-rpc" // streaming RPC to an in-sandbox daemon
	TransportSSH        ExecTransport = "ssh"         // multiplexed SSH session
)

// Capabilities is what an adapter claims it can do. Adapters must report what
// the configured account and image actually support, not what the vendor's
// marketing page lists.
type Capabilities struct {
	Suspend              bool          `json:"suspend"`
	Resume               bool          `json:"resume"`
	Snapshot             bool          `json:"snapshot"`
	PersistentFilesystem bool          `json:"persistent_filesystem"`
	CustomImage          bool          `json:"custom_image"`
	PortForwarding       bool          `json:"port_forwarding"`
	ExecTransport        ExecTransport `json:"exec_transport"`
}

// CreateOptions is the common subset of sandbox creation parameters. Anything
// provider-specific goes in Extra, and the runner records Extra verbatim in
// the result file so a run can be reproduced.
type CreateOptions struct {
	Image    string            `json:"image,omitempty"`
	VCPUs    int               `json:"vcpus,omitempty"`
	MemoryMB int               `json:"memory_mib,omitempty"`
	DiskGB   int               `json:"disk_gb,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	TTL      time.Duration     `json:"ttl,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Extra    map[string]any    `json:"extra,omitempty"`
}

// Command is a single command to run inside the sandbox.
type Command struct {
	Cmd     string            `json:"cmd"`
	Args    []string          `json:"args,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout time.Duration     `json:"timeout,omitempty"`
}

// Shell wraps a shell line so a workload does not have to split arguments.
func Shell(line string) Command {
	return Command{Cmd: "/bin/sh", Args: []string{"-lc", line}}
}

// ExecResult carries both timings because they answer different questions.
//
//	Wall     — measured by the runner, so it includes control-plane RTT.
//	InBox    — measured inside the sandbox, so it excludes that RTT.
//
// Reporting only Wall conflates the provider's API latency with the sandbox's
// compute speed. Every workload that measures compute must use InBox.
type ExecResult struct {
	ExitCode int           `json:"exit_code"`
	Stdout   string        `json:"stdout,omitempty"`
	Stderr   string        `json:"stderr,omitempty"`
	Wall     time.Duration `json:"wall_ns"`
	InBox    time.Duration `json:"in_box_ns,omitempty"`
}

// OK reports whether the command exited 0.
func (r ExecResult) OK() bool { return r.ExitCode == 0 }

// Provider creates sandboxes. One Provider value serves a whole run, so
// implementations must be safe for concurrent use by many goroutines.
type Provider interface {
	Name() string
	Capabilities() Capabilities

	// Create returns a sandbox that is ready to accept Exec. An adapter that
	// must poll for readiness does so here, so that the create latency a
	// workload measures is always create-to-first-exec-ready.
	Create(ctx context.Context, opts CreateOptions) (Sandbox, error)

	Close() error
}

// Sandbox is one live execution environment.
type Sandbox interface {
	ID() string
	Provider() string

	Exec(ctx context.Context, cmd Command) (ExecResult, error)

	WriteFile(ctx context.Context, path string, data []byte) error
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// Suspend and Resume return ErrUnsupported when the provider has no such
	// operation.
	Suspend(ctx context.Context) error
	Resume(ctx context.Context) error

	Destroy(ctx context.Context) error
}
