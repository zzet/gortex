package main

import (
	"bytes"
	"errors"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func editProbeTool(c Corpus) (string, error) {
	tool := c.EditTool
	if tool == "" {
		tool = "edit.file"
	}
	switch tool {
	case "edit.file":
		if c.EditKind == "rename" || c.EditKind == "delete" || c.EditKind == "modify_body" || c.EditKind == "modify_caller_body" {
			if err := editProbeInitialFixture(c); err != nil {
				return "", err
			}
		} else if c.EditInitialContent != "" {
			return "", errors.New("edit_initial_content requires explicit edit.write, edit.batch, edit.symbol or rename/delete fixture")
		}
	case "edit.write", "edit.batch", "edit.symbol":
		if err := editProbeInitialFixture(c); err != nil {
			return "", err
		}
	default:
		return "", errors.New("edit-and-probe supports edit.file, explicit edit.write, edit.symbol or one-file edit.batch")
	}
	if tool == "edit.symbol" {
		if err := editProbeSymbolAnchor(c); err != nil {
			return "", err
		}
	}
	return tool, nil
}

func editProbeInitialFixture(c Corpus) error {
	if c.EditInitialContent == "" || !utf8.ValidString(c.EditInitialContent) || c.EditAnchor == "" || strings.Count(c.EditInitialContent, c.EditAnchor) != 1 {
		return errors.New("explicit edit fixture requires UTF-8 initial content with one stable anchor")
	}
	switch filepath.Ext(c.File) {
	case ".go":
		if _, err := parser.ParseFile(token.NewFileSet(), "fixture.go", c.EditInitialContent, 0); err != nil {
			return errors.New("initial Go content must parse")
		}
	case ".ts":
		if valid, _ := editProbeTypeScriptFixture(c.EditInitialContent, "", ""); !valid {
			return errors.New("initial TypeScript content must match explicit fixture")
		}
	case ".py":
		if valid, _ := editProbePythonFixture(c.EditInitialContent, "", ""); !valid {
			return errors.New("initial Python content must match explicit fixture")
		}
	default:
		return errors.New("unsupported edit fixture language")
	}
	return nil
}

// Content is immutable offered client intent. Each request contains the original
// bytes plus the offered prefix of additions, not a reread of current disk state.
// Concurrent/out-of-order writes may therefore overwrite later intent; they are
// never serialized, regenerated or silently retried with different content.
func editProbeWriteRequest(c Corpus, content, marker string) (request, string) {
	content = strings.Replace(content, c.EditAnchor, c.EditAnchor+strings.ReplaceAll(c.EditTemplate, "{{marker}}", marker), 1)
	return request{Tool: "edit", Operation: "write", Arguments: map[string]any{"operation": "write", "target": map[string]any{"file": c.File}, "content": content}}, content
}

// Check the immutable plan against the actual pinned clone before opening any
// per-edit probe sessions or dispatching edits. This does not refresh intent.
func editProbeWriteAdmission(c Corpus, original []byte) error {
	if (c.EditTool == "edit.write" || c.EditTool == "edit.batch" || c.EditTool == "edit.symbol" || c.EditKind == "rename" || c.EditKind == "delete" || c.EditKind == "modify_body" || c.EditKind == "modify_caller_body") && !bytes.Equal(original, []byte(c.EditInitialContent)) {
		return errors.New("initial content differs from the pinned edit fixture bytes")
	}
	return nil
}
