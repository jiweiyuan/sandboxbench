// Package e2b adapts E2B's sandbox API.
//
// E2B splits its surface in two, and the split matters for what the benchmarks
// measure. Lifecycle goes to the control plane at api.e2b.dev. Command
// execution goes straight to envd, a daemon inside the sandbox, over the
// Connect protocol with a server-streaming response. One command is therefore
// a streaming RPC to the sandbox rather than a request to a control plane,
// which is a different latency class from a per-command REST call.
//
// Verification status: every path, field name and status value here comes from
// api.e2b.dev's OpenAPI document and from the envd spec and process.proto in
// e2b-dev/infra. Nothing in this file has been run against the live API.
package e2b

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
	"github.com/jiweiyuan/sandboxbench/providers/internal/httpx"
)

func init() {
	sandbox.Register("e2b", New)
}

const (
	defaultAPI      = "https://api.e2b.dev"
	defaultDomain   = "e2b.app"
	defaultEnvdPort = 49983
)

type provider struct {
	api        *httpx.Client
	http       *http.Client
	apiKey     string
	template   string
	domain     string
	envdPort   int
	envdScheme string
	timeoutSec int
}

// New builds the E2B adapter.
//
// Config keys:
//
//	api_key:   X-API-Key value, usually "${E2B_API_KEY}"
//	base_url:  control plane base (default https://api.e2b.dev)
//	template:  template id to create from (default "base")
//	domain:    sandbox traffic domain (default e2b.app; the create response
//	           overrides it when it carries one)
//	envd_port: envd port on the sandbox host (default 49983)
//	timeout:   sandbox time to live; also the auto-kill deadline
func New(raw map[string]any) (sandbox.Provider, error) {
	cfg := sandbox.Cfg(raw)
	key, err := cfg.Require("api_key")
	if err != nil {
		return nil, fmt.Errorf("e2b: %w", err)
	}
	h := http.Header{}
	h.Set("X-API-Key", key)

	ttl := cfg.Duration("timeout", 15*time.Minute)
	return &provider{
		api:        httpx.New(cfg.String("base_url", defaultAPI), h, cfg.Duration("http_timeout", 0)),
		http:       httpx.New("", nil, cfg.Duration("http_timeout", 0)).HTTP,
		apiKey:     key,
		template:   cfg.String("template", "base"),
		domain:     cfg.String("domain", defaultDomain),
		envdPort:   cfg.Int("envd_port", defaultEnvdPort),
		envdScheme: cfg.String("envd_scheme", "https"),
		timeoutSec: int(ttl.Seconds()),
	}, nil
}

func (p *provider) Name() string { return "e2b" }

func (p *provider) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		Suspend:              true, // POST /sandboxes/{id}/pause
		Resume:               true, // POST /sandboxes/{id}/resume
		Snapshot:             true, // POST /sandboxes/{id}/snapshots
		PersistentFilesystem: true,
		CustomImage:          true,
		PortForwarding:       true,
		ExecTransport:        sandbox.TransportConnectRPC,
	}
}

func (p *provider) Close() error { return nil }

