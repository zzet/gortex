package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadbackReferenceSupportedLanguageSelectionKeepsGoDefaults(t *testing.T) {
	root := shortPrivateSocketBase(t)
	base := readbackReferencePlan{Version: 1, Binary: filepath.Join(root, "bin"), BinarySHA256: "0123456789012345678901234567890123456789012345678901234567890123", BinaryRevision: "0123456789012345678901234567890123456789", Manifest: filepath.Join(root, "manifest.json"), Scratch: filepath.Join(root, "scratch"), Output: filepath.Join(root, "out"), Seed: 1, EditMeanNS: int64(15 * time.Second), ProductDeadlineSeconds: 120, WarmSeconds: 1800, StopSeconds: 30, ToolSurface: readbackReferenceCompatibilitySurface}
	defaultCells, err := readbackReferenceSchedules(base)
	require.NoError(t, err)
	require.Len(t, defaultCells, 90)
	goldenRaw, err := os.ReadFile("testdata/readback-reference/go-default-trial-hashes.json")
	require.NoError(t, err)
	var golden map[string]string
	require.NoError(t, json.Unmarshal(goldenRaw, &golden))
	for kind, tools := range readbackReferenceTools {
		trial, err := readbackReferenceTrialFor(readbackReferenceCell{Agents: 8, Kind: kind, Tool: tools[0], Offers: 100}, editProbeArrival{Editor: 3, DueNS: 15000000000}, 7)
		require.NoError(t, err)
		hash, err := hashJSON(trial)
		require.NoError(t, err)
		require.Equal(t, golden[kind], hash, "default Go payload/source spans must remain exact")
	}
	for _, language := range []string{"typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			raw, err := json.Marshal(base)
			require.NoError(t, err)
			var wire map[string]any
			require.NoError(t, json.Unmarshal(raw, &wire))
			wire["language"] = language
			raw, err = json.Marshal(wire)
			require.NoError(t, err)
			plan, err := decodeReadbackReferencePlan(raw)
			require.NoError(t, err, "supported existing1005 language must be explicitly selectable for strict1014 reads")
			cells, err := readbackReferenceSchedules(plan)
			require.NoError(t, err)
			require.Len(t, cells, 80, "eight applicable kinds; Go-only relocation stays a separate explicit refusal")
			for _, schedule := range cells {
				require.Equal(t, 100, schedule.Cell.Offers)
				require.Len(t, schedule.Arrival, 100)
				require.NotEqual(t, "relocate", schedule.Cell.Kind)
				trial, err := readbackReferenceTrialFor(schedule.Cell, schedule.Arrival[0], 0)
				require.NoError(t, err)
				ext := map[string]string{"typescript": ".ts", "python": ".py"}[language]
				require.Equal(t, ext, filepath.Ext(trial.ProbeFile))
				image := trial.After[trial.ProbeFile]
				require.NotEmpty(t, image.Symbols)
				require.Contains(t, image.Symbols, "GortexReadback000Stable")
				if schedule.Cell.Kind == "delete" {
					require.NotContains(t, image.Symbols, "GortexReadback000")
				} else {
					require.Contains(t, image.Symbols, trial.Target)
				}
			}
		})
	}
}
