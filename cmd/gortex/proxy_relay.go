package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/zzet/gortex/internal/daemon"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
)

const (
	proxyDaemonUnavailableCode = -32098
	proxySessionResetCode      = -32097
)

var proxyReinitializeTimeout = 5 * time.Second

type proxyFrameEvent struct {
	frame []byte
	err   error
}

type proxyPendingRequest struct {
	id                     json.RawMessage
	key                    string
	frame                  []byte
	idempotent             bool
	definitelyNotDelivered bool
	replayed               bool
}

type proxyRelayState struct {
	ctx       context.Context
	handshake daemon.Handshake
	stdout    io.Writer
	stderr    io.Writer
	surface   *gortexmcp.ToolSurface

	client         *daemon.Client
	responses      <-chan proxyFrameEvent
	pending        map[string]*proxyPendingRequest
	daemonInstance string
	initialize     []byte
	initialized    []byte
}

func relayProxySession(
	ctx context.Context,
	h daemon.Handshake,
	initial *daemon.Client,
	stdin io.Reader,
	stdout, stderr io.Writer,
	surface *gortexmcp.ToolSurface,
	orphan <-chan struct{},
) error {
	relayCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	state := &proxyRelayState{
		ctx:       relayCtx,
		handshake: h,
		stdout:    stdout,
		stderr:    stderr,
		surface:   surface,
		pending:   make(map[string]*proxyPendingRequest),
	}
	state.attach(initial)
	if initial != nil {
		state.daemonInstance = initial.Ack.DaemonInstance
	}
	defer state.closeClient()

	inbound := streamProxyFrames(relayCtx, stdin)
	for {
		select {
		case event, ok := <-inbound:
			if !ok {
				return nil
			}
			if event.err != nil {
				if errors.Is(event.err, io.EOF) {
					return nil
				}
				return event.err
			}
			if state.surface != nil && state.surface.Active() {
				if reply, gated := gateToolCallFrame(event.frame, state.surface); gated {
					if _, err := writeProxyFrame(state.stdout, reply); err != nil {
						return err
					}
					continue
				}
			}

			if state.client == nil {
				preserved, err := state.connect()
				if err != nil {
					if state.ctx.Err() != nil {
						return nil
					}
					if err := state.replyUnavailable(event.frame, err); err != nil {
						return err
					}
					continue
				}
				if !preserved {
					if err := state.replySessionReset(event.frame); err != nil {
						return err
					}
					continue
				}
			}

			state.noteProtocolFrame(event.frame)
			pending, hasID := pendingRequest(event.frame)
			if hasID {
				state.pending[pending.key] = pending
			}
			n, writeErr := writeProxyFrame(state.client.Conn, event.frame)
			if writeErr == nil {
				continue
			}
			if hasID && n == 0 {
				// No byte reached the old socket. Mutations are still never
				// replayed automatically, but the error may tell the host that an
				// explicit retry is safe.
				pending.definitelyNotDelivered = true
			}
			if err := state.recover(writeErr); err != nil {
				return err
			}

		case event, ok := <-state.responses:
			if !ok {
				if state.client != nil {
					if err := state.recover(io.EOF); err != nil {
						return err
					}
				}
				continue
			}
			if event.err != nil {
				if err := state.recover(event.err); err != nil {
					return err
				}
				continue
			}
			if key, ok := responseIDKey(event.frame); ok {
				delete(state.pending, key)
			}
			out := event.frame
			if state.surface != nil && state.surface.Active() {
				out = filterToolsListFrame(out, state.surface)
			}
			if _, err := writeProxyFrame(state.stdout, out); err != nil {
				return err
			}

		case <-orphan:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func (s *proxyRelayState) attach(client *daemon.Client) {
	s.client = client
	if client == nil || client.Conn == nil {
		s.responses = nil
		return
	}
	s.responses = streamProxyFrames(s.ctx, client.Conn)
}

func (s *proxyRelayState) connect() (bool, error) {
	client, _, err := dialDaemonWithRetry(s.ctx, s.handshake)
	if client == nil {
		if err == nil {
			err = daemon.ErrDaemonUnavailable
		}
		return false, err
	}

	previousInstance := s.daemonInstance
	preserved := previousInstance != "" &&
		client.Ack.DaemonInstance == previousInstance &&
		s.handshake.LogicalSessionID != "" &&
		client.Ack.SessionID == s.handshake.LogicalSessionID
	if !preserved {
		if err := s.restoreProtocolState(client); err != nil {
			_ = client.Close()
			return false, fmt.Errorf("restore MCP protocol state: %w", err)
		}
	}

	s.attach(client)
	s.daemonInstance = client.Ack.DaemonInstance
	logProxyConnection(s.stderr, client, true)
	if !preserved {
		if err := s.notifySessionReset(previousInstance, client.Ack.DaemonInstance); err != nil {
			s.closeClient()
			return false, err
		}
	}
	return preserved, nil
}

func (s *proxyRelayState) closeClient() {
	if s.client != nil {
		_ = s.client.Close()
	}
	s.client = nil
	s.responses = nil
}

func (s *proxyRelayState) recover(cause error) error {
	s.closeClient()
	if s.stderr != nil {
		fmt.Fprintf(s.stderr, "[gortex mcp] daemon connection lost (%v); keeping MCP stdio active\n", cause)
	}

	retry := make([]*proxyPendingRequest, 0, len(s.pending))
	for key, pending := range s.pending {
		if pending.idempotent && !pending.replayed {
			retry = append(retry, pending)
			continue
		}
		retryable := pending.idempotent || pending.definitelyNotDelivered
		deliveryUnknown := !pending.definitelyNotDelivered
		if err := s.writeUnavailable(pending.id, cause, retryable, deliveryUnknown); err != nil {
			return err
		}
		delete(s.pending, key)
	}
	if len(retry) == 0 {
		return nil
	}

	preserved, err := s.connect()
	if err != nil {
		for _, pending := range retry {
			if writeErr := s.writeUnavailable(pending.id, err, true, !pending.definitelyNotDelivered); writeErr != nil {
				return writeErr
			}
			delete(s.pending, pending.key)
		}
		return nil
	}
	if !preserved {
		for _, pending := range retry {
			if writeErr := s.writeSessionReset(pending.id); writeErr != nil {
				return writeErr
			}
			delete(s.pending, pending.key)
		}
		return nil
	}

	for _, pending := range retry {
		pending.replayed = true
		if _, err := writeProxyFrame(s.client.Conn, pending.frame); err != nil {
			s.closeClient()
			for _, failed := range retry {
				if writeErr := s.writeUnavailable(failed.id, err, true, true); writeErr != nil {
					return writeErr
				}
				delete(s.pending, failed.key)
			}
			return nil
		}
	}
	return nil
}

func (s *proxyRelayState) noteProtocolFrame(frame []byte) {
	var envelope struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(frame, &envelope) != nil {
		return
	}
	switch envelope.Method {
	case "initialize", "server/discover":
		s.initialize = append(s.initialize[:0], frame...)
	case "notifications/initialized":
		s.initialized = append(s.initialized[:0], frame...)
	}
}

func (s *proxyRelayState) restoreProtocolState(client *daemon.Client) (err error) {
	if len(s.initialize) == 0 {
		return nil
	}
	frame, responseKey, err := reconnectInitializeFrame(s.initialize)
	if err != nil {
		return err
	}
	if client.Conn != nil {
		if deadlineErr := client.Conn.SetDeadline(time.Now().Add(proxyReinitializeTimeout)); deadlineErr != nil {
			return fmt.Errorf("set protocol restore deadline: %w", deadlineErr)
		}
		defer func() {
			if clearErr := client.Conn.SetDeadline(time.Time{}); clearErr != nil && err == nil {
				err = fmt.Errorf("clear protocol restore deadline: %w", clearErr)
			}
		}()
	}
	if err := client.WriteMCPFrame(frame); err != nil {
		return err
	}
	var response []byte
	for {
		response, err = client.ReadMCPFrame()
		if err != nil {
			return err
		}
		gotKey, ok := responseIDKey(response)
		if ok && gotKey == responseKey {
			break
		}
		// MCP notifications and server-initiated requests may legally race the
		// initialize response. Preserve their ordering on the host transport
		// instead of treating the first unrelated frame as a reconnect failure.
		if _, writeErr := writeProxyFrame(s.stdout, response); writeErr != nil {
			return fmt.Errorf("forward protocol-restore frame: %w", writeErr)
		}
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(response, &envelope) != nil {
		return fmt.Errorf("invalid initialize response")
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return fmt.Errorf("initialize rejected: %s", envelope.Error)
	}
	initialized := bytes.TrimSpace(s.initialized)
	if len(initialized) == 0 {
		if peekFrameMethod(s.initialize) == "server/discover" {
			return nil
		}
		initialized = []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	}
	return client.WriteMCPFrame(initialized)
}

func reconnectInitializeFrame(frame []byte) ([]byte, string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return nil, "", err
	}
	id, err := json.Marshal("gortex-reconnect-initialize")
	if err != nil {
		return nil, "", err
	}
	envelope["id"] = id
	out, err := json.Marshal(envelope)
	if err != nil {
		return nil, "", err
	}
	key, ok := canonicalJSONRPCID(id)
	if !ok {
		return nil, "", fmt.Errorf("invalid reconnect initialize id")
	}
	return out, key, nil
}

func (s *proxyRelayState) notifySessionReset(previous, current string) error {
	message := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/message",
		"params": map[string]any{
			"level":  "warning",
			"logger": "gortex",
			"data": map[string]any{
				"code":                     "gortex_session_reset",
				"message":                  "Gortex daemon restarted; daemon-owned MCP session state was reset and protocol initialization was restored. Re-orient before continuing.",
				"previous_daemon_instance": previous,
				"daemon_instance":          current,
				"logical_session_id":       s.handshake.LogicalSessionID,
			},
		},
	}
	frame, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if _, err := writeProxyFrame(s.stdout, append(frame, '\n')); err != nil {
		return err
	}
	_, err = writeProxyFrame(s.stdout, []byte("{\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n"))
	return err
}

func (s *proxyRelayState) replySessionReset(frame []byte) error {
	pending, ok := pendingRequest(frame)
	if !ok {
		return nil
	}
	return s.writeSessionReset(pending.id)
}

func (s *proxyRelayState) writeSessionReset(id json.RawMessage) error {
	reply, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    proxySessionResetCode,
			"message": "Gortex daemon restarted and daemon-owned MCP session state was reset. The request was not executed; re-orient and retry.",
			"data": map[string]any{
				"retryable":       true,
				"session_reset":   true,
				"required_action": "reorient_then_retry",
			},
		},
	})
	if err != nil {
		return err
	}
	_, err = writeProxyFrame(s.stdout, append(reply, '\n'))
	return err
}

