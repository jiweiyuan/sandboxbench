// Package daytona adapts Daytona's sandbox API.
//
// Daytona runs command execution through a per-sandbox toolbox daemon reached
// via a proxy URL the control plane hands back with the sandbox, so one
// command is one HTTP request. That places it in the `rest` exec transport
// class, which sets the floor on its per-command latency.
//
// Verification status: lifecycle calls here follow api.daytona.io's published
// OpenAPI document. The toolbox exec and file paths are not in that document,
// so they are written from Daytona's SDK shape and are configurable. Nothing
// in this file has been run against the live API.
package daytona

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
	"github.com/jiweiyuan/sandboxbench/providers/internal/httpx"
)

func init() {
	sandbox.Register("daytona", New)
}

const defaultAPI = "https://api.daytona.io"

type provider struct {
	api *httpx.Client
	// toolboxPath is the path template appended to a sandbox's toolbox proxy
	// URL. %s is the sandbox id.
	toolboxPath  string
	snapshot     string
	target       string
	readyTimeout time.Duration
	pollInterval time.Duration
}

// New builds the Daytona adapter.
//
// Config keys:
//
//	api_key:       bearer token, usually "${DAYTONA_API_KEY}"
//	base_url:      API base (default https://api.daytona.io)
//	snapshot:      snapshot id or name to create from
//	target:        region target, e.g. "us"
//	toolbox_path:  path template under the toolbox proxy (default "/%s/toolbox")
//	ready_timeout: how long to wait for the started state (default 5m)
func New(raw map[string]any) (sandbox.Provider, error) {
	cfg := sandbox.Cfg(raw)
	key, err := cfg.Require("api_key")
	if err != nil {
		return nil, fmt.Errorf("daytona: %w", err)
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+key)
	return &provider{
		api:          httpx.New(cfg.String("base_url", defaultAPI), h, cfg.Duration("http_timeout", 0)),
		toolboxPath:  cfg.String("toolbox_path", "/%s/toolbox"),
		snapshot:     cfg.String("snapshot", ""),
		target:       cfg.String("target", ""),
		readyTimeout: cfg.Duration("ready_timeout", 5*time.Minute),
		pollInterval: cfg.Duration("poll_interval", 250*time.Millisecond),
	}, nil
}

func (p *provider) Name() string { return "daytona" }

func (p *provider) Capabilities() sandbox.Capabilities {
	// Pause and start exist as API operations, so suspend and resume are
	// claimed. Whether a pause preserves memory or only the filesystem is a
	// separate question that lifecycle.resume answers by checking state, and
	// app.stateful-continue answers by checking a checksum.
	return sandbox.Capabilities{
		Suspend:              true,
		Resume:               true,
		Snapshot:             true,
		PersistentFilesystem: true,
		CustomImage:          true,
		PortForwarding:       true,
		ExecTransport:        sandbox.TransportREST,
	}
}

func (p *provider) Close() error { return nil }

type apiSandbox struct {
	ID              string `json:"id"`
	State           string `json:"state"`
	ToolboxProxyURL string `json:"toolboxProxyUrl"`
	ErrorReason     string `json:"errorReason"`
}

