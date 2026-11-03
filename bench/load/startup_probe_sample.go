package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	startupProbeInterval = time.Second
	startupCallTimeout   = 10 * time.Second
	// An interactive CLI call waits up to 30 s for a loading repository.
	startupCLITimeout = 45 * time.Second
	startupTextLimit  = 240
)

// probeCWD is one directory an agent could start in, and an indexed file the
// native-Read hook probe reads from there.
type probeCWD struct {
	Role string // primary, worktree or router
	Dir  string
	File string
}

// probeSample is one answer as the per-second log records it. Outcome fields
// come from the public wire only: JSON-RPC error data, tool results, CLI exit
// codes and output, and the hook's decision.
type probeSample struct {
	CWD         string    `json:"cwd"`
	Surface     string    `json:"surface"`
	Started     time.Time `json:"started"`
	LatencyMS   float64   `json:"latency_ms"`
	OK          bool      `json:"ok"`
	Code        string    `json:"error_code,omitempty"`
	Retryable   *bool     `json:"retryable,omitempty"`
	Phase       string    `json:"phase,omitempty"`
	Percent     *int      `json:"percent,omitempty"`
	Status      string    `json:"status,omitempty"`
	Variant     string    `json:"variant,omitempty"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	TrackAdvice bool      `json:"track_advice,omitempty"`
	Text        string    `json:"text,omitempty"`
}

func (s probeSample) completed() time.Time {
	return s.Started.Add(time.Duration(s.LatencyMS * float64(time.Millisecond)))
}

// startupOperations is the offered MCP probe plan. The same values build the
// frames, so the plan hash describes exactly what was sent.
func startupOperations(c Corpus) []request {
	return []request{
		{Tool: "initialize"},
		{Tool: "search_symbols", Arguments: map[string]any{"query": c.Query, "limit": 1}},
		{Tool: "resources/read", Operation: "index-health", Arguments: map[string]any{"uri": "gortex://index-health"}},
		{Tool: "index_health"},
		{Tool: "capabilities"},
		{Tool: "tools_search", Arguments: map[string]any{"query": "index"}},
		{Tool: "graph_stats"},
		{Tool: "workspace", Operation: "index", Arguments: map[string]any{"operation": "index"}},
		{Tool: "workspace", Operation: "graph", Arguments: map[string]any{"operation": "graph"}},
	}
}

type startupProbe struct {
	socket, binary, query string
	env                   []string
	cwds                  []probeCWD
	plan                  []request
	ttyStatus             string
	workers               sync.WaitGroup

	mu       sync.Mutex
	samples  []probeSample
	log      *os.File
	encoder  *json.Encoder
	logErr   error
	boot     int
	socketOn bool
	version  string
}

func newStartupProbe(p *privateRun, cwds []probeCWD, query, logPath string) (*startupProbe, error) {
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	s := &startupProbe{socket: p.Socket, binary: p.Binary, query: query, env: p.Env, cwds: cwds, plan: startupOperations(Corpus{Query: query}), log: log, encoder: json.NewEncoder(log), boot: -1}
	s.ttyStatus = "script(1) pty"
	if _, err := ttyCommand(context.Background(), "true", nil); err != nil {
		s.ttyStatus = "unavailable: " + err.Error()
	} else if _, err := exec.LookPath("script"); err != nil {
		s.ttyStatus = "unavailable: " + err.Error()
	}
	return s, nil
}

// start launches every sampler. They run until ctx ends, across boots.
func (s *startupProbe) start(ctx context.Context) {
	for _, cwd := range s.cwds {
		s.every(ctx, func(ctx context.Context) { s.mcpTick(ctx, cwd) })
		s.every(ctx, func(ctx context.Context) { s.recordLive(ctx, s.hookRead(ctx, cwd)) })
		s.cliLoop(ctx, cwd, false)
		if strings.HasPrefix(s.ttyStatus, "script") {
			s.cliLoop(ctx, cwd, true)
		}
	}
}

func (s *startupProbe) every(ctx context.Context, tick func(context.Context)) {
	s.workers.Go(func() {
		ticker := time.NewTicker(startupProbeInterval)
		defer ticker.Stop()
		for ctx.Err() == nil {
			tick(ctx)
			select {
			case <-ctx.Done():
			case <-ticker.C:
			}
		}
	})
}

// cliLoop runs one CLI variant every second until it first succeeds in the
// current boot. The pipe variant also runs before the socket exists; the pty
// variant starts once it does, because an interactive call waits through the
// load and its time to exit is the measurement.
func (s *startupProbe) cliLoop(ctx context.Context, cwd probeCWD, tty bool) {
	done := -1
	s.every(ctx, func(ctx context.Context) {
		boot, socketOn := s.bootState()
		if boot < 0 || boot == done || (tty && !socketOn) {
			return
		}
		sample := s.cliCall(ctx, cwd, tty)
		if s.recordLive(ctx, sample) && sample.OK {
			done = boot
		}
	})
}

func (s *startupProbe) beginBoot(index int) {
	s.mu.Lock()
	s.boot, s.socketOn = index, false
	s.mu.Unlock()
}

func (s *startupProbe) socketUp(index int) {
	s.mu.Lock()
	if s.boot == index {
		s.socketOn = true
	}
	s.mu.Unlock()
}

func (s *startupProbe) stopping() {
	s.mu.Lock()
	s.socketOn = false
	s.mu.Unlock()
}

func (s *startupProbe) bootState() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boot, s.socketOn
}

func (s *startupProbe) daemonVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// recordLive keeps a sample unless the probe was stopping when it ended: a
// call cut short by the end of the run is not an answer.
func (s *startupProbe) recordLive(ctx context.Context, sample probeSample) bool {
	if ctx.Err() != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, sample)
	if s.logErr == nil {
		s.logErr = s.encoder.Encode(sample)
	}
	return true
}

func (s *startupProbe) finish() ([]probeSample, error) {
	s.workers.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.samples, errors.Join(s.logErr, s.log.Close())
}

// mcpTick opens fresh sessions from cwd, as a newly started agent would: the
// legacy surface for search_symbols and the index-health resource, and the
// compact surface for workspace(operation:"index"). Each session records the
// instructions variant its initialize returned.
func (s *startupProbe) mcpTick(ctx context.Context, cwd probeCWD) {
	var legacy, facade []request
	for _, step := range s.plan[1:] {
		if step.Tool == "workspace" {
			facade = append(facade, step)
		} else {
			legacy = append(legacy, step)
			// These discovery tools retain their public names on the facade
			// surface. There is no workspace capability/search operation.
			if step.Tool == "capabilities" || step.Tool == "tools_search" {
				facade = append(facade, step)
			}
		}
	}
	sessions := []struct {
		tools, mode, initialize string
		steps                   []request
	}{
		{"core", "defer", "initialize", legacy},
		{"facade-v1", "hide", "initialize_facade", facade},
	}
	for _, session := range sessions {
		if !s.mcpSession(ctx, cwd, session.tools, session.mode, session.initialize, session.steps) {
			return
		}
	}
}

func (s *startupProbe) mcpSession(ctx context.Context, cwd probeCWD, tools, mode, initialize string, steps []request) bool {
	started := time.Now()
	dialCtx, cancel := context.WithTimeout(ctx, startupCallTimeout)
	conn, reader, version, err := dialSocketSurface(dialCtx, s.socket, cwd.Dir, "mcp", tools, mode)
	cancel()
	if err != nil {
		code := "handshake_error"
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			code = "no_socket"
		}
		s.recordLive(ctx, probeSample{CWD: cwd.Role, Surface: "connect", Started: started, LatencyMS: elapsedMS(started), Code: code, Text: clip(err.Error())})
		return false
	}
	defer conn.Close()
	s.mu.Lock()
	s.version = version
	s.mu.Unlock()
	c := &probeConn{conn: conn, reader: reader}
	initParams := map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "gortex-load", "version": "1"}}
	calls := []probeCall{{initialize, "initialize", initParams}}
	for _, step := range steps {
		switch {
		case step.Tool == "resources/read":
			calls = append(calls, probeCall{"index_health_resource", step.Tool, step.Arguments})
		case step.Operation != "":
			calls = append(calls, probeCall{step.Tool + "_" + step.Operation, "tools/call", map[string]any{"name": step.Tool, "arguments": step.Arguments}})
		default:
			calls = append(calls, probeCall{step.Tool, "tools/call", map[string]any{"name": step.Tool, "arguments": step.Arguments}})
		}
	}
	for i, call := range calls {
		if tools == "facade-v1" && (call.surface == "capabilities" || call.surface == "tools_search") {
			call.surface += "_facade"
		}
		start := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, startupCallTimeout)
		frame, err := c.roundTrip(callCtx, call.method, call.params)
		timedOut := callCtx.Err() == context.DeadlineExceeded
		cancel()
		var sample probeSample
		switch {
		case err == nil:
			sample = decodeProbeFrame(frame)
		case timedOut:
			sample = probeSample{Code: "timeout", Text: clip(err.Error())}
		default:
			sample = probeSample{Code: "transport_error", Text: clip(err.Error())}
		}
		sample.CWD, sample.Surface, sample.Started, sample.LatencyMS = cwd.Role, call.surface, start, elapsedMS(start)
		if !s.recordLive(ctx, sample) || err != nil {
			return ctx.Err() == nil
		}
		if i == 0 && c.notify("notifications/initialized") != nil {
			return true
		}
	}
	return true
}

type probeCall struct {
	surface, method string
	params          any
}

type probeConn struct {
	conn         net.Conn
	reader       *bufio.Reader
	next         int
	notification func([]byte)
}

// roundTrip sends one request and returns its response frame, skipping
// notifications and server-initiated requests.
func (c *probeConn) roundTrip(ctx context.Context, method string, params any) ([]byte, error) {
	c.next++
	id := strconv.Itoa(c.next)
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := c.conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Now()) })
	defer stop()
	if _, err := c.conn.Write(append(data, '\n')); err != nil {
		return nil, err
	}
	for {
		line, err := readWireLine(c.reader)
		if err != nil {
			return nil, err
		}
		var peek struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(line, &peek); err != nil {
			return nil, fmt.Errorf("invalid daemon JSON: %w", err)
		}
		if peek.Method != "" && c.notification != nil {
			c.notification(line)
		}
		if peek.Method == "" && string(peek.ID) == id {
			return line, nil
		}
	}
}

func (c *probeConn) notify(method string) error {
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if err == nil {
		_, err = c.conn.Write(append(data, '\n'))
	}
	return err
}

// admissionFields are the machine-readable fields of a refusal's JSON-RPC
// error data, or of a tool's JSON payload.
type admissionFields struct {
	ErrorCode string   `json:"error_code"`
	Retryable *bool    `json:"retryable"`
	Phase     string   `json:"phase"`
	Percent   *float64 `json:"percent"`
	Status    string   `json:"status"`
}

func (f admissionFields) apply(s *probeSample) {
	if f.ErrorCode != "" {
		s.Code = f.ErrorCode
	}
	s.Retryable, s.Phase, s.Status = f.Retryable, f.Phase, f.Status
	if f.Percent != nil {
		percent := int(*f.Percent)
		s.Percent = &percent
	}
}

// decodeProbeFrame classifies one JSON-RPC response frame.
func decodeProbeFrame(frame []byte) probeSample {
	var reply struct {
		Error *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
		Result *struct {
			Instructions *string `json:"instructions"`
			IsError      bool    `json:"isError"`
			Content      []struct {
				Text string `json:"text"`
			} `json:"content"`
			Contents []struct {
				Text string `json:"text"`
			} `json:"contents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frame, &reply); err != nil || (reply.Error == nil && reply.Result == nil) {
		return probeSample{Code: "invalid_response", Text: clip(string(frame))}
	}
	// Successful graph answers can quote indexed text that mentions the
	// commands, so only refusals and instructions are checked for advice.
	var sample probeSample
	if reply.Error != nil {
		sample.TrackAdvice = adviceIn(string(frame))
		sample.Code = fmt.Sprintf("rpc_%d", reply.Error.Code)
		var fields admissionFields
		_ = json.Unmarshal(reply.Error.Data, &fields)
		fields.apply(&sample)
		sample.Text = clip(reply.Error.Message)
		return sample
	}
	result := reply.Result
	if result.Instructions != nil {
		sample.OK, sample.Variant, sample.TrackAdvice = true, instructionsVariant(*result.Instructions), adviceIn(*result.Instructions)
		if sample.Variant != "active" {
			sample.Text = clip(*result.Instructions)
		}
		return sample
	}
	text := ""
	for _, block := range append(result.Content, result.Contents...) {
		if text = block.Text; text != "" {
			break
		}
	}
	var fields admissionFields
	_ = json.Unmarshal([]byte(text), &fields)
	fields.apply(&sample)
	sample.OK = !result.IsError && fields.ErrorCode == ""
	if !sample.OK {
		if sample.Code == "" {
			sample.Code = "tool_error"
		}
		sample.Text, sample.TrackAdvice = clip(text), adviceIn(string(frame))
	}
	return sample
}