func (s *proxyRelayState) replyUnavailable(frame []byte, cause error) error {
	pending, ok := pendingRequest(frame)
	if !ok {
		// JSON-RPC notifications never receive responses. Dropping one while
		// disconnected is preferable to inventing protocol traffic or
		// replaying an operation whose delivery is unknown.
		if s.stderr != nil {
			fmt.Fprintf(s.stderr, "[gortex mcp] daemon unavailable; notification not forwarded: %v\n", cause)
		}
		return nil
	}
	return s.writeUnavailable(pending.id, cause, true, false)
}

func (s *proxyRelayState) writeUnavailable(id json.RawMessage, cause error, retryable, deliveryUnknown bool) error {
	message := "Gortex daemon is unavailable; the MCP transport remains active."
	if deliveryUnknown && !retryable {
		message += " Request delivery is unknown; do not retry automatically. Inspect mutation state first."
	} else if retryable {
		message += " Retry after the daemon recovers."
	}
	data := map[string]any{
		"retryable":        retryable,
		"delivery_unknown": deliveryUnknown,
	}
	if cause != nil {
		data["cause"] = cause.Error()
	}
	reply, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    proxyDaemonUnavailableCode,
			"message": message,
			"data":    data,
		},
	})
	if err != nil {
		return err
	}
	_, err = writeProxyFrame(s.stdout, append(reply, '\n'))
	return err
}

