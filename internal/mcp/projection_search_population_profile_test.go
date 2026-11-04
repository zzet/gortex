//go:build acceptance

package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const projectionLatencyPendingComment = "\n// pending search comparison fixture\n"

const projectionLatencySelectionPolicy = "directory-round-robin/path-order/actual-charge-feasibility-v1"

const projectionLatencyNativeMembershipEncoding = "UTF-8 sorted IDs joined by LF with terminal LF"

type projectionLatencyPopulationManifest struct {
	Revision              string                       `json:"revision"`
	Policy                string                       `json:"policy"`
	Requested             int                          `json:"requested"`
	Admitted              int                          `json:"admitted"`
	MaxFiles              int                          `json:"max_files"`
	MaxBytes              int64                        `json:"max_bytes"`
	ChargedBytes          int64                        `json:"charged_bytes"`
	IndexedFiles          int                          `json:"indexed_files"`
	NativeSymbols         int                          `json:"native_symbols"`
	NativeSymbolsSHA256   string                       `json:"native_symbols_sha256"`
	NativeSymbolsEncoding string                       `json:"native_symbols_encoding"`
	EligibleCount         int                          `json:"eligible_count"`
	SelectedNodes         int                          `json:"selected_nodes"`
	SelectedEdges         int                          `json:"selected_edges"`
	SelectedFuncs         int                          `json:"selected_functions"`
	InputsVerified        bool                         `json:"inputs_verified"`
	Entries               []projectionLatencyCandidate `json:"entries"`
}

func validateProjectionLatencyPopulationManifest(manifest projectionLatencyPopulationManifest, revision string, count int) error {
	if count <= 0 || count > projectionMaxFiles || manifest.Revision != revision || len(revision) != 40 || manifest.Policy != projectionLatencySelectionPolicy {
		return fmt.Errorf("population revision, policy or requested count differs")
	}
	if manifest.Requested != count || manifest.Admitted != count || len(manifest.Entries) != count || manifest.MaxFiles != projectionMaxFiles || manifest.MaxBytes != projectionMaxBytes {
		return fmt.Errorf("population lacks all %d admitted entries under unchanged caps", count)
	}
	if manifest.IndexedFiles <= 0 || manifest.NativeSymbols <= 0 || manifest.EligibleCount < count {
		return fmt.Errorf("actual native corpus/eligible population unavailable")
	}
	digest, err := hex.DecodeString(manifest.NativeSymbolsSHA256)
	if err != nil || len(digest) != sha256.Size || manifest.NativeSymbolsEncoding != projectionLatencyNativeMembershipEncoding {
		return fmt.Errorf("exact native symbol membership digest/domain unavailable")
	}
	if !manifest.InputsVerified {
		return fmt.Errorf("source literal and covered/uncovered query witnesses are unverified")
	}
	var charged int64
	var nodes, edges, functions int
	seen := map[string]bool{}
	for _, item := range manifest.Entries {
		_, hashErr := hex.DecodeString(item.RawSHA256)
		if item.Path == "." || item.Path == "" || path.IsAbs(item.Path) || path.Clean(item.Path) != item.Path || strings.HasPrefix(item.Path, "../") || strings.ContainsAny(item.Path, "\\:") || !strings.HasSuffix(item.Path, ".go") || seen[item.Path] {
			return fmt.Errorf("invalid or duplicate pinned path %q", item.Path)
		}
		if hashErr != nil || len(item.RawSHA256) != 64 || item.ChargedBytes <= 0 || item.ChargedBytes > projectionMaxBytes-charged || item.Nodes <= 0 || item.Edges < 0 || item.Functions <= 0 || item.SourceBytes <= 0 || item.NamesCharged < 0 || item.StateCharged < 0 {
			return fmt.Errorf("actual charged source witness unavailable for %s", item.Path)
		}
		seen[item.Path] = true
		charged += item.ChargedBytes
		nodes += item.Nodes
		edges += item.Edges
		functions += item.Functions
	}
	if charged != manifest.ChargedBytes || nodes != manifest.SelectedNodes || edges != manifest.SelectedEdges || functions != manifest.SelectedFuncs {
		return fmt.Errorf("population charge/definition census differs")
	}
	return nil
}

