package local

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"
)

func newProcessProvider(t *testing.T) sandbox.Provider {
	t.Helper()
	p, err := New(map[string]any{"mode": "process", "work_dir": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProcessModeExecReportsExitCode(t *testing.T) {
	ctx := context.Background()
	p := newProcessProvider(t)
	box, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Destroy(ctx)

	res, err := box.Exec(ctx, sandbox.Shell("echo hello"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "hello") {
		t.Errorf("got %+v", res)
	}

	// A non-zero exit is a result, not a transport error. A workload needs to
	// tell "the command failed" from "the sandbox is unreachable".
	res, err = box.Exec(ctx, sandbox.Shell("exit 7"))
	if err != nil {
		t.Fatalf("non-zero exit returned an error: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", res.ExitCode)
	}
	if res.OK() {
		t.Error("OK() true for a non-zero exit")
	}
}

func TestProcessModeFileRoundTrip(t *testing.T) {
	ctx := context.Background()
	p := newProcessProvider(t)
	box, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Destroy(ctx)

	path := t.TempDir() + "/nested/file.bin"
	want := []byte{0x00, 0x01, 0xff, 'h', 'i', '\n'}
	if err := box.WriteFile(ctx, path, want); err != nil {
		t.Fatal(err)
	}
	got, err := box.ReadFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("round trip changed the bytes: %v != %v", got, want)
	}
}

func TestProcessModeReportsSuspendUnsupported(t *testing.T) {
	ctx := context.Background()
	p := newProcessProvider(t)
	if p.Capabilities().Suspend {
		t.Error("process mode claims suspend")
	}
	box, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Destroy(ctx)

	// The sentinel matters: the runner turns exactly this error into the
	// unsupported status rather than into a failure.
	if err := box.Suspend(ctx); !errors.Is(err, sandbox.ErrUnsupported) {
		t.Errorf("Suspend returned %v, want ErrUnsupported", err)
	}
	if err := box.Resume(ctx); !errors.Is(err, sandbox.ErrUnsupported) {
		t.Errorf("Resume returned %v, want ErrUnsupported", err)
	}
}

func TestDockerModeClaimsSuspend(t *testing.T) {
	p, err := New(map[string]any{"mode": "docker"})
	if err != nil {
		t.Skipf("docker not available: %v", err)
	}
	c := p.Capabilities()
	if !c.Suspend || !c.Resume {
		t.Error("docker mode should claim suspend and resume via pause/unpause")
	}
}

func TestUnknownModeIsRejected(t *testing.T) {
	if _, err := New(map[string]any{"mode": "wishful"}); err == nil {
		t.Error("expected an error for an unknown mode")
	}
}