func pendingRequest(frame []byte) (*proxyPendingRequest, bool) {
	var envelope struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if json.Unmarshal(frame, &envelope) != nil || envelope.Method == "" || missingJSONRPCID(envelope.ID) {
		return nil, false
	}
	key, ok := canonicalJSONRPCID(envelope.ID)
	if !ok {
		return nil, false
	}
	idempotent := retryableProxyRequest(frame)
	return &proxyPendingRequest{
		id:         append(json.RawMessage(nil), envelope.ID...),
		key:        key,
		frame:      append([]byte(nil), frame...),
		idempotent: idempotent,
	}, true
}

func responseIDKey(frame []byte) (string, bool) {
	var envelope struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if json.Unmarshal(frame, &envelope) != nil || envelope.Method != "" || missingJSONRPCID(envelope.ID) {
		return "", false
	}
	return canonicalJSONRPCID(envelope.ID)
}

func missingJSONRPCID(id json.RawMessage) bool {
	// RawMessage is nil only when the object field was absent. Explicit null
	// has non-zero raw bytes and is a request ID that must be echoed verbatim.
	return len(id) == 0
}

func canonicalJSONRPCID(id json.RawMessage) (string, bool) {
	raw := bytes.TrimSpace(id)
	if len(raw) == 0 || !json.Valid(raw) {
		return "", false
	}
	if bytes.Equal(raw, []byte("null")) {
		return "null", true
	}
	if raw[0] == '"' {
		// Canonicalize string escapes while keeping string and numeric ID
		// namespaces disjoint ("1" must never correlate with 1).
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return "", false
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			return "", false
		}
		return "s:" + string(canonical), true
	}

	// Decode with UseNumber so integers above 2^53 retain every digit. Using
	// interface{} + json.Unmarshal here would round through float64 and could
	// collapse adjacent request IDs into one pending-map entry.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return "", false
	}
	number, ok := value.(json.Number)
	if !ok {
		return "", false
	}
	return "n:" + number.String(), true
}

