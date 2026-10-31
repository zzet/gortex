package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

const maxWireBytes = 64 << 20

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("RPC %d: %s", e.Code, e.Message) }

type wireReply struct {
	ID     json.Number     `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	err    error
}

type notificationObservation struct {
	Received time.Time       `json:"received"`
	Payload  json.RawMessage `json:"payload"`
}

type socketClient struct {
	conn          net.Conn
	reader        *bufio.Reader
	writeGate     chan struct{}
	mu            sync.Mutex
	next          int64
	pending       map[int64]chan wireReply
	closed        error
	done          chan struct{}
	notifies      []notificationObservation
	notifyDropped int
	version       string
}

func readWireLine(reader *bufio.Reader) ([]byte, error) {
	var data []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(data)+len(part) > maxWireBytes {
			return nil, errors.New("daemon response exceeds 64 MiB")
		}
		data = append(data, part...)
		if err != bufio.ErrBufferFull {
			return data, err
		}
	}
}

func dialSocket(ctx context.Context, socket, cwd, mode string) (net.Conn, *bufio.Reader, string, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, nil, "", err
	}
	deadline := time.Now().Add(10 * time.Second)
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, nil, "", err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	handshake := map[string]any{"version": 1, "mode": mode, "cwd": cwd, "client": "gortex-load", "tools": "facade-v1", "tools_mode": "hide"}
	if err := json.NewEncoder(conn).Encode(handshake); err != nil {
		_ = conn.Close()
		return nil, nil, "", err
	}
	reader := bufio.NewReader(conn)
	line, err := readWireLine(reader)
	var ack struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error_msg"`
		Version string `json:"daemon_version"`
	}
	if err == nil {
		err = json.Unmarshal(line, &ack)
	}
	if err == nil && !ack.OK {
		err = fmt.Errorf("handshake refused: %s", ack.Error)
	}
	if err == nil {
		err = conn.SetDeadline(time.Time{})
	}
	if err != nil {
		_ = conn.Close()
		return nil, nil, "", err
	}
	return conn, reader, ack.Version, nil
}

func openMCP(ctx context.Context, socket, cwd string) (*socketClient, error) {
	conn, reader, version, err := dialSocket(ctx, socket, cwd, "mcp")
	if err != nil {
		return nil, err
	}
	c := &socketClient{conn: conn, reader: reader, pending: make(map[int64]chan wireReply), done: make(chan struct{}), version: version, writeGate: make(chan struct{}, 1)}
	go c.readLoop()
	if _, err := c.call(ctx, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "gortex-load", "version": "1"}}); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, false); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func (c *socketClient) readLoop() {
	defer close(c.done)
	for {
		line, err := readWireLine(c.reader)
		if err != nil {
			c.fail(err)
			return
		}
		var reply wireReply
		if err := json.Unmarshal(line, &reply); err != nil {
			c.fail(fmt.Errorf("invalid daemon JSON: %w", err))
			return
		}
		if reply.Method != "" {
			c.mu.Lock()
			// Keep invalidation/publication evidence without retaining unbounded
			// diagnostic notifications throughout a saturation run.
			if len(c.notifies) < 1000 {
				c.notifies = append(c.notifies, notificationObservation{Received: time.Now(), Payload: append(json.RawMessage(nil), line...)})
			} else {
				c.notifyDropped++
			}
			c.mu.Unlock()
			continue
		}
		id, err := strconv.ParseInt(string(reply.ID), 10, 64)
		if err != nil {
			c.fail(errors.New("daemon returned non-integer response ID"))
			return
		}
		if reply.Result == nil && reply.Error == nil {
			c.fail(errors.New("daemon response has neither result nor error"))
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch != nil {
			ch <- reply
		}
	}
}

func (c *socketClient) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed != nil {
		return
	}
	c.closed = err
	for id, ch := range c.pending {
		ch <- wireReply{err: err}
		delete(c.pending, id)
	}
}

func (c *socketClient) Close() error {
	err := c.conn.Close()
	<-c.done
	return err
}

func (c *socketClient) send(ctx context.Context, value any, advisory bool) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if advisory {
		select {
		case c.writeGate <- struct{}{}:
		default:
			return nil // cancellation is advisory; never queue behind a writer
		}
	} else {
		select {
		case c.writeGate <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() { <-c.writeGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		c.fail(err)
		_ = c.conn.Close()
		return err
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		// A canceled partial frame cannot be reused by another request.
		c.fail(ctx.Err())
		_ = c.conn.Close()
		close(interrupted)
	})
	_, err = io.Copy(c.conn, bytes.NewReader(append(data, '\n')))
	if !stop() {
		<-interrupted
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		c.fail(err)
		_ = c.conn.Close()
	}
	return err
}

func (c *socketClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed != nil {
		err := c.closed
		c.mu.Unlock()
		return nil, err
	}
	c.next++
	id := c.next
	ch := make(chan wireReply, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}, false); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		// This is advisory; closing a shared connection here would censor
		// unrelated burst calls and distort their latency/error samples.
		cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		_ = c.send(cancelCtx, map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id, "reason": ctx.Err().Error()}}, true)
		cancel()
		return nil, ctx.Err()
	case reply := <-ch:
		if ctx.Err() != nil {
			return reply.Result, ctx.Err()
		}
		if reply.err != nil {
			return nil, reply.err
		}
		if reply.Error != nil {
			return reply.Result, reply.Error
		}
		return reply.Result, nil
	}
}

func control(ctx context.Context, socket, kind string, params any) (json.RawMessage, error) {
	conn, reader, _, err := dialSocket(ctx, socket, "", "control")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	if err := json.NewEncoder(conn).Encode(map[string]any{"kind": kind, "params": params}); err != nil {
		return nil, err
	}
	line, err := readWireLine(reader)
	if err != nil {
		return nil, err
	}
	var response struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"error_msg"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return nil, err
	}
	if !response.OK {
		return response.Result, fmt.Errorf("control %s: %s", kind, response.Error)
	}
	return response.Result, nil
}

// textPayload handles the public MCP content envelope, never daemon internals.
func textPayload(result json.RawMessage, field string) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(result, &envelope); err != nil {
		return nil, err
	}
	var rows []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(envelope[field], &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if json.Valid([]byte(row.Text)) {
			return json.RawMessage(row.Text), nil
		}
	}
	return nil, io.ErrUnexpectedEOF
}
