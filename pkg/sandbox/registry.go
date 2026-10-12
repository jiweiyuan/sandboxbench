package sandbox

import (
	"fmt"
	"sort"
	"sync"
)

// Factory builds a provider from the adapter-specific config block of a run
// configuration file.
type Factory func(cfg map[string]any) (Provider, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes an adapter available to the CLI. Adapters call it from an
// init function; cmd/sandboxbench imports each adapter package for its side
// effect, which is the only place the binary knows the provider set.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic("sandboxbench: duplicate provider " + name)
	}
	factories[name] = f
}

// New builds a registered provider.
func New(name string, cfg map[string]any) (Provider, error) {
	mu.RLock()
	f, ok := factories[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandboxbench: unknown provider %q (have %v)", name, Registered())
	}
	return f(cfg)
}

// Registered lists adapter names in sorted order.
func Registered() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