// This setup-only census uses the same generation ownership rows as
// SymbolFTSCount. It runs after Index returns and before pending publication,
// warmup or timing; a token count or graph-node approximation cannot replace it.
func projectionLatencyNativeMembership(ctx context.Context, store *store_sqlite.Store, count int) (string, error) {
	ids := make([]string, 0, count)
	seen := make(map[string]bool, count)
	var cursor int64
	for {
		rows, next, err := store.SymbolFTSGenerationRows(ctx, store.ViewGeneration(), "", cursor, 2048)
		if err != nil {
			return "", err
		}
		for _, row := range rows {
			if row.NodeID == "" || !utf8.ValidString(row.NodeID) || strings.ContainsRune(row.NodeID, '\n') || seen[row.NodeID] {
				return "", fmt.Errorf("invalid or duplicate native symbol ownership ID")
			}
			seen[row.NodeID] = true
			ids = append(ids, row.NodeID)
		}
		if next == 0 {
			break
		}
		if next <= cursor {
			return "", fmt.Errorf("native symbol membership cursor did not advance")
		}
		cursor = next
	}
	if count <= 0 || len(ids) != count {
		return "", fmt.Errorf("native symbol membership has %d IDs, expected %d", len(ids), count)
	}
	sort.Strings(ids)
	digest := sha256.Sum256([]byte(strings.Join(ids, "\n") + "\n"))
	return hex.EncodeToString(digest[:]), nil
}

func validateProjectionLatencyNativeCensus(manifest projectionLatencyPopulationManifest, files, count int, digest string) error {
	if files != manifest.IndexedFiles || count != manifest.NativeSymbols || digest != manifest.NativeSymbolsSHA256 {
		return fmt.Errorf("native population differs: files=%d symbols=%d sha256=%s", files, count, digest)
	}
	return nil
}

func readProjectionLatencyPopulationManifest(revision string, count int) (projectionLatencyPopulationManifest, error) {
	var manifest projectionLatencyPopulationManifest
	name := os.Getenv("GORTEX_SEARCH_LATENCY_POPULATION")
	if !filepath.IsAbs(name) {
		return manifest, fmt.Errorf("explicit pinned512 population manifest required before timing")
	}
	raw, err := os.ReadFile(name)
	if err != nil {
		return manifest, err
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return manifest, err
	}
	return manifest, validateProjectionLatencyPopulationManifest(manifest, revision, count)
}

func writeProjectionLatencySetup(t *testing.T, fixture *projectionLatencyFixture, requested int, failure *string) {
	t.Helper()
	out := os.Getenv("GORTEX_SEARCH_LATENCY_OUTPUT")
	if out == "" {
		return
	}
	snapshot := fixture.registry.snap.Load()
	record := map[string]any{
		"requested": requested, "admitted": len(snapshot.entries), "charged_bytes": snapshot.bytes,
		"max_files": projectionMaxFiles, "max_bytes": projectionMaxBytes,
		"indexed_files": fixture.files, "native_symbols": fixture.nativeSymbols, "native_symbols_sha256": fixture.nativeSymbolsSHA256, "native_symbols_encoding": projectionLatencyNativeMembershipEncoding, "failure": *failure,
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(out, fmt.Sprintf("setup-%d.json", requested)), append(raw, '\n'), 0o600)
	}
	if err != nil {
		t.Errorf("retain actual projection setup census: %v", err)
	}
}