type newSandbox struct {
	TemplateID string            `json:"templateID"`
	Timeout    int               `json:"timeout,omitempty"`
	EnvVars    map[string]string `json:"envVars,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type apiSandbox struct {
	SandboxID       string `json:"sandboxID"`
	TemplateID      string `json:"templateID"`
	Domain          string `json:"domain"`
	EnvdAccessToken string `json:"envdAccessToken"`
}

func (p *provider) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	body := newSandbox{
		TemplateID: firstNonEmpty(opts.Image, p.template),
		Timeout:    p.timeoutSec,
		EnvVars:    opts.Env,
		Metadata:   opts.Labels,
	}
	if opts.TTL > 0 {
		body.Timeout = int(opts.TTL.Seconds())
	}

	var created apiSandbox
	// /v2/sandboxes is the current create endpoint; /sandboxes is deprecated.
	if err := p.api.Do(ctx, http.MethodPost, "/v2/sandboxes", body, &created, nil); err != nil {
		return nil, fmt.Errorf("e2b: create: %w", err)
	}
	domain := firstNonEmpty(created.Domain, p.domain)
	b := &box{
		p:     p,
		id:    created.SandboxID,
		token: created.EnvdAccessToken,
		envd:  fmt.Sprintf("%s://%d-%s.%s", p.envdScheme, p.envdPort, created.SandboxID, domain),
	}

	// The control plane returns before envd is necessarily serving, so
	// readiness is confirmed against envd itself. Create latency then means
	// create-to-first-exec-possible for every provider in a run.
	if err := b.waitHealthy(ctx, 2*time.Minute); err != nil {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_ = b.Destroy(tctx)
		return nil, err
	}
	return b, nil
}

type box struct {
	p     *provider
	id    string
	token string
	envd  string
}

func (b *box) ID() string       { return b.id }
func (b *box) Provider() string { return "e2b" }

func (b *box) envdHeader() http.Header {
	h := http.Header{}
	if b.token != "" {
		h.Set("X-Access-Token", b.token)
	}
	return h
}

func (b *box) waitHealthy(ctx context.Context, limit time.Duration) error {
	return httpx.PollUntil(ctx, 100*time.Millisecond, limit, func(ctx context.Context) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.envd+"/health", nil)
		if err != nil {
			return false, err
		}
		for k, v := range b.envdHeader() {
			req.Header[k] = v
		}
		resp, err := b.p.http.Do(req)
		if err != nil {
			return false, nil // envd not reachable yet
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK, nil
	})
}

// --- Connect protocol, server-streaming -------------------------------------
//
// A Connect stream frames each message as a five-byte envelope followed by the
// payload: one flag byte, then a big-endian uint32 length. Flag bit 0x02 marks
// the end-of-stream message, whose payload carries the error when there is one.
// Implementing those two rules directly costs less than generating stubs and
// keeps the adapter dependency-free.

const (
	connectFlagEndStream = 0x02
	connectMaxMessage    = 8 << 20
)

type startRequest struct {
	Process processConfig `json:"process"`
	Stdin   bool          `json:"stdin"`
}

type processConfig struct {
	Cmd  string            `json:"cmd"`
	Args []string          `json:"args,omitempty"`
	Envs map[string]string `json:"envs,omitempty"`
	Cwd  string            `json:"cwd,omitempty"`
}

type startResponse struct {
	Event struct {
		Start *struct {
			PID uint32 `json:"pid"`
		} `json:"start"`
		Data *struct {
			// proto3 JSON encodes bytes as base64 strings.
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
		} `json:"data"`
		End *struct {
			ExitCode int32  `json:"exitCode"`
			Exited   bool   `json:"exited"`
			Status   string `json:"status"`
			Error    string `json:"error"`
		} `json:"end"`
	} `json:"event"`
}

type connectEndStream struct {
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func encodeEnvelope(flags byte, payload []byte) []byte {
	buf := make([]byte, 5+len(payload))
	buf[0] = flags
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return buf
}

func (b *box) Exec(ctx context.Context, c sandbox.Command) (sandbox.ExecResult, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	payload, err := json.Marshal(startRequest{
		Process: processConfig{Cmd: c.Cmd, Args: c.Args, Envs: c.Env, Cwd: c.Cwd},
		Stdin:   false,
	})
	if err != nil {
		return sandbox.ExecResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.envd+"/process.Process/Start", bytes.NewReader(encodeEnvelope(0, payload)))
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	for k, v := range b.envdHeader() {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")

	t0 := time.Now()
	resp, err := b.p.http.Do(req)
	if err != nil {
		return sandbox.ExecResult{Wall: time.Since(t0)}, fmt.Errorf("e2b: exec: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return sandbox.ExecResult{Wall: time.Since(t0)},
			fmt.Errorf("e2b: exec: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var stdout, stderr bytes.Buffer
	exit := -1
	for {
		var head [5]byte
		if _, err := io.ReadFull(resp.Body, head[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return sandbox.ExecResult{Wall: time.Since(t0)}, fmt.Errorf("e2b: exec: read envelope: %w", err)
		}
		n := binary.BigEndian.Uint32(head[1:5])
		if n > connectMaxMessage {
			return sandbox.ExecResult{Wall: time.Since(t0)},
				fmt.Errorf("e2b: exec: stream message of %d bytes exceeds the limit", n)
		}
		msg := make([]byte, n)
		if _, err := io.ReadFull(resp.Body, msg); err != nil {
			return sandbox.ExecResult{Wall: time.Since(t0)}, fmt.Errorf("e2b: exec: read message: %w", err)
		}

		if head[0]&connectFlagEndStream != 0 {
			var end connectEndStream
			if err := json.Unmarshal(msg, &end); err == nil && end.Error != nil {
				return sandbox.ExecResult{Wall: time.Since(t0)},
					fmt.Errorf("e2b: exec: stream error %s: %s", end.Error.Code, end.Error.Message)
			}
			break
		}

		var sr startResponse
		if err := json.Unmarshal(msg, &sr); err != nil {
			return sandbox.ExecResult{Wall: time.Since(t0)}, fmt.Errorf("e2b: exec: decode event: %w", err)
		}
		switch {
		case sr.Event.Data != nil:
			appendB64(&stdout, sr.Event.Data.Stdout)
			appendB64(&stderr, sr.Event.Data.Stderr)
		case sr.Event.End != nil:
			exit = int(sr.Event.End.ExitCode)
		}
	}
	wall := time.Since(t0)

	if exit < 0 {
		return sandbox.ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), Wall: wall},
			fmt.Errorf("e2b: exec: stream ended without an exit code")
	}
	return sandbox.ExecResult{
		ExitCode: exit,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Wall:     wall,
	}, nil
}

// appendB64 decodes a proto3-JSON bytes field into buf. A field that is not
// valid base64 is appended verbatim, so a protocol change degrades into
// readable output instead of an empty one.
func appendB64(buf *bytes.Buffer, s string) {
	if s == "" {
		return
	}
	if dec, err := base64.StdEncoding.DecodeString(s); err == nil {
		buf.Write(dec)
		return
	}
	buf.WriteString(s)
}

// --- envd file endpoints ----------------------------------------------------

func (b *box) WriteFile(ctx context.Context, path string, data []byte) error {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", path)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	u := b.envd + "/files?path=" + url.QueryEscape(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &body)
	if err != nil {
		return err
	}
	for k, v := range b.envdHeader() {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := b.p.http.Do(req)
	if err != nil {
		return fmt.Errorf("e2b: write %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("e2b: write %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

func (b *box) ReadFile(ctx context.Context, path string) ([]byte, error) {
	u := b.envd + "/files?path=" + url.QueryEscape(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range b.envdHeader() {
		req.Header[k] = v
	}
	resp, err := b.p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("e2b: read %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("e2b: read %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

// --- lifecycle --------------------------------------------------------------

func (b *box) Suspend(ctx context.Context) error {
	if err := b.p.api.Do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(b.id)+"/pause", nil, nil, nil); err != nil {
		return fmt.Errorf("e2b: pause: %w", err)
	}
	return nil
}

type resumeBody struct {
	Timeout int `json:"timeout,omitempty"`
}

func (b *box) Resume(ctx context.Context) error {
	var resumed apiSandbox
	err := b.p.api.Do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(b.id)+"/resume",
		resumeBody{Timeout: b.p.timeoutSec}, &resumed, nil)
	if err != nil {
		return fmt.Errorf("e2b: resume: %w", err)
	}
	// A resume can land the sandbox on a different host, so the envd URL and
	// its access token are rebuilt from the response rather than reused.
	if resumed.SandboxID != "" {
		b.id = resumed.SandboxID
	}
	if resumed.EnvdAccessToken != "" {
		b.token = resumed.EnvdAccessToken
	}
	domain := firstNonEmpty(resumed.Domain, b.p.domain)
	b.envd = fmt.Sprintf("%s://%d-%s.%s", b.p.envdScheme, b.p.envdPort, b.id, domain)
	return b.waitHealthy(ctx, 2*time.Minute)
}

func (b *box) Destroy(ctx context.Context) error {
	if err := b.p.api.Do(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(b.id), nil, nil, nil); err != nil {
		return fmt.Errorf("e2b: kill: %w", err)
	}
	return nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
