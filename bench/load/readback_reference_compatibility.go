package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

const readbackReferenceCompatibilitySurface = "all/defer"
const readbackReferenceFacadeSurface = "facade-v1/hide"

// The matrix keeps its logical mutation adapter for original ACK decoding.
// Only this explicit surface selection changes actual transport parameters.
func readbackReferenceCompatibilityParams(req request, repo privateRepo) (map[string]any, error) {
	if repo.Prefix == "" {
		return nil, errors.New("compatibility request has no selected checkout prefix")
	}
	input := copyJSON(req.Arguments)
	args := map[string]any{}
	if options, ok := input["options"].(map[string]any); ok {
		for key, value := range options {
			args[key] = value
		}
	}
	scoped := func(value any, symbol bool) (string, error) {
		text, ok := value.(string)
		if !ok || text == "" || path.IsAbs(text) || path.Clean(text) != text || text == ".." || strings.HasPrefix(text, "../") {
			return "", errors.New("compatibility target is not one fixture-relative path or symbol")
		}
		if symbol && !strings.Contains(text, "::") {
			return "", errors.New("compatibility symbol target lacks its exact declaration")
		}
		return repo.Prefix + "/" + text, nil
	}
	target, _ := input["target"].(map[string]any)
	setTarget := func(field string, symbol bool) error {
		key := "file"
		if symbol {
			key = "symbol"
		}
		value, err := scoped(target[key], symbol)
		if err == nil {
			args[field] = value
		}
		return err
	}
	tool := ""
	var err error
	switch req.name() {
	case "edit.file":
		tool, err = "edit_file", setTarget("path", false)
		args["old_string"], args["new_string"] = input["match"], input["replacement"]
	case "edit.symbol":
		tool, err = "edit_symbol", setTarget("id", true)
		args["old_source"], args["new_source"] = input["match"], input["replacement"]
	case "edit.write":
		tool, err = "write_file", setTarget("path", false)
		args["content"] = input["content"]
	case "edit.batch":
		tool = "batch_edit"
		changes, ok := input["changes"].([]any)
		if !ok || len(changes) != 1 {
			return nil, errors.New("compatibility batch must be the declared one-file fixture edit")
		}
		item, ok := changes[0].(map[string]any)
		if !ok || item["op"] != "edit_file" {
			return nil, errors.New("unsupported compatibility batch operation")
		}
		itemPath, pathErr := scoped(item["path"], false)
		if pathErr != nil {
			return nil, pathErr
		}
		item["path"] = itemPath
		args["edits"] = []any{item}
	case "refactor.rename", "refactor.delete", "refactor.move":
		tool = map[string]string{"refactor.rename": "rename_symbol", "refactor.delete": "safe_delete_symbol", "refactor.move": "move_symbol"}[req.name()]
		err = setTarget("id", true)
		args["dry_run"] = input["dry_run"]
		if tool == "rename_symbol" {
			args["new_name"] = input["new_name"]
		}
		if tool == "safe_delete_symbol" {
			args["force"] = input["force"]
		}
		if tool == "move_symbol" {
			if err == nil {
				args["target_file"], err = scoped(input["destination"], false)
			}
		}
	case "read.source", "read.summary", "read.editing_context", "read.file":
		tool = map[string]string{"read.source": "get_symbol_source", "read.summary": "get_file_summary", "read.editing_context": "get_editing_context", "read.file": "read_file"}[req.name()]
		if tool == "get_symbol_source" {
			err = setTarget("id", true)
		} else {
			err = setTarget("path", false)
		}
		args["format"] = "json"
	case "get_symbol":
		if input["detail"] != "full" {
			return nil, errors.New("compatibility metadata probe must explicitly request full detail")
		}
		tool = "get_symbol"
		args["id"], err = scoped(input["id"], true)
		args["detail"], args["format"] = "full", "json"
	default:
		return nil, fmt.Errorf("no declared compatibility adapter for %q", req.name())
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"name": tool, "arguments": args}, nil
}

func readbackReferenceSurface(surface string) (string, error) {
	if surface == "" || surface == readbackReferenceFacadeSurface {
		return readbackReferenceFacadeSurface, nil
	}
	if surface == readbackReferenceCompatibilitySurface {
		return surface, nil
	}
	return "", errors.New("readback tool_surface must be facade-v1/hide or all/defer")
}

