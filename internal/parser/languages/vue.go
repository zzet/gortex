package languages

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// VueExtractor extracts Vue single-file components. Each .vue file becomes a
// file node plus one always-exported component node, and every <script> /
// <script setup> block is carved out and delegated to the TypeScript or
// JavaScript extractor (per its lang attribute) so the component's real logic —
// functions, imports, calls — lands in the graph rebased into host-file
// coordinates. Without this, a .vue file's script is effectively un-indexed.
type VueExtractor struct {
	ts *TypeScriptExtractor
	js *JavaScriptExtractor
}

// NewVueExtractor constructs a Vue SFC extractor.
func NewVueExtractor() *VueExtractor {
	return &VueExtractor{ts: NewTypeScriptExtractor(), js: NewJavaScriptExtractor()}
}

func (e *VueExtractor) Language() string     { return "vue" }
func (e *VueExtractor) Extensions() []string { return []string{".vue"} }

func (e *VueExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	lineCount := 1 + strings.Count(string(src), "\n")

	fileNode := &graph.Node{
		ID: filePath, Kind: graph.KindFile, Name: filePath,
		FilePath: filePath, StartLine: 1, EndLine: lineCount, Language: "vue",
	}
	result.Nodes = append(result.Nodes, fileNode)

	// One always-exported component node per SFC, named after the file, so a
	// parent component's <ChildComponent/> usage has a target to resolve to.
	componentName := vueComponentName(filePath)
	componentID := filePath + "::" + componentName
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: componentID, Kind: graph.KindType, Name: componentName,
		FilePath: filePath, StartLine: 1, EndLine: lineCount, Language: "vue",
		Meta: map[string]any{"component": true, "exported": true, "ui_component": "vue", "type_flavor": "component"},
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileNode.ID, To: componentID, Kind: graph.EdgeDefines, FilePath: filePath, Line: 1,
	})

	// Carve every <script> / <script setup> block and delegate to the matching
	// embedded-language extractor, rebased into host-file coordinates.
	for _, m := range markupScriptRe.FindAllSubmatchIndex(src, -1) {
		attrs := src[m[2]:m[3]]
		contentStart, contentEnd := m[4], m[5]
		if contentStart < 0 {
			continue
		}
		content := src[contentStart:contentEnd]
		lineOffset := strings.Count(string(src[:contentStart]), "\n")
		var delegate parser.Extractor = e.js
		ts, setup := markupScriptAttributes(attrs)
		if ts {
			delegate = e.ts
		}
		script := delegateInlineScriptSlice(delegate, content, lineOffset, filePath, fileNode.ID, "vue", result)
		if script != nil && !setup {
			if fileNode.Meta == nil {
				fileNode.Meta = make(map[string]any)
			}
			fileNode.Meta[graph.JSExportScopeMeta] = strings.TrimPrefix(script.ID, filePath)
			for _, key := range [...]string{graph.JSLocalExportsMeta, graph.DefaultExportMeta} {
				if value := script.Meta[key]; value != nil {
					fileNode.Meta[key] = value
				}
			}
		}
	}
	mineTemplateComponentUsages(src, filePath, componentID, "vue", result)
	applyFrameworkTemplatePasses(src, filePath, componentID, "vue", result)
	return result, nil
}

// vueComponentName derives the component name from the file name (Counter.vue ->
// Counter).
func vueComponentName(filePath string) string {
	base := filepath.Base(filePath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
