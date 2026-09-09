package serverstack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/platform"
	"go.uber.org/zap"
)

const (
	trackRepoCtxChildEnv    = "GORTEX_TEST_TRACK_REPO_CTX_COLD_NOOP_CHILD_V1"
	trackRepoCtxChildRun    = "^TestTrackRepoCtxColdAndNoop$"
	trackRepoCtxChildMarker = "GORTEX_TRACK_REPO_CTX_COLD_NOOP_CLEANUP_OK_V1="
)

// TestTrackRepoCtxColdAndNoop runs the real stack only in a fresh, isolated
// subprocess. Its environment and working directory are private before package
// initialization; the ordinary parent never constructs a SharedServer.
func TestTrackRepoCtxColdAndNoop(t *testing.T) {
	if token := os.Getenv(trackRepoCtxChildEnv); token != "" {
		decoded, err := hex.DecodeString(token)
		isolation := os.Getenv("GORTEX_TEST_ISOLATION_ROOT")
		if err != nil || len(decoded) != 16 || !filepath.IsAbs(isolation) || filepath.Base(isolation) != token {
			t.Fatal("invalid isolated tracking child token/root")
		}
		if flag.Lookup("test.run").Value.String() != trackRepoCtxChildRun ||
			flag.Lookup("test.count").Value.String() != "1" {
			t.Fatal("isolated tracking child requires the exact selector and count=1")
		}
		// Register first so this runs after every fixture cleanup. A skip is not
		// successful execution, even if the child process otherwise exits zero.
		t.Cleanup(func() {
			if !t.Failed() && !t.Skipped() {
				t.Log(trackRepoCtxChildMarker + token)
			}
		})
		trackRepoCtxColdAndNoop(t)
		return
	}
	runTrackRepoCtxIsolatedChild(t)
}

