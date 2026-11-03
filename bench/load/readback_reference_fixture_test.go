package main

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestReadbackReferenceEveryRequiredKindHasIndependentBeforeAfterImage(t *testing.T) {
	require.Len(t, readbackReferenceTools, 9)
	for kind, tools := range readbackReferenceTools {
		t.Run(kind, func(t *testing.T) {
			cell := readbackReferenceCell{Agents: 8, Kind: kind, Tool: tools[0], Offers: 100}
			trial, err := readbackReferenceTrialFor(cell, editProbeArrival{Editor: 3, DueNS: 15000000000}, 7)
			require.NoError(t, err)
			require.Equal(t, 3, trial.Editor)
			require.Equal(t, int64(15000000000), trial.DueNS)
			require.Equal(t, tools[0], trial.Request.name())
			require.Len(t, readbackReferenceProbeRequests(trial), 5)
			image := trial.After[trial.ProbeFile]
			_, exists := image.Symbols[trial.Target]
			require.Equal(t, !trial.Absent, exists)
			require.NotEmpty(t, image.Symbols)
			changed := 0
			for file, before := range trial.Before {
				if before.Bytes != trial.After[file].Bytes {
					changed++
				}
			}
			require.Positive(t, changed)
			if kind == "modify_body" || kind == "modify_caller_body" {
				before := trial.Before[trial.ProbeFile]
				name := trial.Target
				require.Greater(t, image.Symbols[name].EndLine-image.Symbols[name].StartLine, before.Symbols[name].EndLine-before.Symbols[name].StartLine)
			}
			if kind == "relocate" {
				require.Equal(t, 2, changed)
				require.NotContains(t, trial.After[strings.Replace(trial.ProbeFile, "/d007.go", "/p007.go", 1)].Symbols, trial.Target)
			}
		})
	}
}
