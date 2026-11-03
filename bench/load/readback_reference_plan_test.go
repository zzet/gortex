package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadbackReferenceScheduleKeepsAcceptedPacingAndExactCells(t *testing.T) {
	p := readbackReferencePlan{Seed: 1, EditMeanNS: int64(15 * time.Second), ProductDeadlineSeconds: 120}
	cells, err := readbackReferenceSchedules(p)
	require.NoError(t, err)
	require.Len(t, cells, 90)
	for i, c := range cells {
		require.Equal(t, 100, c.Cell.Offers)
		require.Len(t, c.Arrival, 100)
		require.Positive(t, c.OfferedTailNS)
		require.Equal(t, c.OfferedTailNS+int64(240*time.Second), c.PhaseBoundNS)
		for _, other := range cells[i+1:] {
			if c.Cell.Agents == other.Cell.Agents {
				require.Equal(t, c.Arrival, other.Arrival, "kind/ACK policy must not reseed or compress offered traffic")
			}
		}
	}
}
func TestReadbackReferencePlanRejectsUnknownFieldsAndChangedPacing(t *testing.T) {
	root := shortPrivateSocketBase(t)
	p := readbackReferencePlan{Version: 1, Binary: filepath.Join(root, "bin"), BinarySHA256: "0123456789012345678901234567890123456789012345678901234567890123", BinaryRevision: "0123456789012345678901234567890123456789", Manifest: filepath.Join(root, "manifest.json"), Scratch: filepath.Join(root, "scratch"), Output: filepath.Join(root, "out.json"), EditMeanNS: int64(15 * time.Second), ProductDeadlineSeconds: 120, WarmSeconds: 1800, StopSeconds: 30}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	_, err = decodeReadbackReferencePlan(raw)
	require.NoError(t, err)
	for _, path := range []string{"relative-bin", root + string(filepath.Separator) + "sub" + string(filepath.Separator) + ".." + string(filepath.Separator) + "bin"} {
		invalid := p
		invalid.Binary = path
		raw, err := json.Marshal(invalid)
		require.NoError(t, err)
		_, err = decodeReadbackReferencePlan(raw)
		require.ErrorContains(t, err, "absolute and canonical")
	}
	p.EditMeanNS = int64(time.Second)
	raw, _ = json.Marshal(p)
	_, err = decodeReadbackReferencePlan(raw)
	require.Error(t, err)
	_, err = decodeReadbackReferencePlan([]byte(`{"version":1,"unknown":true}`))
	require.Error(t, err)
}

func TestReadbackReferencePreflightRefusesChangedBinaryAndMissingBuildIdentity(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	require.NoError(t, os.Mkdir(source, 0700))
	corpus := []Corpus{{Label: "primary", Source: source, Commit: "0123456789012345678901234567890123456789", File: "main.go", Symbol: "main.go::Target", Query: "Target", ProposedSignature: "func Target() int"}}
	manifest := filepath.Join(root, "manifest.json")
	raw, err := json.Marshal(corpus)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifest, raw, 0600))
	binary := filepath.Join(root, "candidate")
	require.NoError(t, os.WriteFile(binary, []byte("not a Go native binary"), 0700))
	sum, err := readbackReferenceFileSHA(binary)
	require.NoError(t, err)
	p := readbackReferencePlan{Version: 1, Binary: binary, BinarySHA256: sum, BinaryRevision: corpus[0].Commit, Manifest: manifest, Scratch: filepath.Join(root, "scratch"), Output: filepath.Join(root, "output.json"), Seed: 1, EditMeanNS: int64(15 * time.Second), ProductDeadlineSeconds: 120, WarmSeconds: 1800, StopSeconds: 30}
	_, err = preflightReadbackReference(p)
	require.ErrorContains(t, err, "build metadata")
	p.BinarySHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	_, err = preflightReadbackReference(p)
	require.ErrorContains(t, err, "SHA256 changed")
	require.NoDirExists(t, p.Scratch, "preflight never allocates a private daemon/store or clone")
}
