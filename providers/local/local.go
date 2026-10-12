// Package local is the reference adapter. It runs workloads on the machine the
// runner is on, either as plain processes in a scratch directory or inside a
// Docker container.
//
// Its purpose is to make the pipeline runnable with no credentials, so a
// workload can be debugged and the statistics checked before any money is
// spent. Numbers from the `process` mode are not comparable with a hosted
// provider: there is no isolation boundary and no control plane, so it is a
// floor on latency rather than a competitor.
package local

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
)

func init() {
	sandbox.Register("local", New)
}

// Mode selects how a local sandbox is realised.
type Mode string

const (
	// ModeProcess runs commands directly on the host in a scratch directory.
	ModeProcess Mode = "process"
	// ModeDocker runs each sandbox as a container. `docker pause` and
	// `docker unpause` give it real suspend and resume.
	ModeDocker Mode = "docker"
)

type provider struct {
	mode    Mode
	image   string
	docker  string
	seq     atomic.Int64
	workDir string
}

// New builds the local adapter.
//
// Config keys:
//
//	mode:   "process" (default) or "docker"
//	image:  container image for docker mode (default "debian:stable-slim")
//	docker: docker binary name (default "docker")
func New(raw map[string]any) (sandbox.Provider, error) {
	cfg := sandbox.Cfg(raw)
	mode := Mode(cfg.String("mode", string(ModeProcess)))
	switch mode {
	case ModeProcess, ModeDocker:
	default:
		return nil, fmt.Errorf("local: unknown mode %q", mode)
	}
	p := &provider{
		mode:    mode,
		image:   cfg.String("image", "debian:stable-slim"),
		docker:  cfg.String("docker", "docker"),
		workDir: cfg.String("work_dir", filepath.Join(os.TempDir(), "sandboxbench-local")),
	}
	if mode == ModeDocker {
		if _, err := exec.LookPath(p.docker); err != nil {
			return nil, fmt.Errorf("local: docker mode needs %q on PATH: %w", p.docker, err)
		}
	}
	if err := os.MkdirAll(p.workDir, 0o700); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *provider) Name() string { return "local" }

func (p *provider) Capabilities() sandbox.Capabilities {
	c := sandbox.Capabilities{
		PersistentFilesystem: true,
		CustomImage:          p.mode == ModeDocker,
		ExecTransport:        sandbox.TransportLocal,
	}
	if p.mode == ModeDocker {
		// docker pause freezes the container's processes and docker unpause
		// thaws them. That is a suspend and a resume in the sense the
		// lifecycle benchmarks mean, so it is reported as one.
		c.Suspend = true
		c.Resume = true
	}
	return c
}

func (p *provider) Close() error { return nil }

func (p *provider) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	id := fmt.Sprintf("local-%d-%d", time.Now().UnixNano(), p.seq.Add(1))
	switch p.mode {
	case ModeProcess:
		dir := filepath.Join(p.workDir, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		return &box{p: p, id: id, dir: dir, env: opts.Env}, nil

	case ModeDocker:
		image := opts.Image
		if image == "" {
			image = p.image
		}
		args := []string{"run", "-d", "--name", id, "--label", "sandboxbench=1"}
		if opts.VCPUs > 0 {
			args = append(args, "--cpus", fmt.Sprintf("%d", opts.VCPUs))
		}
		if opts.MemoryMB > 0 {
			args = append(args, "--memory", fmt.Sprintf("%dm", opts.MemoryMB))
		}
		for k, v := range opts.Env {
			args = append(args, "-e", k+"="+v)
		}
		args = append(args, image, "sleep", "infinity")
		if out, err := p.run(ctx, args...); err != nil {
			return nil, fmt.Errorf("local: docker run: %w: %s", err, out)
		}
		return &box{p: p, id: id, env: opts.Env}, nil
	}
	return nil, fmt.Errorf("local: unreachable mode %q", p.mode)
}

