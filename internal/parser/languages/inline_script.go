package languages

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

var (
	markupScriptRe = regexp.MustCompile(`(?is)<script\b((?:[^>"']|"[^"]*"|'[^']*')*)>(.*?)</script>`)
	markupAttrRe   = regexp.MustCompile(`([^\s=/>]+)(?:\s*=\s*("[^"]*"|'[^']*'|[^\s>]+))?`)
)

func markupScriptAttributes(attrs []byte) (ts, setup bool) {
	for _, attr := range markupAttrRe.FindAllSubmatch(attrs, -1) {
		switch strings.ToLower(string(attr[1])) {
		case "setup":
			setup = true
		case "lang":
			switch strings.ToLower(strings.Trim(string(attr[2]), `"'`)) {
			case "ts", "tsx", "typescript":
				ts = true
			}
		}
	}
	return ts, setup
}

// carveAndDelegateScripts runs every <script>/<script setup> block in src through
// the TypeScript or JavaScript extractor (per its lang attribute) and merges the
// result rebased into host-file coordinates. It is the shared carving loop behind
// the Svelte and Astro extractors.
func carveAndDelegateScripts(src []byte, filePath, fileID, language string, ts, js parser.Extractor, result *parser.ExtractionResult) {
	for _, m := range markupScriptRe.FindAllSubmatchIndex(src, -1) {
		contentStart, contentEnd := m[4], m[5]
		if contentStart < 0 {
			continue
		}
		delegate := js
		if isTS, _ := markupScriptAttributes(src[m[2]:m[3]]); isTS {
			delegate = ts
		}
		lineOffset := strings.Count(string(src[:contentStart]), "\n")
		delegateInlineScriptSlice(delegate, src[contentStart:contentEnd], lineOffset, filePath, fileID, language, result)
	}
}

// delegateInlineScriptSlice runs delegate over a carved inline-script slice and
// merges its symbols/edges into result, rebased into the host file's coordinate
// space. content is the raw script body; lineOffset is the 0-based line in the
// host file where the slice begins. Every delegated node/edge line is shifted by
// lineOffset, the synthetic file node is dropped, file-defines edges are
// repointed at fileID, nodes are relabeled to the host filePath (and language,
// when non-empty), and Meta["inline_script"]=true is stamped so downstream
// passes can tell a carved symbol from a natively-parsed one.
//
// It is the shared spine behind every markup extractor that embeds another
// language (HTML/Vue/Svelte/Astro <script>, Razor @code), so the error-prone
// offset-rebase math lives — and is tested — in exactly one place.
func delegateInlineScriptSlice(delegate parser.Extractor, content []byte, lineOffset int, filePath, fileID, language string, result *parser.ExtractionResult) *graph.Node {
	if delegate == nil || result == nil || strings.TrimSpace(string(content)) == "" {
		return nil
	}
	virtual := filePath + "#script:" + strconv.Itoa(lineOffset+1)
	sub, err := delegate.Extract(virtual, content)
	if err != nil || sub == nil {
		return nil
	}
	var file *graph.Node
	for _, n := range sub.Nodes {
		if n == nil {
			continue
		}
		if n.ID == virtual {
			file = n
			continue
		}
		n.FilePath = filePath
		if language != "" {
			n.Language = language
		}
		if n.StartLine > 0 {
			n.StartLine += lineOffset
		}
		if n.EndLine > 0 {
			n.EndLine += lineOffset
		}
		if n.Meta == nil {
			n.Meta = map[string]any{}
		}
		n.Meta["inline_script"] = true
		result.Nodes = append(result.Nodes, n)
	}
	for _, ed := range sub.Edges {
		if ed == nil {
			continue
		}
		if ed.From == virtual { // "file defines symbol" → the host file owns it
			ed.From = fileID
		}
		ed.FilePath = filePath
		if ed.Line > 0 {
			ed.Line += lineOffset
		}
		result.Edges = append(result.Edges, ed)
	}
	return file
}
