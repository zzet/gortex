package main

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This is the supported public shape captured by the actual R2 quartet. It
// deliberately has no invented end_line on defines, and includes a real local.
func readbackReferenceActualContext(t *testing.T) (readbackReferenceImage, map[string]any) {
	t.Helper()
	image, err := readbackReferenceParse("zz_gortex_readback_probe/modify_body/p000.go", "package readbackprobe\n\nfunc GortexReadback000() int {\n\tvalue := 7\n\treturn value\n}\n\nfunc GortexReadback000Stable() int { return 2 }\n")
	require.NoError(t, err)
	image.Prefix = "search-gortex-go"
	id := image.Prefix + "/" + image.File + "::"
	return image, map[string]any{
		"file": map[string]any{"id": image.Prefix + "/" + image.File, "language": "go"},
		"defines": []any{
			map[string]any{"id": id + "GortexReadback000", "kind": "function", "name": "GortexReadback000", "start_line": 3},
			map[string]any{"id": id + "GortexReadback000#local:value@+2", "kind": "local", "name": "value", "start_line": 4},
			map[string]any{"id": id + "GortexReadback000Stable", "kind": "function", "name": "GortexReadback000Stable", "start_line": 8},
		},
		"freshness": map[string]any{"file_view": "projected", "readback_exact": true, "graph_freshness": "pending", "freshness_scope": "relations"},
	}
}

func TestReadbackReferenceActualPublicContextDefines(t *testing.T) {
	image, body := readbackReferenceActualContext(t)
	require.True(t, readbackReferenceCorrect("editing_context", readbackReferenceWire(body), image, "GortexReadback000", false), "actual supported defines and independently verified starts must pass")
	_, functionOnly := readbackReferenceActualContext(t)
	functionOnly["defines"] = []any{functionOnly["defines"].([]any)[0], functionOnly["defines"].([]any)[2]}
	require.True(t, readbackReferenceCorrect("editing_context", readbackReferenceWire(functionOnly), image, "GortexReadback000", false), "optional locals are not required function definitions")
	for name, mutate := range map[string]func(map[string]any){
		"missing_target":      func(b map[string]any) { b["defines"] = b["defines"].([]any)[1:] },
		"missing_neighbor":    func(b map[string]any) { b["defines"] = b["defines"].([]any)[:2] },
		"stale_neighbor_line": func(b map[string]any) { b["defines"].([]any)[2].(map[string]any)["start_line"] = 5 },
		"wrong_target_line":   func(b map[string]any) { b["defines"].([]any)[0].(map[string]any)["start_line"] = 4 },
		"cross_file":          func(b map[string]any) { b["file"] = map[string]any{"id": "foreign/p000.go"} },
		"cross_file_node": func(b map[string]any) {
			b["defines"].([]any)[0].(map[string]any)["id"] = "foreign/p000.go::GortexReadback000"
		},
		"foreign_explicit_file":    func(b map[string]any) { b["defines"].([]any)[0].(map[string]any)["file_path"] = "foreign/p000.go" },
		"wrong_kind":               func(b map[string]any) { b["defines"].([]any)[0].(map[string]any)["kind"] = "local" },
		"wrong_name":               func(b map[string]any) { b["defines"].([]any)[0].(map[string]any)["name"] = "Other" },
		"local_substitutes_target": func(b map[string]any) { b["defines"] = []any{b["defines"].([]any)[1], b["defines"].([]any)[2]} },
		"unknown_local":            func(b map[string]any) { b["defines"].([]any)[1].(map[string]any)["name"] = "not_in_source" },
		"stale_local_line":         func(b map[string]any) { b["defines"].([]any)[1].(map[string]any)["start_line"] = 5 },
		"foreign_local_owner": func(b map[string]any) {
			b["defines"].([]any)[1].(map[string]any)["id"] = image.Prefix + "/" + image.File + "::GortexReadback000Stable#local:value@+2"
		},
		"duplicate_target": func(b map[string]any) {
			b["defines"] = append(b["defines"].([]any), maps.Clone(b["defines"].([]any)[0].(map[string]any)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, bad := readbackReferenceActualContext(t)
			mutate(bad)
			require.False(t, readbackReferenceCorrect("editing_context", readbackReferenceWire(bad), image, "GortexReadback000", false))
		})
	}
}

func TestReadbackReferenceFreshScratchUsesExistingVolume(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native/reference volume identity is supported on Darwin/Linux")
	}
	parent := t.TempDir()
	fresh := filepath.Join(parent, "not-created", "fresh-run")
	before, err := inspectReadbackReferenceHost("native", fresh)
	require.NoError(t, err)
	require.NotNil(t, before.Host.Limits["disk_capacity_bytes"].Value, "initial capacity must be observable before the fresh namespace exists")
	require.NotEmpty(t, before.VolumeIdentity)
	require.Equal(t, parent, before.ProbePath)
	_, err = os.Lstat(fresh)
	require.ErrorIs(t, err, os.ErrNotExist, "census must not allocate the future run")
	require.NoError(t, os.MkdirAll(fresh, 0700))
	after, err := inspectReadbackReferenceHost("native", before.ProbePath)
	require.NoError(t, err)
	require.Equal(t, before, after, "fresh directory allocation cannot masquerade as a changed host")
}

