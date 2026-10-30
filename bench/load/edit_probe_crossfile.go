package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func editProbeCrossFileCallee(c Corpus) (file, name string, err error) {
	sep := strings.LastIndex(c.EditCallee, "::")
	if sep < 1 {
		return "", "", errors.New("crossfile fixture requires exact edit_callee")
	}
	file, name = c.EditCallee[:sep], c.EditCallee[sep+2:]
	if filepath.Base(file) != file || file == c.File || filepath.Ext(file) != filepath.Ext(c.File) || !simpleSymbolName(name) {
		return "", "", errors.New("crossfile fixture requires distinct same-language root file and plain callee")
	}
	base := strings.TrimSuffix(file, filepath.Ext(file))
	if !simpleSymbolName(base) {
		return "", "", errors.New("crossfile fixture module basename must be a plain identifier")
	}
	return file, name, nil
}

func simpleSymbolName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func editProbeCrossFileImport(c Corpus) string {
	file, name, err := editProbeCrossFileCallee(c)
	if err != nil {
		return ""
	}
	module := strings.TrimSuffix(file, filepath.Ext(file))
	switch filepath.Ext(c.File) {
	case ".ts":
		return "import { " + name + " } from \"./" + module + "\";\n"
	case ".py":
		return "from " + module + " import " + name + "\n"
	}
	return ""
}
func editProbeCrossFilePlain(c Corpus, source string) string {
	if c.EditKind != "add_crossfile_call" {
		return source
	}
	prefix := editProbeCrossFileImport(c)
	return strings.TrimPrefix(source, prefix)
}

func (r *editProbeRuntime) admitCrossFileCallee(ctx context.Context) error {
	c := r.p.Repos[0].Corpus
	file, name, err := editProbeCrossFileCallee(c)
	if err != nil {
		return err
	}
	prefix := editProbeCrossFileImport(c)
	if prefix != "" && !strings.HasPrefix(string(r.original), prefix) {
		return errors.New("crossfile fixture missing exact explicit import")
	}
	content, err := os.ReadFile(filepath.Join(r.p.Repos[0].Path, file))
	if err != nil {
		return err
	}
	if !editProbeLeafSource(file, string(content), name) {
		return errors.New("crossfile fixture callee must be a real numeric leaf")
	}
	req := request{Tool: "read", Operation: "source", Arguments: map[string]any{"operation": "source", "target": map[string]any{"symbol": c.EditCallee}, "options": map[string]any{"context_lines": 0}}}
	raw, identity, err := r.editor.callWithIdentity(ctx, "tools/call", map[string]any{"name": req.Tool, "arguments": dispatchArgs(req, r.p.Repos[0])})
	if err != nil {
		return err
	}
	proof := c
	proof.File = file
	proof.Symbol = c.EditCallee
	proof.EditKind = ""
	if !editProbePresenceProof(raw, proof, c.EditCallee) {
		return errors.New("crossfile callee public source admission failed")
	}
	record, err := json.Marshal(map[string]any{"symbol": c.EditCallee, "wire_id": identity.ID, "write_started": identity.WriteStarted, "finished": identity.Finished, "payload": raw})
	if err != nil {
		return err
	}
	if r.phase.Gate == nil {
		r.phase.Gate = make(map[string]json.RawMessage)
	}
	r.phase.Gate["edit_probe_crossfile_callee_admission"] = record
	return nil
}
