// Package licenses recognises SPDX license headers in source files,
// producing one license node per distinct SPDX identifier and an
// EdgeLicensedAs from each file to the relevant license. Lets agents
// answer "what's the license of this file" and "find every file
// shipping under GPL" without grepping.
//
// Scope (v1): per-file SPDX-License-Identifier headers in the first
// ten lines of source. The repo-level LICENSE-file fallback is not
// emitted from here — it would belong at MultiIndexer level (one
// pass per repo on warmup) and adds setup state out of scope here.
// Files without a header simply produce no license edge; agents can
// detect that gap with a graph query.
package licenses

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// scanBufPool holds reusable 64 KB scratch buffers for bufio.Scanner.
// See internal/codegen/scanner.go for the same rationale — license
// header detection runs on every indexed file too.
var scanBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 64*1024)
		return &b
	},
}

// spdxRe matches the canonical SPDX header line. The standard form
// is:
//
//	SPDX-License-Identifier: <expression>
//
// Authors usually wrap it in a comment opener. We accept any
// whitespace + comment-opener prefix on the same line, mirroring the
// TODO scanner's "comment context" rule. The captured group is the
// SPDX expression — we keep it verbatim (including AND / OR / WITH)
// so license-expression queries still see the structure.
var spdxRe = regexp.MustCompile(`(?:^|[\s/#*\-]+)SPDX-License-Identifier:\s*([^\n\r*/]+?)\s*$`)

// maxScanLines is the per-file scan window. SPDX guidance puts the
// identifier on the first line after the optional shebang; ten is a
// generous upper bound that accommodates copyright headers and
// "Code generated …" comments without scanning whole files.
const maxScanLines = 10

// Scan returns the SPDX identifier found in the file header, or ""
// if none was present. The returned string is the verbatim
// expression (no normalisation) — callers that need a canonical
// form should normalise downstream.
func Scan(source []byte) string {
	if len(source) == 0 {
		return ""
	}
	bufPtr := scanBufPool.Get().(*[]byte)
	defer scanBufPool.Put(bufPtr)
	scanner := bufio.NewScanner(bytes.NewReader(source))
	scanner.Buffer(*bufPtr, 1024*1024)
	for i := 0; i < maxScanLines && scanner.Scan(); i++ {
		line := scanner.Text()
		if !strings.Contains(line, "SPDX-License-Identifier") {
			continue
		}
		if m := spdxRe.FindStringSubmatch(line); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	return ""
}

// BuildGraphArtifacts converts a per-file SPDX result into the node
// and edge to append. The license node is shared across files in a
// repo: a single `license::<spdx>` node per distinct expression.
// graph.AddNode is idempotent on ID, so emitting the same license
// for every file is cheap.
//
// In multi-repo mode the indexer's applyRepoPrefix pass prepends the
// repo prefix to every node ID — this is the same behaviour
// `annotation::<lang>::<name>` nodes get today. So a "shared license
// node" is repo-scoped, not graph-wide. Cross-repo de-duplication is
// out of scope for v1.
//
// filePath is the unprefixed path; applyRepoPrefix handles multi-
// repo namespacing downstream. Its spelling is preserved verbatim —
// the edge endpoint must match the extractor's file-node ID spelling
// (OS-native separators on Windows) or the edge dangles.
func BuildGraphArtifacts(filePath, spdx, language string) ([]*graph.Node, []*graph.Edge) {
	if spdx == "" {
		return nil, nil
	}
	licenseID := LicenseNodeID(spdx)
	licenseNode := &graph.Node{
		ID:       licenseID,
		Kind:     graph.KindLicense,
		Name:     spdx,
		FilePath: filePath, // file path of first sighting; not authoritative
		Language: language,
		Meta: map[string]any{
			"spdx": spdx,
		},
	}
	edge := &graph.Edge{
		From:     filePath,
		To:       licenseID,
		Kind:     graph.EdgeLicensedAs,
		FilePath: filePath,
		Origin:   graph.OriginASTResolved,
	}
	return []*graph.Node{licenseNode}, []*graph.Edge{edge}
}

// LicenseNodeID returns the canonical ID for a license node. Shared
// across files within a repo (one node per distinct SPDX
// expression). The "license::" prefix follows the same synthetic-
// node convention as `annotation::<lang>::<name>`.
func LicenseNodeID(spdx string) string {
	return "license::" + spdx
}
