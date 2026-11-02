package mcp

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unsafe"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/pathkey"
)

// This bounded immutable evidence lives only inside the existing projection
// registry. Physical receipts and replay caches must not retain its strings.
type fileViewEvidence struct {
	Delta  fileSymbolDelta
	Syntax fileViewSyntaxHealth
}

type fileViewSyntaxHealth struct {
	State    string `json:"state"`
	Checked  bool   `json:"checked"`
	Errors   *int   `json:"errors"`
	Language string `json:"language,omitempty"`
}

type mutationFileView struct {
	State        string                 `json:"state"`
	Reason       string                 `json:"reason,omitempty"`
	Historical   bool                   `json:"historical,omitempty"`
	DeltaState   string                 `json:"delta_state"`
	DeltaReason  string                 `json:"delta_reason,omitempty"`
	Counts       *fileSymbolDeltaCounts `json:"counts"`
	Symbols      []fileSymbolDeltaEntry `json:"symbols"`
	Truncated    bool                   `json:"truncated"`
	SyntaxHealth fileViewSyntaxHealth   `json:"syntax_health"`
}

func projectionSyntaxHealth(entry *projectionEntry) fileViewSyntaxHealth {
	health := fileViewSyntaxHealth{State: "unknown"}
	if entry == nil {
		return health
	}
	health.Language = entry.language
	if entry.tombstone {
		health.State = "not_applicable_removed"
		return health
	}
	if entry.parseChecked && entry.parseErrors >= 0 && entry.hash == hex.EncodeToString(entry.rawHash[:]) {
		errors := entry.parseErrors
		health.State = "checked"
		health.Checked = true
		health.Errors = &errors
	}
	return health
}

func (s *Server) prepareFileViewEvidence(ctx context.Context, prepared *preparedFileProjection, before *plannedSource, after []byte) {
	if prepared == nil || prepared.entry == nil {
		return
	}
	entry := prepared.entry
	evidence := &fileViewEvidence{Syntax: projectionSyntaxHealth(entry)}
	if !entry.tombstone && entry.hash != hex.EncodeToString(entry.rawHash[:]) {
		evidence.Delta = fileSymbolDelta{State: "unavailable", Reason: "transformed_source"}
	} else {
		budget, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
		old, known, reason := s.fileViewBaseline(budget, prepared, before)
		if before == nil {
			before = &plannedSource{}
		}
		evidence.Delta = buildFileSymbolDelta(indexer.DecodeUTF16Source(before.before), indexer.DecodeUTF16Source(after), old, fileViewSymbols(entry.layer.FileNodes(entry.graphPath)), known)
		if !known && reason != "" {
			evidence.Delta.Reason = reason
		}
		if budget.Err() != nil {
			evidence.Delta = fileSymbolDelta{State: "unavailable", Reason: "baseline_deadline"}
		}
		cancel()
	}
	// Tight copies prevent the display slice or a substring from retaining
	// undisplayed changes or an entire older source outside the byte allowance.
	evidence.Delta.Symbols = slices.Clone(evidence.Delta.Symbols)
	for i := range evidence.Delta.Symbols {
		row := &evidence.Delta.Symbols[i]
		row.ID = strings.Clone(row.ID)
		row.Kind = graph.NodeKind(strings.Clone(string(row.Kind)))
		row.Signature = strings.Clone(row.Signature)
	}
	entry.fileView = evidence
	entry.size += fileViewEvidenceCharge(evidence)
}

func fileViewSymbols(nodes []*graph.Node) []*graph.Node {
	symbols := make([]*graph.Node, 0, len(nodes))
	for _, node := range nodes {
		if node == nil {
			symbols = append(symbols, nil)
			continue
		}
		switch node.Kind {
		case graph.KindFile, graph.KindPackage, graph.KindImport:
			continue
		}
		symbols = append(symbols, node)
	}
	return symbols
}