func retryableProxyRequest(frame []byte) bool {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				Operation string `json:"operation"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(frame, &envelope) != nil {
		return false
	}
	// The daemon can deduplicate a replay only when it admitted the exact raw ID
	// to its bounded response cache. Keep proxy replay admission identical.
	if len(bytes.TrimSpace(envelope.ID)) > daemon.MCPResponseCacheMaxIDBytes {
		return false
	}
	switch envelope.Method {
	case "initialize", "server/discover", "ping", "tools/list", "resources/list", "resources/templates/list", "prompts/list":
		return true
	case "tools/call":
		switch envelope.Params.Name {
		case "capabilities", "explore", "search", "read", "relations", "trace", "analyze", "workspace", "recall":
			return true
		}
	}
	return false
}

func streamProxyFrames(ctx context.Context, src io.Reader) <-chan proxyFrameEvent {
	out := make(chan proxyFrameEvent, 16)
	go func() {
		defer close(out)
		reader := bufio.NewReaderSize(src, 1<<20)
		for {
			frame, err := reader.ReadBytes('\n')
			if len(frame) > 0 {
				select {
				case out <- proxyFrameEvent{frame: frame}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					select {
					case out <- proxyFrameEvent{err: err}:
					case <-ctx.Done():
					}
				} else if src != nil {
					select {
					case out <- proxyFrameEvent{err: io.EOF}:
					case <-ctx.Done():
					}
				}
				return
			}
		}
	}()
	return out
}

func writeProxyFrame(dst io.Writer, frame []byte) (int, error) {
	written := 0
	for len(frame) > 0 {
		n, err := dst.Write(frame)
		written += n
		frame = frame[n:]
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func logProxyConnection(stderr io.Writer, client *daemon.Client, reconnected bool) {
	if stderr == nil || client == nil {
		return
	}
	verb := "proxying"
	if reconnected {
		verb = "reconnected"
	}
	if client.Ack.Warming {
		fmt.Fprintf(stderr,
			"[gortex mcp] %s to daemon (session %s, daemon warming up — phase %q; graph still filling)\n",
			verb, client.Ack.SessionID, client.Ack.WarmupPhase)
		return
	}
	fmt.Fprintf(stderr, "[gortex mcp] %s to daemon (session %s, default_repo=%q)\n",
		verb, client.Ack.SessionID, client.Ack.DefaultRepo)
}
