// Package runta adapts Runta's runtime API.
//
// Runta is the third exec transport class in v0.1, and the most interesting
// one for the paper. Its public REST API covers lifecycle, files and
// checkpoints but has no execute endpoint: commands go in over an SSH gateway.
// One TCP connection then carries many commands as multiplexed sessions, so
// per-command overhead is a session open rather than an HTTP request. Any
// cross-provider comparison of command latency has to report that difference
// rather than average over it.
//
// Verification status: lifecycle, file and checkpoint calls here follow
// runta.com/openapi.yaml. The SSH gateway's hostname, port and username
// convention are not in that document, so they are configuration with no
// default, and `sandboxbench run` fails fast when they are missing rather than
// guessing. Nothing in this file has been run against the live API.
package runta

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
	"github.com/jiweiyuan/sandboxbench/providers/internal/httpx"
)

func init() {
	sandbox.Register("runta", New)
}

const defaultAPI = "https://api.runta.com"

type provider struct {
	api *httpx.Client

	imageID  string
	sshKeyID string

	sshHost      string
	sshPort      int
	sshUserTpl   string
	signer       ssh.Signer
	readyTimeout time.Duration
	pollInterval time.Duration

	hostKeyOnce sync.Once
	hostKeyFn   ssh.HostKeyCallback
	hostKeyErr  error
}

// New builds the Runta adapter.
//
// Config keys:
//
//	api_key:          bearer token, usually "${RUNTA_API_KEY}"
//	base_url:         API base (default https://api.runta.com)
//	image_id:         runtime image variant; omit for the account default
//	ssh_key_id:       UUID of a personal SSH key registered with Runta
//	ssh_private_key:  path to the matching private key
//	ssh_host:         SSH gateway hostname (required for exec)
//	ssh_port:         SSH gateway port (default 22)
//	ssh_user:         username template; %s is the runtime id
func New(raw map[string]any) (sandbox.Provider, error) {
	cfg := sandbox.Cfg(raw)
	key, err := cfg.Require("api_key")
	if err != nil {
		return nil, fmt.Errorf("runta: %w", err)
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+key)

	p := &provider{
		api:          httpx.New(cfg.String("base_url", defaultAPI), h, cfg.Duration("http_timeout", 0)),
		imageID:      cfg.String("image_id", ""),
		sshKeyID:     cfg.String("ssh_key_id", ""),
		sshHost:      cfg.String("ssh_host", ""),
		sshPort:      cfg.Int("ssh_port", 22),
		sshUserTpl:   cfg.String("ssh_user", "%s"),
		readyTimeout: cfg.Duration("ready_timeout", 5*time.Minute),
		pollInterval: cfg.Duration("poll_interval", 250*time.Millisecond),
	}

	if path := cfg.String("ssh_private_key", ""); path != "" {
		pem, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("runta: read ssh_private_key: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			return nil, fmt.Errorf("runta: parse ssh_private_key: %w", err)
		}
		p.signer = signer
	}
	return p, nil
}

func (p *provider) Name() string { return "runta" }

func (p *provider) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		Suspend:              true, // POST /v2/runtimes/{id}/pause
		Resume:               true, // POST /v2/runtimes/{id}/resume
		Snapshot:             true, // POST /v2/runtimes/{id}/checkpoints
		PersistentFilesystem: true,
		CustomImage:          true, // /v2/images and /v2/images/builds
		PortForwarding:       true, // ingress_specs on create
		ExecTransport:        sandbox.TransportSSH,
	}
}

func (p *provider) Close() error { return nil }

// --- control plane ----------------------------------------------------------

type runtime struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	SSHEnabled  bool   `json:"ssh_enabled"`
	Degraded    bool   `json:"degraded"`
	ErrorCode   string `json:"error_code"`
}

type runtimeEnvelope struct {
	Data runtime `json:"data"`
}

type createResources struct {
	Requests struct {
		VCPUs     int `json:"vcpus,omitempty"`
		MemoryMiB int `json:"memory_mib,omitempty"`
	} `json:"requests"`
	Limits struct {
		MemoryMiB int `json:"memory_mib,omitempty"`
	} `json:"limits"`
}

type createRuntime struct {
	Name                 string            `json:"name,omitempty"`
	EnvironmentVariables map[string]string `json:"environment_variables,omitempty"`
	Resources            *createResources  `json:"resources,omitempty"`
	Image                map[string]any    `json:"image,omitempty"`
	SSHKeyIDs            []string          `json:"ssh_key_ids,omitempty"`
}

