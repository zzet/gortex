package crashpool

import (
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/procio"
)

// defaultRequestTimeout bounds one parse round-trip. A worker that
// exceeds it is presumed hung (a non-crashing pathological file) and is
// killed and respawned, the same as a crash. Generous: the in-process
// parse budget is 5s, and a cold worker also pays registry build time.
const defaultRequestTimeout = 45 * time.Second

// Config configures a Pool.
type Config struct {
	// Argv is the command used to spawn one worker subprocess. In
	// production this is {gortexBinary, "__parse-worker"}.
	Argv []string
	// Env is appended to the inherited environment of every worker.
	Env []string
	// Workers is the number of worker subprocesses. Clamped to >= 1.
	Workers int
	// RequestTimeout bounds one parse round-trip; 0 uses
	// defaultRequestTimeout.
	RequestTimeout time.Duration
	// Logger receives crash / respawn diagnostics. May be nil.
	Logger *zap.Logger
}

// Pool manages a fixed set of parser worker subprocesses and dispatches
// extraction work to them. It is safe for concurrent use: Submit may be
// called from many goroutines at once.
type Pool struct {
	cfg    Config
	free   chan *procWorker
	mu     sync.Mutex
	closed bool
	seq    atomic.Uint64

	spawns  atomic.Int64 // worker spawns incl. respawns — telemetry
	crashes atomic.Int64 // worker deaths detected by Submit
}

// procWorker wraps one worker subprocess and its gob pipes.
type procWorker struct {
	cmd   *exec.Cmd
	enc   *gob.Encoder
	dec   *gob.Decoder
	stdin io.Closer
}

// NewPool spawns cfg.Workers worker subprocesses and returns a ready
// Pool. If no worker can be spawned it returns an error and leaks
// nothing.
func NewPool(cfg Config) (*Pool, error) {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if len(cfg.Argv) == 0 {
		return nil, errors.New("crashpool: empty worker argv")
	}
	p := &Pool{cfg: cfg, free: make(chan *procWorker, cfg.Workers)}
	for i := 0; i < cfg.Workers; i++ {
		w, err := p.spawn()
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("crashpool: spawn worker: %w", err)
		}
		p.free <- w
	}
	return p, nil
}

// Workers returns the configured worker count.
func (p *Pool) Workers() int { return p.cfg.Workers }

// reqTimeout is the effective per-request deadline.
func (p *Pool) reqTimeout() time.Duration {
	if p.cfg.RequestTimeout > 0 {
		return p.cfg.RequestTimeout
	}
	return defaultRequestTimeout
}

// Stats returns cumulative telemetry: total worker spawns (initial +
// respawns) and total worker deaths detected.
func (p *Pool) Stats() (spawns, crashes int64) {
	return p.spawns.Load(), p.crashes.Load()
}

// spawn starts one worker subprocess. Its stderr is not inherited: a
// scanner goroutine routes it through the pool's Logger as structured,
// rate-limited Warn entries instead of raw text landing wherever the
// daemon's own stderr is wired to.
func (p *Pool) spawn() (*procWorker, error) {
	p.spawns.Add(1)
	cmd := exec.Command(p.cfg.Argv[0], p.cfg.Argv[1:]...) //nolint:gosec // argv is internal, not user-derived
	platform.ConfigureBackgroundCommand(cmd)
	if len(p.cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), p.cfg.Env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	procio.StderrWatcher{Logger: p.cfg.Logger, Tag: "crashpool worker"}.Watch(stderr)
	return &procWorker{
		cmd:   cmd,
		enc:   gob.NewEncoder(stdin),
		dec:   gob.NewDecoder(stdout),
		stdin: stdin,
	}, nil
}

// kill terminates the worker and reaps it so no zombie is left.
func (w *procWorker) kill() {
	if w == nil || w.cmd == nil {
		return
	}
	_ = w.stdin.Close()
	if w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
	_ = w.cmd.Wait()
}

