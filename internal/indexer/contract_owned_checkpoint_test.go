package indexer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
)

func TestContractOptionalCheckpointChargesBackingAndPreservesRawAtCap(t *testing.T) {
	raw := []contracts.Contract{{ID: "unchanged", Meta: map[string]any{"source": "raw-authority"}}}
	unit := &contractUnit{path: "r/f.go", raw: raw, ledger: newContractUnitLedger()}
	owned := strings.Repeat("owned-input-", 1024)
	unit.bodyBindings = map[contractBodyBindingQuestion]contractBodyBindingAnswer{{handler: "handler", name: "value"}: {source: "source", handler: "node", binding: contracts.Binding{RawExpr: owned}}}
	unit.providerSchemas = map[string]contractProviderSchemaAnswer{"provider": {input: contracts.Contract{Meta: map[string]any{"owned": owned}}, output: map[string]any{"owned": owned}}}
	p := &contractDeltaCache{units: map[string]*contractUnit{"r/f.go": unit}, names: &contractPositiveNameProjection{answers: map[string]contractCertifiedNameAnswer{"answer": {name: owned, digest: "digest"}}}}
	charge := contractOptionalCheckpointSize(p)
	headersOnly := contractRetainedSizeMode(reflect.ValueOf(p.names), false) + contractRetainedSizeMode(reflect.ValueOf(unit.bodyBindings), false) + contractRetainedSizeMode(reflect.ValueOf(unit.providerSchemas), false)
	require.Greater(t, charge, headersOnly+int64(3*len(owned)), "owned backing cannot disappear through the shared-pool size walk")
	base := int64(4096)
	p.retainedBytes = base + charge
	rawBefore := cloneContractRecords(raw)
	ledger := unit.ledger
	trimContractOptionalCheckpoint(p, base+128)
	require.Nil(t, p.names)
	require.Nil(t, unit.providerSchemas)
	require.Nil(t, unit.bodyBindings)
	require.Equal(t, base, p.retainedBytes)
	require.Equal(t, rawBefore, unit.raw, "optional admission cannot delete/reclassify raw contract work")
	require.Same(t, ledger, unit.ledger)
}

func TestContractOptionalCheckpointKeepsAdmittedValues(t *testing.T) {
	unit := &contractUnit{providerSchemas: map[string]contractProviderSchemaAnswer{"provider": {output: map[string]any{"schema_source": "none"}}}}
	p := &contractDeltaCache{units: map[string]*contractUnit{"r/f.go": unit}}
	p.retainedBytes = 4096 + contractOptionalCheckpointSize(p)
	before := p.retainedBytes
	trimContractOptionalCheckpoint(p, before)
	require.Equal(t, before, p.retainedBytes)
	require.NotEmpty(t, unit.providerSchemas)
}
