package sandbox

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Cfg reads typed values out of an adapter config block. Every getter resolves
// a ${ENV_VAR} value against the environment, so a config file committed to a
// repository never has to hold a token.
type Cfg map[string]any

func (c Cfg) String(key, def string) string {
	v, ok := c[key]
	if !ok {
		return expand(def)
	}
	s, ok := v.(string)
	if !ok {
		return expand(def)
	}
	return expand(s)
}

func (c Cfg) Int(key string, def int) int {
	switch v := c[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return def
	}
}

func (c Cfg) Bool(key string, def bool) bool {
	if b, ok := c[key].(bool); ok {
		return b
	}
	return def
}

func (c Cfg) Duration(key string, def time.Duration) time.Duration {
	s, ok := c[key].(string)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// Require returns an error naming the missing key, so a misconfigured run
// fails before it creates any sandbox.
func (c Cfg) Require(key string) (string, error) {
	s := c.String(key, "")
	if s == "" {
		return "", fmt.Errorf("sandboxbench: config key %q is required and empty", key)
	}
	return s, nil
}

func expand(s string) string {
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") {
		return s
	}
	return os.Getenv(strings.TrimSuffix(strings.TrimPrefix(s, "${"), "}"))
}