// roundTrip sends one request and decodes the matching response. Any
// pipe error means the worker died.
func (w *procWorker) roundTrip(req *extractRequest, resp *extractResponse) error {
	if err := w.enc.Encode(req); err != nil {
		return err
	}
	if err := w.dec.Decode(resp); err != nil {
		return err
	}
	if resp.Seq != req.Seq {
		return fmt.Errorf("crashpool: response seq %d != request seq %d", resp.Seq, req.Seq)
	}
	return nil
}

// Submit extracts one file with empty request options. It preserves the base
// pool API for ordinary callers and tests.
func (p *Pool) Submit(relPath, language string, content []byte) Result {
	return p.SubmitWithOptions(relPath, language, content, nil)
}

// SubmitWithOptions extracts one file in a worker subprocess. It blocks until
// a worker is free, then runs the round-trip under requestTimeout. Temporal
// helper names are normalized into the request and never mutate worker state.
// A crashed or hung worker is killed, replaced, and reported via Result.Crashed;
// the pool stays at full strength so the caller can keep submitting.
func (p *Pool) SubmitWithOptions(relPath, language string, content []byte, temporalEnvHelpers []string) Result {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return Result{Err: "crashpool: pool is closed"}
	}

	w, ok := <-p.free
	if !ok {
		return Result{Err: "crashpool: pool is closed"}
	}

	req := extractRequest{
		Seq:                p.seq.Add(1),
		RelPath:            relPath,
		Language:           language,
		Content:            content,
		TemporalEnvHelpers: parser.NewExtractionOptions(temporalEnvHelpers).TemporalEnvHelpers(),
	}
	var resp extractResponse
	done := make(chan error, 1)
	go func() { done <- w.roundTrip(&req, &resp) }()

	timeout := p.reqTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-done:
		if err != nil {
			return p.replace(w, "parser worker crashed: "+err.Error())
		}
		p.free <- w
		if resp.Panicked {
			return Result{Panicked: true, Err: resp.Err}
		}
		return Result{
			Nodes:        resp.Nodes,
			Edges:        resp.Edges,
			ParseErrors:  resp.ParseErrors,
			ParseChecked: resp.Err == "" && resp.ParseChecked && resp.ParseErrors >= 0,
			HasParseErr:  resp.HasParseErr,
			Err:          resp.Err,
		}
	case <-timer.C:
		// Worker hung. Killing it unblocks the roundTrip goroutine
		// (its Decode errors out and drains into done).
		return p.replace(w, fmt.Sprintf("parser worker timed out after %s on %s", timeout, relPath))
	}
}

// replace kills a dead/hung worker, spawns a replacement, returns it to
// the free pool, and reports the crash. The free channel always keeps
// exactly Workers entries so Submit never deadlocks: on respawn failure
// the dead worker is returned and the spawn is retried on its next use.
func (p *Pool) replace(dead *procWorker, reason string) Result {
	p.crashes.Add(1)
	dead.kill()
	if p.cfg.Logger != nil {
		p.cfg.Logger.Warn("crashpool: worker died, respawning", zap.String("reason", reason))
	}

	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		// Don't respawn into a closed pool; the free slot is gone but
		// no further Submit will read it.
		return Result{Crashed: true, Err: reason}
	}

	fresh, err := p.spawn()
	if err != nil {
		if p.cfg.Logger != nil {
			p.cfg.Logger.Error("crashpool: respawn failed; reusing dead slot", zap.Error(err))
		}
		// Return the dead worker so the channel stays balanced; its
		// next roundTrip fails fast and triggers another respawn.
		p.free <- dead
		return Result{Crashed: true, Err: reason + " (respawn failed: " + err.Error() + ")"}
	}
	p.free <- fresh
	return Result{Crashed: true, Err: reason}
}

// Close terminates every worker subprocess. It is idempotent.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	// Drain every worker currently in the free channel. Workers
	// checked out by an in-flight Submit are returned to free after
	// the round-trip and reaped by the OS at process exit; in normal
	// use Close runs after all Submit callers have finished.
	for i := 0; i < p.cfg.Workers; i++ {
		select {
		case w := <-p.free:
			w.kill()
		default:
		}
	}
}