func TestReadbackReferenceContextAuxiliaryDefinitionsAreSourceBound(t *testing.T) {
	image, err := readbackReferenceParse("params.go", "package probe\n\nfunc Target(optional int) int { return optional }\n\nfunc Survivor() int { return 2 }\n")
	require.NoError(t, err)
	image.Prefix = "owned"
	param := map[string]any{"id": "owned/params.go::Target#param:optional", "name": "optional", "kind": "param", "start_line": 3}
	body := map[string]any{"file": map[string]any{"id": "owned/params.go"}, "defines": []any{
		map[string]any{"id": "owned/params.go::Target", "name": "Target", "kind": "function", "start_line": 3},
		param,
		map[string]any{"id": "owned/params.go::Survivor", "name": "Survivor", "kind": "function", "start_line": 5},
	}}
	require.True(t, readbackReferenceCorrect("editing_context", readbackReferenceWire(body), image, "Target", false))
	param["kind"] = "local"
	require.False(t, readbackReferenceCorrect("editing_context", readbackReferenceWire(body), image, "Target", false))
	param["kind"], param["start_line"] = "param", 4
	require.False(t, readbackReferenceCorrect("editing_context", readbackReferenceWire(body), image, "Target", false))
}

func TestReadbackReferenceVolumeCensusRefusesSymlinkAndFile(t *testing.T) {
	parent := t.TempDir()
	file := filepath.Join(parent, "file")
	require.NoError(t, os.WriteFile(file, []byte("owned"), 0600))
	snapshot, err := inspectReadbackReferenceHost("native", file)
	require.Error(t, err)
	require.NotEmpty(t, snapshot.Error)
	if runtime.GOOS == "windows" {
		return // The regular-file refusal above remains platform independent.
	}
	link := filepath.Join(parent, "link")
	require.NoError(t, os.Symlink(parent, link))
	snapshot, err = inspectReadbackReferenceHost("native", filepath.Join(link, "fresh"))
	require.Error(t, err)
	require.NotEmpty(t, snapshot.Error)
}

func TestReadbackReferenceDiagnosticCountsAreOne(t *testing.T) {
	schedules, err := readbackReferenceSchedules(readbackReferencePlan{Seed: 1, EditMeanNS: int64(editProbeMean), ProductDeadlineSeconds: 120})
	require.NoError(t, err)
	diagnostic := readbackReferenceProtocolSchedule(schedules[0], 120*time.Second)
	require.Equal(t, 1, readbackReferencePhaseOffered(diagnostic, "protocol_preflight"))
	require.Equal(t, 100, diagnostic.Cell.Offers, "nominal acceptance cell remains unchanged")
	require.Equal(t, 100, readbackReferencePhaseOffered(schedules[0], "under_edit"))
	require.Equal(t, 100, readbackReferencePhaseOffered(diagnostic, "under_edit"), "ordinary acceptance cannot borrow the diagnostic truncation")
	require.Zero(t, summarizeReadbackReference(readbackReferencePhaseOffered(diagnostic, "protocol_preflight"), []bool{false}).Missing)
	require.Equal(t, 1, summarizeReadbackReference(readbackReferencePhaseOffered(diagnostic, "protocol_preflight"), []bool{false}).Failures)
	require.Equal(t, 1, readbackReferenceObservedOffers([]readbackReferenceRecord{{ProbeOrigin: time.Now()}, {Errors: []string{"censored before dispatch"}}}))
}

func TestReadbackReferenceACKCorrectIsActualValidation(t *testing.T) {
	image, bodies := readbackReferenceOracleFixture(t)
	trial := readbackReferenceTrial{ProbeFile: image.File, Target: "Target", After: map[string]readbackReferenceImage{image.File: image}, Request: request{Tool: "edit", Operation: "file", Arguments: map[string]any{"operation": "file"}}}
	for name, ack := range map[string]map[string]any{
		"actual_local":       {"disk_status": "committed", "ack_mode": "local", "content_sha256": readbackReferenceSHA(image.Bytes)},
		"wrong_sha":          {"disk_status": "committed", "ack_mode": "local", "content_sha256": "foreign"},
		"unknown_policy":     {"disk_status": "committed", "ack_mode": "other", "content_sha256": readbackReferenceSHA(image.Bytes)},
		"full_still_pending": {"disk_status": "committed", "ack_mode": "full", "content_sha256": readbackReferenceSHA(image.Bytes), "graph_status": "pending"},
	} {
		t.Run(name, func(t *testing.T) {
			caller := referenceACKModeCaller{&referenceOrderedCaller{bodies: bodies, image: image}, ack}
			row := observeReadbackReference(t.Context(), caller, privateRepo{Path: "/owned", Prefix: "owned"}, trial, "under_edit", time.Second)
			require.Equal(t, name == "actual_local", row.ACK.Correct)
		})
	}
}
