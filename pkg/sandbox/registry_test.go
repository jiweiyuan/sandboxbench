package sandbox_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jiweiyuan/sandboxbench/pkg/sandbox"

	_ "github.com/jiweiyuan/sandboxbench/providers/daytona"
	_ "github.com/jiweiyuan/sandboxbench/providers/e2b"
	_ "github.com/jiweiyuan/sandboxbench/providers/local"
	_ "github.com/jiweiyuan/sandboxbench/providers/runta"
)

func TestEveryAdapterRegisters(t *testing.T) {
	got := sandbox.Registered()
	for _, want := range []string{"daytona", "e2b", "local", "runta"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s did not register; have %v", want, got)
		}
	}
}

// A missing credential must fail at construction, before any sandbox exists
// and before any money is spent.
func TestHostedAdaptersRefuseAnEmptyAPIKey(t *testing.T) {
	for _, name := range []string{"daytona", "e2b", "runta"} {
		_, err := sandbox.New(name, map[string]any{})
		if err == nil {
			t.Errorf("%s accepted an empty configuration", name)
			continue
		}
		if !strings.Contains(err.Error(), "api_key") {
			t.Errorf("%s: error does not name the missing key: %v", name, err)
		}
	}
}

func TestUnknownProviderNamesTheAlternatives(t *testing.T) {
	_, err := sandbox.New("nope", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "local") {
		t.Errorf("error does not list what is available: %v", err)
	}
}

func TestCfgExpandsEnvironmentReferences(t *testing.T) {
	t.Setenv("SANDBOXBENCH_TEST_TOKEN", "value-from-env")
	c := sandbox.Cfg{"api_key": "${SANDBOXBENCH_TEST_TOKEN}", "plain": "literal"}
	if got := c.String("api_key", ""); got != "value-from-env" {
		t.Errorf("api_key = %q", got)
	}
	if got := c.String("plain", ""); got != "literal" {
		t.Errorf("plain = %q", got)
	}
	if got := c.String("absent", "fallback"); got != "fallback" {
		t.Errorf("absent = %q", got)
	}
}