// run invokes the docker binary and returns its combined output.
func (p *provider) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, p.docker, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return strings.TrimSpace(buf.String()), err
}

type box struct {
	p   *provider
	id  string
	dir string
	env map[string]string
}

func (b *box) ID() string       { return b.id }
func (b *box) Provider() string { return "local" }

func (b *box) Exec(ctx context.Context, c sandbox.Command) (sandbox.ExecResult, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	var cmd *exec.Cmd
	switch b.p.mode {
	case ModeProcess:
		cmd = exec.CommandContext(ctx, c.Cmd, c.Args...)
		cmd.Dir = b.dir
		if c.Cwd != "" {
			cmd.Dir = c.Cwd
		}
		cmd.Env = append(os.Environ(), flatten(b.env)...)
		cmd.Env = append(cmd.Env, flatten(c.Env)...)
	case ModeDocker:
		args := []string{"exec"}
		if c.Cwd != "" {
			args = append(args, "-w", c.Cwd)
		}
		for k, v := range c.Env {
			args = append(args, "-e", k+"="+v)
		}
		args = append(args, b.id, c.Cmd)
		args = append(args, c.Args...)
		cmd = exec.CommandContext(ctx, b.p.docker, args...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	t0 := time.Now()
	err := cmd.Run()
	res := sandbox.ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
		Wall:   time.Since(t0),
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		return res, err
	}
	return res, nil
}

func (b *box) WriteFile(ctx context.Context, path string, data []byte) error {
	switch b.p.mode {
	case ModeProcess:
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, data, 0o644)
	case ModeDocker:
		// `docker cp` from stdin needs a tar stream, so the file goes in
		// through the shell instead. It is base64-encoded so that arbitrary
		// bytes survive the round trip.
		enc := base64.StdEncoding.EncodeToString(data)
		line := fmt.Sprintf("mkdir -p \"$(dirname %s)\" && printf '%%s' %s | base64 -d > %s",
			shQuote(path), shQuote(enc), shQuote(path))
		res, err := b.Exec(ctx, sandbox.Shell(line))
		if err != nil {
			return err
		}
		if !res.OK() {
			return fmt.Errorf("local: write %s: exit %d: %s", path, res.ExitCode, res.Stderr)
		}
		return nil
	}
	return fmt.Errorf("local: unreachable mode")
}

func (b *box) ReadFile(ctx context.Context, path string) ([]byte, error) {
	switch b.p.mode {
	case ModeProcess:
		return os.ReadFile(path)
	case ModeDocker:
		res, err := b.Exec(ctx, sandbox.Shell("cat "+shQuote(path)))
		if err != nil {
			return nil, err
		}
		if !res.OK() {
			return nil, fmt.Errorf("local: read %s: exit %d: %s", path, res.ExitCode, res.Stderr)
		}
		return []byte(res.Stdout), nil
	}
	return nil, fmt.Errorf("local: unreachable mode")
}

func (b *box) Suspend(ctx context.Context) error {
	if b.p.mode != ModeDocker {
		return sandbox.ErrUnsupported
	}
	out, err := b.p.run(ctx, "pause", b.id)
	if err != nil {
		return fmt.Errorf("local: docker pause: %w: %s", err, out)
	}
	return nil
}

func (b *box) Resume(ctx context.Context) error {
	if b.p.mode != ModeDocker {
		return sandbox.ErrUnsupported
	}
	out, err := b.p.run(ctx, "unpause", b.id)
	if err != nil {
		return fmt.Errorf("local: docker unpause: %w: %s", err, out)
	}
	return nil
}

func (b *box) Destroy(ctx context.Context) error {
	switch b.p.mode {
	case ModeProcess:
		return os.RemoveAll(b.dir)
	case ModeDocker:
		out, err := b.p.run(ctx, "rm", "-f", b.id)
		if err != nil {
			return fmt.Errorf("local: docker rm: %w: %s", err, out)
		}
		return nil
	}
	return nil
}

func flatten(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
