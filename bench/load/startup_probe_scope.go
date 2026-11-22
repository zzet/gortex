package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

const (
	startupScopeDeadline = time.Second
	startupScopeLogLimit = 1 << 20
	startupScopeFrameMax = 256 << 10
)

type startupScopeSample struct {
	probeSample
	Completed     time.Time       `json:"completed"`
	Frame         json.RawMessage `json:"frame,omitempty"`
	FrameTooLarge bool            `json:"frame_too_large,omitempty"`
}

type startupScopeEvidence struct {
	Root              string               `json:"root"`
	Socket            string               `json:"socket"`
	PID               int                  `json:"pid"`
	ProcessBirthToken string               `json:"process_birth_token,omitempty"`
	Stderr            string               `json:"stderr"`
	Serving           json.RawMessage      `json:"serving,omitempty"`
	ServingAt         time.Time            `json:"serving_at,omitempty"`
	ServingError      string               `json:"serving_error,omitempty"`
	Core              []startupScopeSample `json:"core,omitempty"`
	Facade            []startupScopeSample `json:"facade,omitempty"`
	File              string               `json:"file"`
	Pattern           string               `json:"pattern"`
	EarlyGrep         *probeSample         `json:"early_grep,omitempty"`
	EarlyGrepHealth   []startupScopeSample `json:"early_grep_health,omitempty"`
	ReadyGrep         *probeSample         `json:"ready_grep,omitempty"`
}

// The companion is scoped to the primary and to the process started by the
// current boot. Its responses are separate from the stock probe metrics.
type startupScopeObserver struct {
	evidence startupScopeEvidence
	probe    *startupProbe
	cwd      probeCWD
	ctx      context.Context
	cancel   context.CancelFunc
	workers  sync.WaitGroup
}

func startStartupScopeObserver(ctx context.Context, p *privateRun, probe *startupProbe, started time.Time) *startupScopeObserver {
	ctx, cancel := context.WithCancel(ctx)
	primary := p.Repos[0]
	_, symbol, _ := strings.Cut(primary.Corpus.Symbol, "::")
	s := &startupScopeObserver{
		ctx: ctx, cancel: cancel, probe: probe, cwd: probeCWD{Role: "primary", Dir: primary.Path, File: primary.File},
		evidence: startupScopeEvidence{Root: primary.Path, Socket: p.Socket, PID: p.process.Process.Pid,
			Stderr: filepath.Base(p.stderr.Name()), File: primary.File, Pattern: regexp.QuoteMeta(symbol)},
	}
	if owner, err := platform.CaptureProcessIdentity(s.evidence.PID); err == nil {
		s.evidence.ProcessBirthToken, _ = owner.Token()
	}
	s.workers.Go(func() { s.observeServing(p.stderr, started) })
	return s
}

func (s *startupScopeObserver) close() {
	s.cancel()
	s.workers.Wait()
}

func (s *startupScopeObserver) observeServing(stderr *os.File, started time.Time) {
	raw, at, err := awaitStartupServing(s.ctx, stderr, s.evidence.Socket, started)
	if err != nil {
		s.evidence.ServingError = err.Error()
		return
	}
	s.evidence.Serving, s.evidence.ServingAt = raw, at
	ctx, cancel := context.WithDeadline(s.ctx, at.Add(startupScopeDeadline))
	defer cancel()
	legacy := []request{
		{Tool: "index_health"},
		{Tool: "tools_search", Arguments: map[string]any{"query": "index", "promote": false, "max_results": 1}},
		{Tool: "graph_stats"},
	}
	facade := []request{
		{Tool: "workspace", Operation: "index", Arguments: map[string]any{"operation": "index"}},
		{Tool: "capabilities"},
		{Tool: "workspace", Operation: "graph", Arguments: map[string]any{"operation": "graph"}},
	}
	var burst sync.WaitGroup
	burst.Go(func() { s.evidence.Core = s.capture(ctx, "core", "defer", "initialize", legacy) })
	burst.Go(func() { s.evidence.Facade = s.capture(ctx, "facade-v1", "hide", "initialize_facade", facade) })
	burst.Go(s.observeEarlyGrep)
	burst.Wait()
}

func (s *startupScopeObserver) capture(ctx context.Context, tools, mode, initialize string, steps []request) []startupScopeSample {
	var samples []startupScopeSample
	s.probe.mcpSession(ctx, s.cwd, tools, mode, initialize, steps, func(sample probeSample, frame []byte) bool {
		row := startupScopeSample{probeSample: sample, Completed: sample.completed()}
		if len(frame) <= startupScopeFrameMax {
			row.Frame = frame
		} else {
			row.FrameTooLarge = true
		}
		samples = append(samples, row)
		return ctx.Err() == nil
	})
	return samples
}

func (s *startupScopeObserver) observeReady() {
	s.workers.Go(func() { s.evidence.ReadyGrep = s.grep(s.ctx) })
}

func (s *startupScopeObserver) observeEarlyGrep() {
	s.evidence.EarlyGrep = s.grep(s.ctx)
	if s.evidence.EarlyGrep == nil || !s.evidence.EarlyGrep.OK {
		return
	}
	// The first ready poll is an upper bound. A separate matching loading
	// answer after Grep exits can establish that it actually ran while loading.
	ctx, cancel := context.WithTimeout(s.ctx, startupScopeDeadline)
	defer cancel()
	s.evidence.EarlyGrepHealth = s.capture(ctx, "core", "defer", "initialize", []request{{Tool: "index_health"}})
}

func (s *startupScopeObserver) grep(ctx context.Context) *probeSample {
	if s.evidence.Pattern == "" {
		return nil
	}
	row := s.probe.nativeHook(ctx, s.cwd, "Grep", "hook_grep", map[string]string{
		"path": s.evidence.File, "pattern": s.evidence.Pattern,
	})
	return &row
}

// p.start passes the captured stderr file directly to the foreground daemon.
// Open a separate cursor and require the same file and socket before using a
// log timestamp. A stale, missing or late marker supplies no timed success.
func awaitStartupServing(ctx context.Context, stderr *os.File, socket string, started time.Time) (json.RawMessage, time.Time, error) {
	log, err := os.Open(stderr.Name())
	if err != nil {
		return nil, time.Time{}, err
	}
	defer func() { _ = log.Close() }()
	want, err := stderr.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	actual, err := log.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	if !os.SameFile(want, actual) {
		return nil, time.Time{}, errors.New("startup stderr identity changed")
	}
	limit := &io.LimitedReader{R: log, N: startupScopeLogLimit}
	reader := bufio.NewReader(limit)
	var line []byte
	for {
		fragment, err := reader.ReadBytes('\n')
		line = append(line, fragment...)
		if err == nil {
			var row struct {
				Message   string  `json:"msg"`
				Socket    string  `json:"socket"`
				Timestamp float64 `json:"ts"`
			}
			if json.Unmarshal(line, &row) == nil && row.Message == "daemon: serving" && row.Socket == socket {
				at := time.Unix(0, int64(row.Timestamp*float64(time.Second)))
				if !at.Before(started) && !at.After(time.Now()) {
					return line, at, nil
				}
			}
			line = line[:0]
			continue
		}
		if !errors.Is(err, io.EOF) {
			return nil, time.Time{}, err
		}
		if limit.N == 0 {
			return nil, time.Time{}, errors.New("startup serving marker exceeded log limit")
		}
		if err := waitContext(ctx, 20*time.Millisecond); err != nil {
			return nil, time.Time{}, err
		}
	}
}