// One negotiated connection owns the whole cell. An unavailable tool closes
// that connection; this function never reconnects using another surface.
func openReadbackReferenceCompatibility(ctx context.Context, socket, cwd, mutationTool string) (*socketClient, map[string]json.RawMessage, error) {
	proof := map[string]json.RawMessage{}
	conn, reader, version, err := dialSocketSurface(ctx, socket, cwd, "mcp", "all", "defer")
	if err != nil {
		return nil, proof, err
	}
	c := &socketClient{conn: conn, reader: reader, pending: make(map[int64]chan wireReply), done: make(chan struct{}), version: version, cwd: cwd, writeGate: make(chan struct{}, 1)}
	go c.readLoop()
	refuse := func(err error) (*socketClient, map[string]json.RawMessage, error) {
		_ = c.Close()
		return nil, proof, err
	}
	if _, err := c.call(ctx, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "gortex-load", "version": "1"}}); err != nil {
		return refuse(err)
	}
	if err := c.send(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, false); err != nil {
		return refuse(err)
	}
	for _, tool := range []string{mutationTool, "get_symbol_source", "get_file_summary", "get_editing_context", "read_file", "get_symbol"} {
		raw, err := c.call(ctx, "tools/call", map[string]any{"name": "tool_profile", "arguments": map[string]any{"tool": tool}})
		proof[tool] = append(json.RawMessage(nil), raw...)
		if err != nil {
			return refuse(err)
		}
		body, isError, ok := readbackReferenceBody(raw)
		if !ok || isError || body["tool"] != tool || body["enabled"] != true || body["status"] != "live" && body["status"] != "deferred" {
			return refuse(fmt.Errorf("compatibility tool %q is absent, blocked or unobservable on this connection", tool))
		}
	}
	return c, proof, nil
}

func observeReadbackReferenceCompatibility(ctx context.Context, client readbackReferenceCaller, repo privateRepo, trial readbackReferenceTrial, role string, deadline time.Duration) readbackReferenceRecord {
	return observeReadbackReferenceSurface(ctx, client, repo, trial, role, deadline, true)
}

// The same legacy connection reaches the existing readiness consumers before
// the timed offers. No facade-only dispatcher or second warmup client is used.
func warmReadbackReferenceCompatibility(ctx context.Context, c *socketClient, deadline time.Duration) (map[string]json.RawMessage, error) {
	proof := map[string]json.RawMessage{}
	for {
		callCtx, cancel := context.WithTimeout(ctx, deadline)
		raw, err := c.call(callCtx, "tools/call", map[string]any{"name": "analyze", "arguments": map[string]any{"kind": "hotspots", "format": "json"}})
		cancel()
		if err != nil {
			return proof, err
		}
		body, isError, ok := readbackReferenceBody(raw)
		if !ok || isError {
			return proof, errors.New("legacy analysis readiness unavailable")
		}
		proof["analysis_consumer"] = append(json.RawMessage(nil), raw...)
		if body["status"] == "analysis_pending" {
			if err := waitContext(ctx, time.Second); err != nil {
				return proof, err
			}
			continue
		}
		if body["hotspots"] == nil || body["status"] != nil && body["status"] != "" {
			return proof, errors.New("legacy analysis has no current-snapshot answer")
		}
		resourceCtx, resourceCancel := context.WithTimeout(ctx, deadline)
		resource, err := c.call(resourceCtx, "resources/read", map[string]any{"uri": "gortex://communities"})
		resourceCancel()
		if err != nil {
			return proof, err
		}
		payload, err := textPayload(resource, "contents")
		if err != nil {
			return proof, err
		}
		var receipt struct {
			Returned    *int            `json:"returned"`
			Total       *int            `json:"total"`
			Communities json.RawMessage `json:"communities"`
		}
		if err := json.Unmarshal(payload, &receipt); err != nil {
			return proof, err
		}
		if receipt.Returned == nil || receipt.Total == nil || receipt.Communities == nil {
			return proof, errors.New("legacy analysis publication lacks its durable-generation receipt")
		}
		proof["analysis_publication"] = payload
		return proof, nil
	}
}