// Authenticate a complete prior symbol set against the very same selected
// reader and the caller's held-path byte snapshot. No canonical fallback can
// fill a missing selected capability, inventory row, or newer pointer.
func (s *Server) fileViewBaseline(ctx context.Context, prepared *preparedFileProjection, before *plannedSource) ([]*graph.Node, bool, string) {
	entry := prepared.entry
	if before == nil || pathkey.MutationKey(before.path) != prepared.key || !mutationPathAdmitted(ctx, prepared.key) {
		return nil, false, "baseline_unknown"
	}
	if !before.existed {
		stamp, err := statDiskStamp(entry.absPath)
		if err == nil && stamp.info == nil && pendingFileProjections.snap.Load().entries[prepared.key] == prepared.expected {
			return nil, true, ""
		}
		return nil, false, "absence_not_current"
	}
	rawHash := blake3.Sum256(before.before)
	prior := prepared.expected
	if prior != nil && !prior.tombstone && prior.graphPath == entry.graphPath && prior.repoPrefix == entry.repoPrefix && prior.language == entry.language && prior.rawHash == rawHash && !prior.suspect.Load() && pendingFileProjections.snap.Load().entries[prepared.key] == prior {
		nodes := fileViewSymbols(prior.layer.FileNodes(prior.graphPath))
		if !prior.suspect.Load() && pendingFileProjections.snap.Load().entries[prepared.key] == prior {
			return nodes, true, ""
		}
		return nil, false, "baseline_replaced"
	}
	reader := s.readerFor(ctx)
	inventory, ok := reader.(graph.ConstantValueProjectionReader)
	if !ok {
		return nil, false, "baseline_metadata_unsupported"
	}
	key := graph.ConstantFileKey{RepoPrefix: entry.repoPrefix, FilePath: entry.graphPath}
	read := func() (graph.FileMetaRow, bool) {
		rows, err := inventory.ReadConstantValueProjectionContext(ctx, nil, []graph.ConstantFileKey{key})
		if err != nil {
			return graph.FileMetaRow{}, false
		}
		row, found := rows.Files[key]
		return row, found
	}
	first, found := read()
	if !found || first.ContentHash == "" || first.ContentHash != hex.EncodeToString(rawHash[:]) {
		return nil, false, "baseline_bytes_not_accepted"
	}
	rows, err := graph.ContractFileNodesContext(ctx, reader, entry.repoPrefix, []string{entry.graphPath})
	if err != nil {
		return nil, false, "baseline_nodes_unavailable"
	}
	last, found := read()
	if !found || first != last || ctx.Err() != nil || pendingFileProjections.snap.Load().entries[prepared.key] != prepared.expected {
		return nil, false, "baseline_changed"
	}
	nodes, found := rows[entry.graphPath]
	if !found || len(nodes) != first.NodeCount {
		return nil, false, "baseline_nodes_incomplete"
	}
	return fileViewSymbols(nodes), true, ""
}

func fileViewEvidenceCharge(e *fileViewEvidence) int64 {
	if e == nil {
		return 0
	}
	// Round each separately owned allocation conservatively, including headers.
	allocation := func(n int64) int64 {
		if n <= 0 {
			return 0
		}
		return ((n+15)/16)*16 + 64
	}
	size := allocation(int64(unsafe.Sizeof(*e))) + allocation(int64(cap(e.Delta.Symbols))*int64(unsafe.Sizeof(fileSymbolDeltaEntry{})))
	if e.Delta.Counts != nil {
		size += allocation(int64(unsafe.Sizeof(*e.Delta.Counts)))
	}
	if e.Syntax.Errors != nil {
		size += allocation(int64(unsafe.Sizeof(*e.Syntax.Errors)))
	}
	for _, row := range e.Delta.Symbols {
		size += allocation(int64(len(row.ID))) + allocation(int64(len(row.Kind))) + allocation(int64(len(row.Signature)))
	}
	for _, value := range []string{e.Delta.State, e.Delta.Reason, e.Syntax.State, e.Syntax.Language} {
		size += allocation(int64(len(value)))
	}
	return size
}

func fileViewForEntry(entry *projectionEntry) mutationFileView {
	view := mutationFileView{State: "projected", DeltaState: "unavailable", DeltaReason: "baseline_unknown", SyntaxHealth: projectionSyntaxHealth(entry)}
	if entry != nil && entry.fileView != nil {
		evidence := entry.fileView
		view.DeltaState, view.DeltaReason = evidence.Delta.State, evidence.Delta.Reason
		view.Counts, view.Symbols, view.Truncated = evidence.Delta.Counts, evidence.Delta.Symbols, evidence.Delta.Truncated
		view.SyntaxHealth = evidence.Syntax
	}
	return view
}

func mutationFileGraphHint(payload map[string]any, exact bool) string {
	status, ok := payload["graph_status"].(string)
	if !ok || status == "" {
		status = "unobserved"
	}
	if exact {
		return fmt.Sprintf("file current; recorded graph status: %s", status)
	}
	return fmt.Sprintf("file readback unavailable; recorded graph status: %s", status)
}

func baselineForPreparedWrite(absPath string, before []byte) *plannedSource {
	if before != nil {
		return &plannedSource{path: absPath, before: before, existed: true}
	}
	_, err := os.Lstat(absPath)
	if os.IsNotExist(err) {
		return &plannedSource{path: absPath, existed: false}
	}
	return nil
}

// Capture only a bounded, regular source under the existing physical path
// admission. Failure makes delta evidence unavailable; it never grants writes.
func captureFileViewBaseline(ctx context.Context, absPath string) *plannedSource {
	if !mutationPathAdmitted(ctx, pathkey.MutationKey(absPath)) {
		return nil
	}
	before, err := statDiskStamp(absPath)
	if err != nil {
		return nil
	}
	if before.info == nil {
		return &plannedSource{path: absPath, existed: false}
	}
	if !before.info.Mode().IsRegular() || before.info.Size() > 1<<20 {
		return nil
	}
	files, err := source.NewFilesystemSource(filepath.Dir(absPath))
	if err != nil {
		return nil
	}
	defer files.Close()
	raw, _, err := files.ReadRegularFile(ctx, filepath.Base(absPath), 1<<20)
	if err != nil {
		return nil
	}
	after, err := statDiskStamp(absPath)
	if err != nil || !before.matches(after) || ctx.Err() != nil {
		return nil
	}
	return &plannedSource{path: absPath, before: raw, existed: true}
}
