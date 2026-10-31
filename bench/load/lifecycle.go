package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type privateRepo struct {
	Corpus Corpus
	Path   string
	File   string
	Prefix string
}

type privateRun struct {
	Root       string
	SocketDir  string
	Socket     string
	Store      string
	Config     string
	Env        []string
	Repos      []privateRepo
	Binary     string
	process    *exec.Cmd
	done       chan struct{}
	exitErr    error
	stdout     *os.File
	stderr     *os.File
	startCount int
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func loadManifest(path string) ([]Corpus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var corpora []Corpus
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&corpora); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("manifest must contain one JSON array")
	}
	if len(corpora) == 0 {
		return nil, errors.New("manifest has no repositories")
	}
	labels := make(map[string]bool)
	for _, corpus := range corpora {
		if err := validateCorpus(corpus); err != nil {
			return nil, err
		}
		if labels[corpus.Label] {
			return nil, fmt.Errorf("duplicate corpus label %s", corpus.Label)
		}
		labels[corpus.Label] = true
	}
	return corpora, nil
}

func validateOutputRoots(corpora []Corpus, paths ...string) error {
	for _, corpus := range corpora {
		source, err := canonicalPath(corpus.Source)
		if err != nil {
			continue
		}
		for _, output := range paths {
			if output == "" {
				continue
			}
			path, err := canonicalPath(output)
			if err != nil {
				return err
			}
			if inside(source, path) {
				return errors.New("scratch/report output must be outside every manifest source root")
			}
		}
	}
	return nil
}

// Only this function allocates mutation ownership. Manifest roots are inputs;
// no edit or daemon is ever pointed at them. Every invocation starts fresh.
func newPrivateRun(ctx context.Context, scratch, binary string, corpora []Corpus) (_ *privateRun, err error) {
	if len(corpora) == 0 {
		return nil, errors.New("private run requires a corpus")
	}
	scratch, err = canonicalPath(scratch)
	if err != nil {
		return nil, err
	}
	for _, c := range corpora {
		if err := validateCorpus(c); err != nil {
			return nil, err
		}
		if source, sourceErr := canonicalPath(c.Source); sourceErr == nil {
			if inside(source, scratch) {
				return nil, errors.New("scratch must be outside every manifest source root")
			}
		}
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(scratch, "load-")
	if err != nil {
		return nil, err
	}
	p := &privateRun{Root: root, Binary: binary, Store: filepath.Join(root, "store.db"), Config: filepath.Join(root, "config.yaml")}
	// Short sockets are the sole exception to the caller-selected artifact
	// root. This freshly owned directory is removed after the process exits.
	socketBase := os.TempDir()
	if runtime.GOOS != "windows" {
		socketBase = "/tmp"
	}
	p.SocketDir, err = os.MkdirTemp(socketBase, "gl-")
	if err != nil {
		return nil, err
	}
	p.Socket = filepath.Join(p.SocketDir, "d.sock")
	defer func() {
		if err != nil {
			_ = os.RemoveAll(p.SocketDir)
			_ = p.removeState()
			err = fmt.Errorf("private preparation at %s: %w", p.Root, err)
		}
	}()
	for _, dir := range []string{"home", "cache", "config", "data", "runtime", "tmp", "repos", "hooks"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(p.Config, []byte("{}\n"), 0o600); err != nil {
		return nil, err
	}
	gitConfig := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(gitConfig, nil, 0o600); err != nil {
		return nil, err
	}
	p.Env = privateEnv(root, p.Socket, gitConfig)
	for _, corpus := range corpora {
		dest := filepath.Join(root, "repos", corpus.Label)
		if _, err := p.git(ctx, "", "clone", "--no-hardlinks", "--no-checkout", "--", corpus.Source, dest); err != nil {
			return nil, err
		}
		if _, err := p.git(ctx, dest, "checkout", "--detach", corpus.Commit); err != nil {
			return nil, err
		}
		sha, err := p.git(ctx, dest, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(strings.TrimSpace(string(sha)), corpus.Commit) {
			return nil, fmt.Errorf("clone %s does not match pinned commit", corpus.Label)
		}
		file, err := ownedFile(dest, corpus.File)
		if err != nil {
			return nil, fmt.Errorf("corpus %s: %w", corpus.Label, err)
		}
		p.Repos = append(p.Repos, privateRepo{Corpus: corpus, Path: dest, File: file})
	}
	return p, nil
}

// Resolve existing ancestors as well as the final component: a not-yet-created
// scratch directory below a symlink must not bypass source-root protection.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(abs)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", err
		}
		suffix = append(suffix, filepath.Base(abs))
		abs = parent
	}
}