func (p *provider) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	body := createRuntime{
		EnvironmentVariables: opts.Env,
	}
	if opts.VCPUs > 0 || opts.MemoryMB > 0 {
		res := &createResources{}
		res.Requests.VCPUs = opts.VCPUs
		res.Requests.MemoryMiB = opts.MemoryMB
		res.Limits.MemoryMiB = opts.MemoryMB
		body.Resources = res
	}
	if id := firstNonEmpty(opts.Image, p.imageID); id != "" {
		body.Image = map[string]any{"image_id": id}
	}
	if p.sshKeyID != "" {
		body.SSHKeyIDs = []string{p.sshKeyID}
	}

	var env runtimeEnvelope
	if err := p.api.Do(ctx, http.MethodPost, "/v2/runtimes", body, &env, nil); err != nil {
		return nil, fmt.Errorf("runta: create: %w", err)
	}
	b := &box{p: p, id: env.Data.ID}

	if err := p.waitRunning(ctx, b.id); err != nil {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = b.Destroy(tctx)
		return nil, err
	}
	// Opening the SSH connection here, not on first Exec, is what makes create
	// latency mean create-to-first-exec rather than create-to-API-accepted.
	if err := b.dial(ctx); err != nil {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = b.Destroy(tctx)
		return nil, err
	}
	return b, nil
}

func (p *provider) get(ctx context.Context, id string) (runtime, error) {
	var env runtimeEnvelope
	err := p.api.Do(ctx, http.MethodGet, "/v2/runtimes/"+url.PathEscape(id), nil, &env, nil)
	return env.Data, err
}

func (p *provider) waitRunning(ctx context.Context, id string) error {
	return httpx.PollUntil(ctx, p.pollInterval, p.readyTimeout, func(ctx context.Context) (bool, error) {
		r, err := p.get(ctx, id)
		if err != nil {
			return false, fmt.Errorf("runta: get runtime: %w", err)
		}
		switch r.Status {
		case "running":
			return true, nil
		case "error", "crashed", "shutdown", "unavailable":
			return false, fmt.Errorf("runta: runtime %s entered status %q (%s)", id, r.Status, r.ErrorCode)
		default:
			return false, nil
		}
	})
}

// hostKeyCallback fetches the gateway's host key once and verifies against it.
// A benchmark that disabled host key checking would be measuring an
// unauthenticated channel, which is not the product anyone runs.
func (p *provider) hostKeyCallback(ctx context.Context) (ssh.HostKeyCallback, error) {
	p.hostKeyOnce.Do(func() {
		var env struct {
			Data struct {
				KnownHosts string `json:"known_hosts"`
			} `json:"data"`
		}
		if err := p.api.Do(ctx, http.MethodGet, "/v2/ssh-host-key", nil, &env, nil); err != nil {
			p.hostKeyErr = fmt.Errorf("runta: get ssh host key: %w", err)
			return
		}
		if strings.TrimSpace(env.Data.KnownHosts) == "" {
			p.hostKeyErr = fmt.Errorf("runta: ssh host key response carried no known_hosts entry")
			return
		}
		f, err := os.CreateTemp("", "sandboxbench-runta-known-hosts-*")
		if err != nil {
			p.hostKeyErr = err
			return
		}
		defer f.Close()
		if _, err := io.WriteString(f, env.Data.KnownHosts+"\n"); err != nil {
			p.hostKeyErr = err
			return
		}
		cb, err := knownhosts.New(f.Name())
		if err != nil {
			p.hostKeyErr = fmt.Errorf("runta: parse known_hosts: %w", err)
			return
		}
		p.hostKeyFn = cb
	})
	return p.hostKeyFn, p.hostKeyErr
}

// --- sandbox ----------------------------------------------------------------

type box struct {
	p  *provider
	id string

	mu     sync.Mutex
	client *ssh.Client
}

func (b *box) ID() string       { return b.id }
func (b *box) Provider() string { return "runta" }

// dial opens the multiplexed SSH connection every later command rides on.
func (b *box) dial(ctx context.Context) error {
	if b.p.sshHost == "" {
		return fmt.Errorf("runta: ssh_host is not configured, so no command can be executed; " +
			"set it from Runta's SSH gateway documentation")
	}
	if b.p.signer == nil {
		return fmt.Errorf("runta: ssh_private_key is not configured, so no command can be executed")
	}
	hostKey, err := b.p.hostKeyCallback(ctx)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(b.p.sshHost, strconv.Itoa(b.p.sshPort))
	d := net.Dialer{Timeout: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("runta: dial %s: %w", addr, err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User:            fmt.Sprintf(b.p.sshUserTpl, b.id),
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(b.p.signer)},
		HostKeyCallback: hostKey,
		Timeout:         30 * time.Second,
	})
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("runta: ssh handshake with %s: %w", addr, err)
	}

	b.mu.Lock()
	b.client = ssh.NewClient(cc, chans, reqs)
	b.mu.Unlock()
	return nil
}

