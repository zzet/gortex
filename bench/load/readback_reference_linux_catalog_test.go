//go:build acceptance

package main

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
)

var readbackReferenceLinuxCatalogOutput = flag.String("readback-reference-linux-catalog-out", "", "fresh metadata-only schedule catalog output; no daemon or corpus read")

func TestReadbackReferenceLinuxScheduleCatalog(t *testing.T) {
	if *readbackReferenceLinuxCatalogOutput == "" {
		t.Skip("no explicit catalog output: no metadata file or runtime launched")
	}
	catalog, err := readbackReferenceLinuxLaunchCatalog(readbackReferencePlan{Version: 1, Seed: 1, EditMeanNS: 15000000000, ProductDeadlineSeconds: 120, WarmSeconds: 1800, StopSeconds: 30, ToolSurface: readbackReferenceCompatibilitySurface})
	require.NoError(t, err)
	require.NoError(t, readbackReferenceWriteJSON(*readbackReferenceLinuxCatalogOutput, catalog))
}