func privateEnv(root, socket, gitConfig string) []string {
	values := map[string]string{
		"HOME": filepath.Join(root, "home"), "USERPROFILE": filepath.Join(root, "home"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"), "XDG_CACHE_HOME": filepath.Join(root, "cache"), "XDG_DATA_HOME": filepath.Join(root, "data"), "XDG_RUNTIME_DIR": filepath.Join(root, "runtime"),
		"APPDATA": filepath.Join(root, "config"), "LOCALAPPDATA": filepath.Join(root, "cache"),
		"TMPDIR": filepath.Join(root, "tmp"), "TMP": filepath.Join(root, "tmp"), "TEMP": filepath.Join(root, "tmp"),
		"GORTEX_DAEMON_SOCKET": socket, "GORTEX_DAEMON_PIDFILE": filepath.Join(root, "daemon.pid"), "GORTEX_DAEMON_LOGFILE": filepath.Join(root, "daemon.log"),
		"GORTEX_DAEMON_PPROF_ADDR": "127.0.0.1:0", "GORTEX_TOOLS": "facade-v1", "GORTEX_TOOLS_MODE": "hide",
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": gitConfig, "GIT_TERMINAL_PROMPT": "0",
	}
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := values[key]; !replaced && !strings.HasPrefix(key, "GORTEX_") && !strings.HasPrefix(key, "GIT_") {
			env = append(env, entry)
		}
	}
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func ownedFile(root, relative string) (string, error) {
	path, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		return "", err
	}
	if !inside(root, path) {
		return "", errors.New("target resolves outside the private clone")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("target is not a regular file")
	}
	return path, nil
}

func (p *privateRun) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	operation := args[0]
	args = append([]string{"-c", "core.hooksPath=" + filepath.Join(p.Root, "hooks"), "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	configureOwnedProcess(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return killOwnedProcess(cmd)
	}
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir, cmd.Env = dir, p.Env
	var stderr diagnosticBuffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		if cmd.Process != nil {
			_ = cleanupExitedOwnedProcess(cmd)
		}
		return nil, fmt.Errorf("git %s in %s: %w; stderr: %s", operation, dir, err, stderr.String())
	}
	return data, nil
}

func (p *privateRun) addWorktrees(ctx context.Context, count int) error {
	if len(p.Repos) != 1 {
		return errors.New("worktree scenario requires exactly one primary corpus")
	}
	primary := p.Repos[0]
	for i := range count {
		path := filepath.Join(p.Root, "repos", fmt.Sprintf("linked-%d", i+1))
		if _, err := p.git(ctx, primary.Path, "worktree", "add", "--detach", path, primary.Corpus.Commit); err != nil {
			return err
		}
		file, err := ownedFile(path, primary.Corpus.File)
		if err != nil {
			return err
		}
		corpus := primary.Corpus
		corpus.Label = fmt.Sprintf("linked-%d", i+1)
		p.Repos = append(p.Repos, privateRepo{Corpus: corpus, Path: path, File: file})
	}
	return nil
}

func (p *privateRun) writeFile(repo privateRepo, content []byte) error {
	root, err := canonicalPath(repo.Path)
	if err != nil || !inside(p.Root, root) || root == p.Root {
		return errors.New("edit target is not inside an owned clone")
	}
	registered := false
	for _, owned := range p.Repos {
		registered = registered || owned.Path == repo.Path
	}
	if !registered {
		return errors.New("edit target is not a registered private clone")
	}
	file, err := ownedFile(root, repo.Corpus.File)
	if err != nil {
		return err
	}
	return os.WriteFile(file, content, 0o600)
}

func (p *privateRun) start() error {
	if p.process != nil {
		return errors.New("private daemon already started")
	}
	p.startCount++
	var err error
	p.stdout, err = os.Create(filepath.Join(p.Root, fmt.Sprintf("daemon-%d.stdout.log", p.startCount)))
	if err != nil {
		return err
	}
	p.stderr, err = os.Create(filepath.Join(p.Root, fmt.Sprintf("daemon-%d.stderr.log", p.startCount)))
	if err != nil {
		_ = p.stdout.Close()
		return err
	}
	cmd := exec.Command(p.Binary, "--config", p.Config, "daemon", "start", "--backend-path", p.Store, "--tools", "facade-v1", "--tools-mode", "hide")
	configureOwnedProcess(cmd)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = p.Root, p.Env, p.stdout, p.stderr
	if err := cmd.Start(); err != nil {
		_ = p.stdout.Close()
		_ = p.stderr.Close()
		return err
	}
	p.process, p.done = cmd, make(chan struct{})
	p.exitErr = nil
	go func() {
		p.exitErr = cmd.Wait()
		close(p.done)
	}()
	return nil
}

func (p *privateRun) stop(ctx context.Context) (elapsed time.Duration, forced bool, err error) {
	if p.process == nil {
		return 0, false, nil
	}
	start := time.Now()
	defer func() {
		elapsed = time.Since(start)
		p.process = nil
		err = errors.Join(err, p.stdout.Close(), p.stderr.Close())
	}()
	select {
	case <-p.done:
		return 0, false, errors.Join(errors.New("private daemon exited before requested shutdown"), p.exitErr, cleanupExitedOwnedProcess(p.process))
	default:
	}
	_, controlErr := control(ctx, p.Socket, "shutdown", nil)
	select {
	case <-p.done:
		return 0, false, errors.Join(controlErr, p.exitErr, cleanupExitedOwnedProcess(p.process))
	case <-ctx.Done():
		forced = true
		killErr := killOwnedProcess(p.process)
		<-p.done
		return 0, true, errors.Join(ctx.Err(), controlErr, killErr, p.exitErr)
	}
}

func (p *privateRun) removeState() error {
	var result error
	for _, name := range []string{"repos", "cache", "data", "home", "config", "runtime", "tmp", "store.db", "store.db-wal", "store.db-shm"} {
		result = errors.Join(result, os.RemoveAll(filepath.Join(p.Root, name)))
	}
	return result
}