func TestProjectionLatencyPopulationPreservesChargedNamesAcrossReadCOW(t *testing.T) {
	selectedProjectionRegistry(t)
	server, name, physical, _ := acknowledgedCanonicalFile(t)
	require.NotNil(t, physical.names)
	require.Positive(t, physical.names.Count())
	require.NotNil(t, physical.fileView)
	oldContext, err := retainRequestProjection(t.Context(), physical)
	require.NoError(t, err)
	pendingFileProjections.markSuspect(name)
	read, err := server.projectCurrentTrackedFile(t.Context(), t.Context(), name, physical.graphPath, physical.graphPath)
	require.NoError(t, err)
	require.NotSame(t, physical, read)
	require.Zero(t, read.commitSeq)
	require.Equal(t, physical.sourceCommitSeq, read.sourceCommitSeq)
	require.Same(t, physical.layer, read.layer)
	require.Same(t, physical.names, read.names)
	require.Same(t, physical.fileView, read.fileView)
	require.Equal(t, physical.size, read.size)
	require.Equal(t, physical.size, pendingFileProjections.snap.Load().bytes)
	require.Error(t, validateRequestProjections(oldContext), "the new immutable owner must not rehabilitate a borrowed old owner")
}

// This unmeasured structural proof uses the original full Gortex archive,
// original index scope and actual production constructor/registry charges.
// It does not call handlers, collect latency samples, or grant a timed run.
func TestProjectionLatencyActualCorpusCapacityPreflight(t *testing.T) {
	out := os.Getenv("GORTEX_SEARCH_CAPACITY_OUTPUT")
	if out == "" {
		t.Skip("explicit owned capacity preflight only; no performance credit")
	}
	require.True(t, filepath.IsAbs(out) && strings.HasPrefix(out, "/Volumes/mmext4tb/"))
	require.NoError(t, os.Mkdir(out, 0o700), "fresh capacity output required")
	t.Setenv("GORTEX_SEARCH_LATENCY_OUTPUT", out)
	source, revision := os.Getenv("GORTEX_SEARCH_LATENCY_SOURCE"), os.Getenv("GORTEX_SEARCH_LATENCY_REVISION")
	require.NotEmpty(t, source)
	require.Len(t, revision, 40)
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = source
	head, err := command.Output()
	require.NoError(t, err)
	require.Equal(t, revision, strings.TrimSpace(string(head)))
	fixture := prepareProjectionLatencyFixture(t, source, revision, 0)
	var paths []string
	require.NoError(t, filepath.WalkDir(fixture.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(fixture.root, path)
		if relErr != nil {
			return relErr
		}
		if fixture.indexer.IsTracked(filepath.ToSlash(rel)) {
			paths = append(paths, path)
		}
		return nil
	}))
	sort.Strings(paths)
	var candidates []projectionLatencyCandidate
	var excluded []map[string]string
	complete := false
	defer func() {
		profile := map[string]any{"revision": revision, "full_indexed_files": fixture.files, "native_symbols": fixture.nativeSymbols, "tracked_go_files": len(paths), "profile_complete": complete, "eligible": candidates, "excluded_before_selection": excluded}
		raw, marshalErr := json.MarshalIndent(profile, "", "  ")
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(filepath.Join(out, "actual-charge-profile.json"), append(raw, '\n'), 0o600))
	}()
	for _, path := range paths {
		raw, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		raw = append(raw, []byte(projectionLatencyPendingComment)...)
		require.NoError(t, os.WriteFile(path, raw, 0o600))
		projection, projectErr := fixture.indexer.ProjectSource(t.Context(), path, raw)
		if projectErr != nil {
			excluded = append(excluded, map[string]string{"path": filepath.ToSlash(strings.TrimPrefix(path, fixture.root+string(filepath.Separator))), "reason": projectErr.Error()})
			require.NoError(t, projectErr, "a projection refusal cannot be silently removed to make capacity appear valid")
		}
		functions := 0
		for _, node := range projection.Nodes {
			if node.Kind == graph.KindFunction {
				functions++
			}
		}
		if functions == 0 {
			excluded = append(excluded, map[string]string{"path": projection.GraphPath, "reason": "no_function_target_for_this_search_workload"})
			continue
		}
		stamp, stampErr := statDiskStamp(path)
		require.NoError(t, stampErr)
		entry := newProjectionEntry(path, fixture.indexer.RepoPrefix(), projection, raw, 0, stamp)
		hash := sha256.Sum256(raw)
		stateCharged := entry.size - int64(len(projection.Nodes))*projectionNodeBytes - int64(len(projection.Edges))*projectionEdgeBytes - entry.names.chargedBytes
		candidates = append(candidates, projectionLatencyCandidate{Path: projection.GraphPath, ChargedBytes: entry.size, RawSHA256: hex.EncodeToString(hash[:]), SourceBytes: len(raw), Nodes: len(projection.Nodes), Edges: len(projection.Edges), Functions: functions, NamesCharged: entry.names.chargedBytes, StateCharged: stateCharged})
	}
	complete = true
	selected, err := selectProjectionLatencyPopulation(candidates, projectionMaxFiles, projectionMaxBytes)
	require.NoError(t, err)
	manifest := projectionLatencyPopulationManifest{Revision: revision, Policy: projectionLatencySelectionPolicy, Requested: projectionMaxFiles, MaxFiles: projectionMaxFiles, MaxBytes: projectionMaxBytes, IndexedFiles: fixture.files, NativeSymbols: fixture.nativeSymbols, NativeSymbolsSHA256: fixture.nativeSymbolsSHA256, NativeSymbolsEncoding: projectionLatencyNativeMembershipEncoding, EligibleCount: len(candidates), Entries: selected}
	failure := ""
	defer writeProjectionLatencySetup(t, fixture, projectionMaxFiles, &failure)
	for _, candidate := range selected {
		path := filepath.Join(fixture.root, filepath.FromSlash(candidate.Path))
		raw, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		stamp, stampErr := statDiskStamp(path)
		require.NoError(t, stampErr)
		entry, projectErr := projectFile(t.Context(), fixture.indexer, path, raw, 0, stamp)
		if projectErr != nil {
			failure = projectErr.Error()
		}
		require.NoError(t, projectErr, "every pinned member must remain projectable")
		require.Equal(t, candidate.ChargedBytes, entry.size)
		publishErr := fixture.registry.publish(entry)
		if publishErr != nil {
			failure = publishErr.Error()
		}
		require.NoError(t, publishErr, "record actual count/charge before any capacity refusal")
		fixture.covered[entry.graphPath] = true
		manifest.SelectedNodes += candidate.Nodes
		manifest.SelectedEdges += candidate.Edges
		manifest.SelectedFuncs += candidate.Functions
	}
	fixture.snapshot = fixture.registry.snap.Load()
	manifest.Admitted, manifest.ChargedBytes = len(fixture.snapshot.entries), fixture.snapshot.bytes
	require.Equal(t, projectionMaxFiles, manifest.Admitted)
	require.LessOrEqual(t, manifest.ChargedBytes, int64(projectionMaxBytes))
	raw, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(out, "population-capacity-only.json"), append(raw, '\n'), 0o600))
	probes := projectionLatencyProbes(t, fixture)
	require.Len(t, probes, 40, "retain10covered/10uncovered real unique function targets and both tools")
	witnesses, err := json.MarshalIndent(probes, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(out, "population-probes.json"), append(witnesses, '\n'), 0o600))
	for _, probe := range probes {
		if probe.Tool != "search_text" {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(fixture.root, filepath.FromSlash(probe.File)))
		require.NoError(t, readErr)
		lines := strings.Split(string(body), "\n")
		require.Greater(t, probe.Line, 0)
		require.LessOrEqual(t, probe.Line, len(lines))
		require.Contains(t, lines[probe.Line-1], probe.Query, "the pinned literal must actually exist at its claimed source line before timing")
	}
	manifest.InputsVerified = true
	require.NoError(t, validateProjectionLatencyPopulationManifest(manifest, revision, projectionMaxFiles))
	raw, err = json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(out, "population-512.json"), append(raw, '\n'), 0o600))
}