// instructionsVariant names the initialize instructions an agent received.
// Anything without one of the inactive or loading preambles is active.
func instructionsVariant(text string) string {
	for _, variant := range []struct{ prefix, name string }{
		{"Gortex is LOADING graph admission", "admission_pending"},
		{"Gortex is LOADING ", "loading"},
		{"Gortex is connected but could not load", "unavailable"},
		{"Gortex is connected but INACTIVE", "untracked"},
		{"Gortex is connected; checkout discovery is pending", "checkout_pending"},
		{"Gortex checkout discovery is unavailable", "checkout_admission_error"},
	} {
		if strings.HasPrefix(text, variant.prefix) {
			return variant.name
		}
	}
	if strings.TrimSpace(text) == "" {
		return "empty"
	}
	return "active"
}

// trackAdvicePattern finds advice to run `gortex track` or `gortex repos
// reconcile`. A negated mention ("Do not run `gortex track`") is guidance
// against it and is captured in the first group so it can be ignored.
var trackAdvicePattern = regexp.MustCompile("(?i)(\\bnot\\s+(?:run\\s+)?|n't\\s+(?:run\\s+)?|\\bnever\\s+(?:run\\s+)?)?`?gortex\\s+(?:track|repos\\s+reconcile)\\b")

func adviceIn(text string) bool {
	for _, match := range trackAdvicePattern.FindAllStringSubmatch(text, -1) {
		if match[1] == "" {
			return true
		}
	}
	return false
}