type createBody struct {
	Name     string            `json:"name,omitempty"`
	Snapshot string            `json:"snapshot,omitempty"`
	Target   string            `json:"target,omitempty"`
	CPU      int               `json:"cpu,omitempty"`
	Memory   int               `json:"memory,omitempty"`
	Disk     int               `json:"disk,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	// AutoDeleteInterval is minutes. A benchmark that crashes must not leave
	// sandboxes billing, so a TTL is always sent.
	AutoDeleteInterval int `json:"autoDeleteInterval,omitempty"`
}

func (p *provider) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	body := createBody{
		Snapshot: firstNonEmpty(opts.Image, p.snapshot),
		Target:   p.target,
		CPU:      opts.VCPUs,
		Env:      opts.Env,
		Labels:   opts.Labels,
	}
	if opts.MemoryMB > 0 {
		// Daytona expresses memory in GiB.
		body.Memory = maxInt(1, opts.MemoryMB/1024)
	}
	if opts.DiskGB > 0 {
		body.Disk = opts.DiskGB
	}
	if opts.TTL > 0 {
		body.AutoDeleteInterval = maxInt(1, int(opts.TTL.Minutes()))
	}

	var created apiSandbox
	if err := p.api.Do(ctx, http.MethodPost, "/sandbox", body, &created, nil); err != nil {
		return nil, fmt.Errorf("daytona: create: %w", err)
	}
	b := &box{p: p, id: created.ID, toolbox: created.ToolboxProxyURL}

	if err := p.waitStarted(ctx, b); err != nil {
		destroyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = b.Destroy(destroyCtx)
		return nil, err
	}
	return b, nil
}

// waitStarted polls until the sandbox reports started, so that the latency the
// lifecycle benchmark records includes everything up to a usable sandbox.
func (p *provider) waitStarted(ctx context.Context, b *box) error {
	return httpx.PollUntil(ctx, p.pollInterval, p.readyTimeout, func(ctx context.Context) (bool, error) {
		var s apiSandbox
		if err := p.api.Do(ctx, http.MethodGet, "/sandbox/"+url.PathEscape(b.id), nil, &s, nil); err != nil {
			return false, fmt.Errorf("daytona: get sandbox: %w", err)
		}
		if s.ToolboxProxyURL != "" {
			b.toolbox = s.ToolboxProxyURL
		}
		switch s.State {
		case "started":
			return true, nil
		case "error", "build_failed", "destroyed":
			return false, fmt.Errorf("daytona: sandbox %s entered state %q: %s", b.id, s.State, s.ErrorReason)
		default:
			return false, nil
		}
	})
}

type box struct {
	p       *provider
	id      string
	toolbox string
}

func (b *box) ID() string       { return b.id }
func (b *box) Provider() string { return "daytona" }

// toolboxURL builds an absolute URL under the sandbox's toolbox proxy.
func (b *box) toolboxURL(suffix string) (string, error) {
	if b.toolbox == "" {
		return "", fmt.Errorf("daytona: sandbox %s has no toolbox proxy URL", b.id)
	}
	base := strings.TrimRight(b.toolbox, "/") + fmt.Sprintf(b.p.toolboxPath, url.PathEscape(b.id))
	return base + suffix, nil
}

type execRequest struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
	Timeout int    `json:"timeout,omitempty"`
}

type execResponse struct {
	ExitCode int    `json:"exitCode"`
	Result   string `json:"result"`
}

func (b *box) Exec(ctx context.Context, c sandbox.Command) (sandbox.ExecResult, error) {
	u, err := b.toolboxURL("/process/execute")
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	req := execRequest{Command: joinCommand(c), Cwd: c.Cwd}
	if c.Timeout > 0 {
		req.Timeout = int(c.Timeout.Seconds())
	}

	var out execResponse
	t0 := time.Now()
	err = b.p.api.Do(ctx, http.MethodPost, u, req, &out, nil)
	wall := time.Since(t0)
	if err != nil {
		return sandbox.ExecResult{Wall: wall}, fmt.Errorf("daytona: exec: %w", err)
	}
	// The toolbox returns one combined output stream. It is reported as
	// stdout, and stderr is left empty rather than guessed at.
	return sandbox.ExecResult{ExitCode: out.ExitCode, Stdout: out.Result, Wall: wall}, nil
}

func (b *box) WriteFile(ctx context.Context, path string, data []byte) error {
	// Writing through the shell keeps this adapter to one toolbox endpoint.
	// A multipart upload would be faster for large files; no benchmark in
	// v0.1 writes one.
	enc := base64.StdEncoding.EncodeToString(data)
	line := fmt.Sprintf("mkdir -p \"$(dirname %s)\" && printf '%%s' %s | base64 -d > %s",
		shQuote(path), shQuote(enc), shQuote(path))
	res, err := b.Exec(ctx, sandbox.Shell(line))
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("daytona: write %s: exit %d: %s", path, res.ExitCode, res.Stdout)
	}
	return nil
}

func (b *box) ReadFile(ctx context.Context, path string) ([]byte, error) {
	res, err := b.Exec(ctx, sandbox.Shell("cat "+shQuote(path)))
	if err != nil {
		return nil, err
	}
	if !res.OK() {
		return nil, fmt.Errorf("daytona: read %s: exit %d: %s", path, res.ExitCode, res.Stdout)
	}
	return []byte(res.Stdout), nil
}

func (b *box) Suspend(ctx context.Context) error {
	if err := b.p.api.Do(ctx, http.MethodPost, "/sandbox/"+url.PathEscape(b.id)+"/pause", nil, nil, nil); err != nil {
		return fmt.Errorf("daytona: pause: %w", err)
	}
	return httpx.PollUntil(ctx, b.p.pollInterval, b.p.readyTimeout, func(ctx context.Context) (bool, error) {
		var s apiSandbox
		if err := b.p.api.Do(ctx, http.MethodGet, "/sandbox/"+url.PathEscape(b.id), nil, &s, nil); err != nil {
			return false, err
		}
		return s.State == "paused" || s.State == "stopped", nil
	})
}

func (b *box) Resume(ctx context.Context) error {
	if err := b.p.api.Do(ctx, http.MethodPost, "/sandbox/"+url.PathEscape(b.id)+"/start", nil, nil, nil); err != nil {
		return fmt.Errorf("daytona: start: %w", err)
	}
	return b.p.waitStarted(ctx, b)
}

func (b *box) Destroy(ctx context.Context) error {
	if err := b.p.api.Do(ctx, http.MethodDelete, "/sandbox/"+url.PathEscape(b.id), nil, nil, nil); err != nil {
		return fmt.Errorf("daytona: delete: %w", err)
	}
	return nil
}

func joinCommand(c sandbox.Command) string {
	parts := make([]string, 0, len(c.Args)+1)
	parts = append(parts, c.Cmd)
	for _, a := range c.Args {
		parts = append(parts, shQuote(a))
	}
	return strings.Join(parts, " ")
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
