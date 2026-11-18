package citest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"time"
)

type set map[string]struct{}

func (s set) add(v string)      { s[v] = struct{}{} }
func (s set) has(v string) bool { _, ok := s[v]; return ok }
func (s set) sorted() []string {
	out := make([]string, 0, len(s))
	for v := range s {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}
func newSet(names []string) set {
	s := set{}
	for _, n := range names {
		s.add(n)
	}
	return s
}
func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if ok && (k == key || runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
			return v
		}
	}
	return ""
}
func withEnv(env []string, values map[string]string) []string {
	out := append([]string{}, env...)
	for key, value := range values {
		for i := len(out) - 1; i >= 0; i-- {
			k, _, _ := strings.Cut(out[i], "=")
			if k == key || runtime.GOOS == "windows" && strings.EqualFold(k, key) {
				out = append(out[:i], out[i+1:]...)
			}
		}
		out = append(out, key+"="+value)
	}
	return out
}

type commandFunc func(context.Context, string, []string, ...string) ([]byte, error)

func metadataCommand(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	configureProcess(cmd)
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("metadata command failed: %s %s: %w", args[0], args[1], err)
	}
	return out, nil
}
func baseEnv(env []string) []string {
	if env == nil {
		return os.Environ()
	}
	return env
}
