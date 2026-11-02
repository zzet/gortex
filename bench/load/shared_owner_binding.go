package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/platform"
)

// A binding names the existing owned child and report arm. It neither creates
// an execution identity nor grants a producer's server/recorder/member claims.
// Capture is used only after an actual shared proof is encountered; ordinary
// point accounting performs no additional file or native process lookup.
type editProbeSharedBinding struct {
	run        *privateRun
	command    *exec.Cmd
	done       <-chan struct{}
	startCount int
	identity   platform.ProcessIdentity
	birth      string
	product    string
	role       string
	root       string
	prefix     string
	store      string
}

func sharedBindingEnvironment(env []string, key string) (string, error) {
	value, found := "", false
	for _, entry := range env {
		name, candidate, _ := strings.Cut(entry, "=")
		if name == key {
			if found {
				return "", errors.New("ambiguous owned child environment")
			}
			value, found = candidate, true
		}
	}
	return value, nil
}

func captureEditProbeSharedBinding(ctx context.Context, p *privateRun, role string) (*editProbeSharedBinding, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errors.New("shared proof binding context unavailable")
	}
	if p == nil || p.process == nil || p.process.Process == nil || p.done == nil || p.startCount <= 0 || len(p.Repos) == 0 || role != "candidate" ||
		!filepath.IsAbs(p.Binary) || p.process.Path != p.Binary || !filepath.IsAbs(p.Root) || !filepath.IsAbs(p.Store) {
		return nil, errors.New("shared proof has no actual owned daemon/product/arm")
	}
	select {
	case <-p.done:
		return nil, errors.New("shared proof belongs to an exited owned child")
	default:
	}
	statePath, err := sharedBindingEnvironment(p.process.Env, "GORTEX_DAEMON_STATEFILE")
	if err != nil {
		return nil, err
	}
	if statePath == "" {
		cache, envErr := sharedBindingEnvironment(p.process.Env, "XDG_CACHE_HOME")
		if envErr != nil || !filepath.IsAbs(cache) {
			return nil, errors.New("owned startup state location unavailable")
		}
		statePath = filepath.Join(cache, "gortex", "daemon.state.json")
	}
	if !filepath.IsAbs(statePath) || !inside(p.Root, statePath) {
		return nil, errors.New("startup state is outside the owned runtime")
	}
	relative, err := filepath.Rel(p.Root, statePath)
	if err != nil {
		return nil, errors.New("owned startup state path unavailable")
	}
	files, err := source.NewFilesystemSource(p.Root)
	if err != nil {
		return nil, errors.New("owned startup runtime cannot be opened")
	}
	defer files.Close()
	raw, _, err := files.ReadRegularFile(ctx, filepath.ToSlash(relative), 1<<20)
	if err != nil {
		return nil, errors.New("owned startup state cannot be read")
	}
	var state daemon.RuntimeState
	if err := json.Unmarshal(raw, &state); err != nil || state.PID != p.process.Process.Pid || state.StartupPhase != daemon.StartupServing || state.BackendPath != p.Store {
		return nil, errors.New("startup state does not identify the owned serving child/store")
	}
	identity, err := platform.CaptureProcessIdentity(p.process.Process.Pid)
	if err != nil {
		return nil, errors.New("owned child native process identity unavailable")
	}
	birth, err := identity.Token()
	if err != nil || birth == "" || birth != state.ProcessBirthToken {
		return nil, errors.New("owned child incarnation differs from actual startup state")
	}
	root := p.Repos[0].Path
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !inside(p.Root, root) {
		return nil, errors.New("actual primary checkout is outside the owned runtime")
	}
	binding := &editProbeSharedBinding{run: p, command: p.process, done: p.done, startCount: p.startCount, identity: identity, birth: birth, product: p.Binary, role: role, root: root, prefix: p.Repos[0].Prefix, store: p.Store}
	if err := binding.validate(p, role); err != nil {
		return nil, err
	}
	return binding, nil
}

func (b *editProbeSharedBinding) validate(p *privateRun, role string) error {
	if b == nil || p == nil || b.run != p || b.command != p.process || b.done != p.done || b.startCount != p.startCount || b.role != role || role != "candidate" ||
		b.product != p.Binary || p.process == nil || p.process.Path != b.product || p.process.Process == nil || p.process.Process.Pid != b.identity.PID() || b.store != p.Store ||
		len(p.Repos) == 0 || p.Repos[0].Path != b.root || p.Repos[0].Prefix != b.prefix {
		return errors.New("shared proof binding belongs to another run/product/arm/incarnation")
	}
	select {
	case <-b.done:
		return errors.New("shared proof owned child has exited")
	default:
	}
	running, err := b.identity.Running()
	if err != nil || !running {
		return errors.New("shared proof owned child incarnation is not running")
	}
	return nil
}