// cliCall runs `gortex call search_symbols` from cwd. Without a pty the CLI
// must fail fast with exit code 75 while the repository loads; with one it
// waits for the load, so its time to exit includes that wait.
func (s *startupProbe) cliCall(ctx context.Context, cwd probeCWD, tty bool) probeSample {
	surface := "cli_pipe"
	if tty {
		surface = "cli_tty"
	}
	args, _ := json.Marshal(map[string]any{"query": s.query, "limit": 1})
	callCtx, cancel := context.WithTimeout(ctx, startupCLITimeout)
	defer cancel()
	argv := []string{"call", "search_symbols", "--json", string(args)}
	var cmd *exec.Cmd
	var err error
	if tty {
		cmd, err = ttyCommand(callCtx, s.binary, argv)
	} else {
		cmd = exec.CommandContext(callCtx, s.binary, argv...)
	}
	started := time.Now()
	if err != nil {
		return probeSample{CWD: cwd.Role, Surface: surface, Started: started, Code: "unavailable", Text: clip(err.Error())}
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	exitCode, err := runOwned(cmd, cwd.Dir, s.env)
	sample := probeSample{CWD: cwd.Role, Surface: surface, Started: started, LatencyMS: elapsedMS(started), ExitCode: &exitCode}
	text := output.String()
	sample.OK, sample.Code = classifyCLI(exitCode, text)
	if callCtx.Err() == context.DeadlineExceeded {
		sample.OK, sample.Code = false, "timeout"
	} else if err != nil && exitCode < 0 {
		sample.Code = "exec_error"
		text += err.Error()
	}
	if !sample.OK {
		sample.Text, sample.TrackAdvice = clip(text), adviceIn(text)
	}
	return sample
}

func classifyCLI(exitCode int, text string) (bool, string) {
	switch {
	case exitCode == 0:
		return true, ""
	case exitCode == 75:
		return false, "repo_loading"
	case strings.Contains(text, "does not track"):
		return false, "repo_not_tracked"
	case strings.Contains(text, "no gortex daemon is running"):
		return false, "no_daemon"
	default:
		return false, fmt.Sprintf("exit_%d", exitCode)
	}
}

// ttyCommand runs argv under script(1) so the CLI's stdout is a terminal.
func ttyCommand(ctx context.Context, binary string, argv []string) (*exec.Cmd, error) {
	switch runtime.GOOS {
	case "darwin", "freebsd":
		return exec.CommandContext(ctx, "script", append([]string{"-q", "/dev/null", binary}, argv...)...), nil
	case "linux":
		quoted := make([]string, 0, len(argv)+1)
		for _, arg := range append([]string{binary}, argv...) {
			quoted = append(quoted, "'"+strings.ReplaceAll(arg, "'", `'\''`)+"'")
		}
		return exec.CommandContext(ctx, "script", "-q", "-e", "-c", strings.Join(quoted, " "), "/dev/null"), nil
	default:
		return nil, fmt.Errorf("no pty runner on %s", runtime.GOOS)
	}
}

// hookRead asks the PreToolUse hook about a native Read of an indexed file.
func (s *startupProbe) hookRead(ctx context.Context, cwd probeCWD) probeSample {
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": "gortex-load-startup", "tool_name": "Read", "tool_input": map[string]any{"file_path": cwd.File}, "cwd": cwd.Dir})
	callCtx, cancel := context.WithTimeout(ctx, startupCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(callCtx, s.binary, "hook")
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	started := time.Now()
	exitCode, err := runOwned(cmd, cwd.Dir, s.env)
	sample := probeSample{CWD: cwd.Role, Surface: "hook_read", Started: started, LatencyMS: elapsedMS(started), ExitCode: &exitCode}
	decision, text := decodeHookOutput(stdout.Bytes())
	sample.Variant, sample.TrackAdvice, sample.Text = decision, adviceIn(text), clip(text)
	sample.OK = err == nil
	if err != nil {
		sample.Variant, sample.Code = "", "hook_error"
		if callCtx.Err() == context.DeadlineExceeded {
			sample.Code = "timeout"
		}
		sample.Text = clip(err.Error() + " " + stderr.String())
	}
	return sample
}

// decodeHookOutput reads the default hook protocol's PreToolUse reply. No
// output, or context without a decision, lets the native tool run.
func decodeHookOutput(output []byte) (decision, text string) {
	output = bytes.TrimSpace(output)
	if len(output) == 0 {
		return "allow", ""
	}
	var reply struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
		Specific *struct {
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
			Context  string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal(output, &reply) != nil {
		return "unparsed", string(output)
	}
	decision, text = "allow", reply.Reason
	if reply.Specific != nil {
		if reply.Specific.Decision != "" {
			decision = reply.Specific.Decision
		}
		text = strings.TrimSpace(text + " " + reply.Specific.Reason + " " + reply.Specific.Context)
	}
	if reply.Decision == "block" {
		decision = "deny"
	}
	return decision, text
}

// runOwned runs a probe command in its own process group, so a call cut
// short by its deadline cannot leave children behind.
func runOwned(cmd *exec.Cmd, dir string, env []string) (int, error) {
	configureOwnedProcess(cmd)
	cmd.Dir, cmd.Env, cmd.WaitDelay = dir, env, 5*time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return killOwnedProcess(cmd)
	}
	err := cmd.Run()
	if err != nil && cmd.Process != nil {
		_ = cleanupExitedOwnedProcess(cmd)
	}
	if cmd.ProcessState == nil {
		return -1, err
	}
	return cmd.ProcessState.ExitCode(), err
}

func elapsedMS(start time.Time) float64 { return durationMS(time.Since(start)) }

func clip(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= startupTextLimit {
		return text
	}
	return text[:startupTextLimit] + "..."
}