func runTrackRepoCtxIsolatedChild(t *testing.T) {
	t.Helper()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(random[:])
	isolation, err := filepath.Abs(filepath.Join(t.TempDir(), token))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(isolation, 0o700); err != nil {
		t.Fatal(err)
	}
	// macOS may expose the same directory as /var/... and /private/var/....
	// Use one physical spelling for Dir and every environment path.
	isolation, err = filepath.EvalSymlinks(isolation)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cwd", "config", "data", "cache", "state", "tmp"} {
		if err := os.Mkdir(filepath.Join(isolation, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required for the isolated tracking regression: %v", err)
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	pathDirs := []string{filepath.Dir(gitPath)}
	childEnv := []string{
		trackRepoCtxChildEnv + "=" + token,
		"GORTEX_TEST_ISOLATION_ROOT=" + isolation,
		"XDG_CONFIG_HOME=" + filepath.Join(isolation, "config"),
		"XDG_DATA_HOME=" + filepath.Join(isolation, "data"),
		"XDG_CACHE_HOME=" + filepath.Join(isolation, "cache"),
		"XDG_STATE_HOME=" + filepath.Join(isolation, "state"),
		"TMPDIR=" + filepath.Join(isolation, "tmp"),
		"TMP=" + filepath.Join(isolation, "tmp"),
		"TEMP=" + filepath.Join(isolation, "tmp"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_CEILING_DIRECTORIES=" + isolation,
		"GOMAXPROCS=2",
		"LANG=C",
		"LC_ALL=C",
	}
	if runtime.GOOS == "windows" {
		// Do not inherit a user profile. Windows standard-library locations
		// remain private even if a provider uses them instead of XDG paths.
		childEnv = append(childEnv,
			"APPDATA="+filepath.Join(isolation, "config"),
			"LOCALAPPDATA="+filepath.Join(isolation, "cache"),
			"USERPROFILE="+isolation,
		)
		for _, key := range []string{"SystemRoot", "WINDIR"} {
			if value := os.Getenv(key); value != "" {
				childEnv = append(childEnv, key+"="+value)
				pathDirs = append(pathDirs, filepath.Join(value, "System32"), value)
			}
		}
	} else {
		pathDirs = append(pathDirs, "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	}
	childEnv = append(childEnv, "PATH="+strings.Join(pathDirs, string(filepath.ListSeparator)))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Existing fixture operations have shorter contexts; the child test bound
	// precedes the outer kill bound. The parent waits for exit before TempDir
	// cleanup, and WaitDelay also bounds inherited output pipes.
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable,
		"-test.run="+trackRepoCtxChildRun, "-test.count=1", "-test.timeout=150s", "-test.v")
	cmd.Dir = filepath.Join(isolation, "cwd")
	cmd.Env = childEnv
	cmd.WaitDelay = 5 * time.Second
	output, err := cmd.CombinedOutput()
	t.Logf("isolated tracking child output:\n%s", output)
	if err != nil {
		t.Fatalf("isolated tracking child failed: %v (context: %v)", err, ctx.Err())
	}
	if strings.Count(string(output), trackRepoCtxChildMarker+token) != 1 {
		t.Fatal("isolated tracking child did not report one post-cleanup success marker")
	}
}

// This verified fixture invokes public TrackRepoCtx directly. It does not
// Register/activate/untrack a checkout or claim watcher, second-dedup Close, or
// exact parser-pass coverage. Only the isolated child calls it.
func trackRepoCtxRuntimeFixture(tb testing.TB) (*SharedServer, *store_sqlite.Store, config.RepoEntry) {
	tb.Helper()
	isolation := os.Getenv("GORTEX_TEST_ISOLATION_ROOT")
	if isolation == "" || !filepath.IsAbs(isolation) {
		tb.Fatal("explicit absolute private isolation root required before package initialization")
	}
	within := func(path string) {
		tb.Helper()
		rel, err := filepath.Rel(isolation, path)
		if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			tb.Fatalf("path outside private isolation root: %q", path)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		tb.Fatal(err)
	}
	within(cwd)
	for key, category := range map[string]string{
		"XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data", "XDG_CACHE_HOME": "cache",
		"XDG_STATE_HOME": "state", "TMPDIR": "tmp",
	} {
		if got, want := os.Getenv(key), filepath.Join(isolation, category); got != want {
			tb.Fatalf("%s=%q; want %q before package initialization", key, got, want)
		}
	}
	for _, path := range []string{platform.ConfigDir(), platform.DataDir(), platform.CacheDir(),
		platform.StoreDir(), platform.ModelsDir(), platform.MemoriesDir(), platform.TelemetryDir()} {
		within(path)
	}
	if os.Getenv("HOME") != "" || os.Getenv("GORTEX_EMBEDDINGS_URL") != "" || os.Getenv("GORTEX_TELEMETRY_ENDPOINT") != "" {
		tb.Fatal("HOME or external provider/telemetry endpoint inherited")
	}
	for key, want := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_TERMINAL_PROMPT": "0", "GIT_NO_LAZY_FETCH": "1",
	} {
		if os.Getenv(key) != want {
			tb.Fatalf("private Git guard %s must be %q", key, want)
		}
	}
	root := tb.TempDir()
	within(root)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		tb.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(tb.Context(), 30*time.Second)
	defer cancel()
	git := func(args ...string) {
		tb.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			tb.Fatalf("private git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--initial-branch=main")
	for name, source := range map[string]string{
		"go.mod":  "module example.test/tracksplit\n\ngo 1.23\n",
		"main.go": "package tracksplit\n\nfunc TrackSplitProbe() int { return 7 }\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(source), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	git("add", "go.mod", "main.go")
	git("-c", "user.name=Isolated Test", "-c", "user.email=isolated@example.test",
		"-c", "commit.gpgsign=false", "commit", "-m", "fixture baseline")
	disabled := false
	conf := &config.Config{}
	conf.Search.RerankEmbedder = &disabled
	conf.Embedding.Enabled = &disabled
	conf.Semantic.Enabled = false
	privateGlobal := &config.GlobalConfig{}
	within(privateGlobal.ConfigPath())
	if _, err := os.Stat(privateGlobal.ConfigPath()); err == nil {
		tb.Fatal("fresh private global config required; each repetition needs a fresh env-i process")
	} else if !os.IsNotExist(err) {
		tb.Fatal(err)
	}
	graphPath := filepath.Join(root, "graph.sqlite")
	srv, err := NewSharedServer(SharedServerConfig{
		Lifecycle: LifecycleDaemon, Backend: "sqlite", BackendPath: graphPath,
		Index: root, Config: conf, Global: privateGlobal,
		Logger: zap.NewNop(), Version: "isolated-track-split",
		Embedder:    EmbedderRequest{FlagChanged: true, FlagEnabled: false},
		SavingsPath: filepath.Join(root, "savings.sqlite"), SavingsLegacyJSON: filepath.Join(root, "savings.json"),
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := srv.Close(); err != nil {
			tb.Errorf("private stack close: %v", err)
		}
	})
	if srv.Graph == nil || srv.Indexer == nil || srv.MultiIndexer == nil || srv.CheckoutLifecycle == nil || srv.ConfigMgr == nil || srv.MCP == nil {
		tb.Fatal("private stack is not fully wired")
	}
	// LIFO: lifecycle closes before the stack/store. There is no invented
	// per-Close deadline; the whole test binary has an explicit timeout.
	tb.Cleanup(func() {
		if err := srv.CheckoutLifecycle.Close(); err != nil {
			tb.Errorf("private lifecycle close: %v", err)
		}
	})
	if srv.StorePath != graphPath {
		tb.Fatalf("store=%q; want private %q", srv.StorePath, graphPath)
	}
	within(srv.ConfigMgr.Global().ConfigPath())
	if srv.ConfigMgr.Global().ConfigPath() != privateGlobal.ConfigPath() {
		tb.Fatal("actual config manager does not use the verified private global path")
	}
	for _, path := range srv.sidecarPaths {
		within(path)
	}
	if srv.LSPRouter != nil || srv.ResolverLSPRegistry != nil || srv.Indexer.SemanticManager() != nil {
		tb.Fatal("disabled semantic providers were unexpectedly constructed")
	}
	store, ok := srv.Graph.(*store_sqlite.Store)
	if !ok {
		tb.Fatalf("actual private graph has unexpected backend %T", srv.Graph)
	}
	return srv, store, config.RepoEntry{Path: repo, Name: "track-split-public"}
}

func trackRepoCtxColdAndNoop(t *testing.T) {
	srv, store, entry := trackRepoCtxRuntimeFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	result, err := srv.MultiIndexer.TrackRepoCtx(ctx, entry)
	if err != nil || result == nil {
		t.Fatalf("real public cold tracking: result=%+v err=%v", result, err)
	}
	t.Logf("actual public cold result: %+v", result)
	probeID := entry.Name + "/main.go::TrackSplitProbe"
	if node := store.GetNode(probeID); node == nil || node.Name != "TrackSplitProbe" || node.FilePath != entry.Name+"/main.go" {
		t.Fatalf("real cold tracking did not persist the expected function: %+v", node)
	}
	// A real source edit makes hidden no-op indexing observable. This test does
	// not Register a checkout or manually trigger any watcher/reconciliation.
	if err := os.WriteFile(filepath.Join(entry.Path, "main.go"), []byte("package tracksplit\n\nfunc TrackSplitProbe() int { return 7 }\nfunc NoopMustNotParse() int { return 11 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		result, err := srv.MultiIndexer.TrackRepoCtx(ctx, entry)
		if result != nil || err != nil {
			t.Fatalf("real public already-tracked result=%+v err=%v", result, err)
		}
	}
	if store.GetNode(probeID) == nil || store.GetNode(entry.Name+"/main.go::NoopMustNotParse") != nil {
		t.Fatal("public no-op lost installed payload or reparsed modified source")
	}
}
