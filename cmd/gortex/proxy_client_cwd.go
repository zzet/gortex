package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
)

// The stdio proxy sends its working directory once, in the daemon handshake.
// A host can change directory afterwards without restarting its MCP servers —
// Claude Code's EnterWorktree moves the host process into a linked worktree
// and leaves `gortex mcp` running where it was launched. The daemon then keeps
// resolving the session against the launch checkout, so a bare relative edit
// path lands in the main checkout while the agent works in the worktree.
//
// The proxy's own cwd never changes, but its parent's does: the host is the
// process that spawned it. On each tools/call the proxy reads the parent's
// current directory and stamps it as `params._meta.cwd`; the daemon records it
// as the session's live CWD (daemon.Session.SetLiveCWD).

// clientCWDProbe returns the MCP host's current working directory, or "" when
// it cannot be determined.
type clientCWDProbe func() string

// liveClientCWD runs probe and keeps only an absolute, informative directory —
// the same filter resolveLaunchCWD applies to the launch directory, so a host
// sitting in `/` or $HOME never displaces the handshake CWD.
func liveClientCWD(probe clientCWDProbe) string {
	if probe == nil {
		return ""
	}
	cwd := probe()
	if cwd == "" || !filepath.IsAbs(cwd) || isAmbiguousLaunchCWD(cwd) {
		return ""
	}
	return filepath.Clean(cwd)
}

// stampClientCWD returns frame with `params._meta.cwd` set to cwd when frame is
// a tools/call request. Every other frame, a frame whose params are not an
// object, and a frame whose client already supplied `_meta.cwd` are returned
// unchanged. The trailing newline framing is preserved.
func stampClientCWD(frame []byte, cwd string) []byte {
	if cwd == "" {
		return frame
	}
	body := bytes.TrimRight(frame, "\r\n")
	suffix := frame[len(body):]

	var msg map[string]json.RawMessage
	if json.Unmarshal(body, &msg) != nil {
		return frame
	}
	var method string
	if json.Unmarshal(msg["method"], &method) != nil || method != "tools/call" {
		return frame
	}
	params := map[string]json.RawMessage{}
	if raw, ok := msg["params"]; ok && json.Unmarshal(raw, &params) != nil {
		return frame
	}
	if params == nil {
		params = map[string]json.RawMessage{}
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := params["_meta"]; ok && json.Unmarshal(raw, &meta) != nil {
		return frame
	}
	if meta == nil {
		meta = map[string]json.RawMessage{}
	}
	if _, present := meta["cwd"]; present {
		return frame
	}

	encoded, err := json.Marshal(cwd)
	if err != nil {
		return frame
	}
	meta["cwd"] = encoded
	if params["_meta"], err = json.Marshal(meta); err != nil {
		return frame
	}
	if msg["params"], err = json.Marshal(params); err != nil {
		return frame
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return frame
	}
	return append(out, suffix...)
}
