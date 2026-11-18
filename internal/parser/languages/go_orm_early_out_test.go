package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestGoORM_TableNameOverrideWithoutModel(t *testing.T) {
	res := extractGoORM(t, `package model
type Plain struct{}
func (*Plain) TableName() string { return "plain_override" }
`)
	assert.Empty(t, res.edgesByKind[graph.EdgeModelsTable])
	assert.Nil(t, res.nodesByID["db::orm::plain_override"], "an override alone must not create a table")
}

func TestGoORM_TableNameOverridePreservesUnrelatedTable(t *testing.T) {
	res := extractGoORM(t, `package model
type Customer struct { ID uint `+"`gorm:\"primaryKey\"`"+` }
type Plain struct{}
func (*Customer) TableName() string { return "buyers" }
func (Plain) TableName() string { return "plain_override" }
`)
	models := res.edgesByKind[graph.EdgeModelsTable]
	require.Len(t, models, 1)
	assert.Equal(t, "db::orm::buyers", models[0].To)
	assert.Equal(t, "override", models[0].Meta["derivation"])
	// Existing output includes every literal override table when any model
	// edge exists, even if that override has no matching model edge.
	require.NotNil(t, res.nodesByID["db::orm::plain_override"])
}