func (b *box) conn(ctx context.Context) (*ssh.Client, error) {
	b.mu.Lock()
	c := b.client
	b.mu.Unlock()
	if c != nil {
		return c, nil
	}
	if err := b.dial(ctx); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.client, nil
}

func (b *box) Exec(ctx context.Context, c sandbox.Command) (sandbox.ExecResult, error) {
	client, err := b.conn(ctx)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	sess, err := client.NewSession()
	if err != nil {
		return sandbox.ExecResult{}, fmt.Errorf("runta: new ssh session: %w", err)
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	for k, v := range c.Env {
		// A server with a restrictive AcceptEnv ignores these; the command
		// line below carries the same variables as a fallback.
		_ = sess.Setenv(k, v)
	}

	line := commandLine(c)

	// The session has no context support, so cancellation closes it.
	done := make(chan error, 1)
	go func() { done <- sess.Run(line) }()

	t0 := time.Now()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		return sandbox.ExecResult{Wall: time.Since(t0)}, ctx.Err()
	case err = <-done:
	}
	res := sandbox.ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
		Wall:   time.Since(t0),
	}
	var ee *ssh.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case asExitError(err, &ee):
		res.ExitCode = ee.ExitStatus()
	default:
		return res, fmt.Errorf("runta: exec: %w", err)
	}
	return res, nil
}

// commandLine renders a Command as one shell line, including any cwd and env,
// so a server that refuses SendEnv still runs the right thing.
func commandLine(c sandbox.Command) string {
	var b strings.Builder
	if c.Cwd != "" {
		b.WriteString("cd " + shQuote(c.Cwd) + " && ")
	}
	for k, v := range c.Env {
		b.WriteString(k + "=" + shQuote(v) + " ")
	}
	b.WriteString(c.Cmd)
	for _, a := range c.Args {
		b.WriteString(" " + shQuote(a))
	}
	return b.String()
}

func (b *box) WriteFile(ctx context.Context, path string, data []byte) error {
	u := "/v2/runtimes/" + url.PathEscape(b.id) + "/files?path=" + url.QueryEscape(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, b.p.api.BaseURL+u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	for k, v := range b.p.api.Header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := b.p.api.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("runta: write %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("runta: write %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (b *box) ReadFile(ctx context.Context, path string) ([]byte, error) {
	u := "/v2/runtimes/" + url.PathEscape(b.id) + "/files?path=" + url.QueryEscape(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.p.api.BaseURL+u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range b.p.api.Header {
		req.Header[k] = v
	}
	resp, err := b.p.api.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runta: read %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("runta: read %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

func (b *box) Suspend(ctx context.Context) error {
	b.closeConn()
	if err := b.p.api.Do(ctx, http.MethodPost, "/v2/runtimes/"+url.PathEscape(b.id)+"/pause", nil, nil, nil); err != nil {
		return fmt.Errorf("runta: pause: %w", err)
	}
	return httpx.PollUntil(ctx, b.p.pollInterval, b.p.readyTimeout, func(ctx context.Context) (bool, error) {
		r, err := b.p.get(ctx, b.id)
		if err != nil {
			return false, err
		}
		return r.Status == "paused" || r.Status == "suspended", nil
	})
}

func (b *box) Resume(ctx context.Context) error {
	if err := b.p.api.Do(ctx, http.MethodPost, "/v2/runtimes/"+url.PathEscape(b.id)+"/resume", nil, nil, nil); err != nil {
		return fmt.Errorf("runta: resume: %w", err)
	}
	if err := b.p.waitRunning(ctx, b.id); err != nil {
		return err
	}
	// The gateway drops the old SSH connection across a pause, so resume
	// latency is only honest if it includes reconnecting.
	return b.dial(ctx)
}

func (b *box) Destroy(ctx context.Context) error {
	b.closeConn()
	if err := b.p.api.Do(ctx, http.MethodDelete, "/v2/runtimes/"+url.PathEscape(b.id), nil, nil, nil); err != nil {
		return fmt.Errorf("runta: delete: %w", err)
	}
	return nil
}

func (b *box) closeConn() {
	b.mu.Lock()
	c := b.client
	b.client = nil
	b.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func asExitError(err error, target **ssh.ExitError) bool {
	if ee, ok := err.(*ssh.ExitError); ok {
		*target = ee
		return true
	}
	return false
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
