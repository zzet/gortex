package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestIndex_AlembicEntryPoint indexes a real Alembic migration and
// verifies its upgrade() is flagged as an entry point and therefore
// not reported as dead code.
func TestIndex_AlembicEntryPoint(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "alembic", "versions"), 0o755))
	writeFile(t, filepath.Join(dir, "alembic", "versions", "001_init.py"), `revision = "001"
down_revision = None


def upgrade():
    pass


def downgrade():
    pass
`)
	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewPythonExtractor())
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	var upgrade *graph.Node
	for _, n := range g.AllNodes() {
		if n.Name == "upgrade" && n.Kind == graph.KindFunction {
			upgrade = n
		}
	}
	require.NotNil(t, upgrade, "upgrade() must be indexed")
	require.Equal(t, true, upgrade.Meta["entry_point"])
	require.Equal(t, "alembic:migration", upgrade.Meta["entry_point_kind"])

	// The migration's upgrade() has no in-app caller — without the
	// entry-point tag it would be a dead-code false positive.
	for _, d := range analysis.FindDeadCode(g, nil, nil) {
		require.NotEqual(t, "upgrade", d.Name, "Alembic upgrade() must not be flagged dead")
		require.NotEqual(t, "downgrade", d.Name, "Alembic downgrade() must not be flagged dead")
	}
}

// TestIndex_JavaDeadCodeAndEntryPoints is the end-to-end parity check:
// after indexing a Spring controller, dead-code must (a) flag an
// uncalled private method — which the old name-based heuristic could
// never do for Java — while (b) leaving public API and framework
// entry points alone.
func TestIndex_JavaDeadCodeAndEntryPoints(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "UserController.java"), `package com.example.web;

import org.springframework.web.bind.annotation.RestController;
import org.springframework.web.bind.annotation.GetMapping;

@RestController
public class UserController {

    @GetMapping("/users")
    public String listUsers() {
        return "ok";
    }

    private String deadHelper() {
        return "never called";
    }

    public static void main(String[] args) {
        new UserController();
    }
}
`)
	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewJavaExtractor())
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	byName := map[string]*graph.Node{}
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindMethod || n.Kind == graph.KindType {
			byName[n.Name] = n
		}
	}

	// Framework entry points are stamped by entrypoints.Detect during indexing.
	require.NotNil(t, byName["UserController"], "controller type must be indexed")
	require.Equal(t, true, byName["UserController"].Meta["entry_point"], "@RestController is an entry point")
	require.Equal(t, "spring:controller", byName["UserController"].Meta["entry_point_kind"])
	require.NotNil(t, byName["listUsers"], "handler must be indexed")
	require.Equal(t, true, byName["listUsers"].Meta["entry_point"], "@GetMapping handler is an entry point")

	dead := map[string]bool{}
	for _, d := range analysis.FindDeadCode(g, nil, nil) {
		dead[d.Name] = true
	}
	require.True(t, dead["deadHelper"], "an uncalled private Java method must now be reported dead")
	require.False(t, dead["listUsers"], "a public @GetMapping handler is not dead")
	require.False(t, dead["main"], "main is the JVM entry point, not dead")
}

// TestIndex_PydanticValidatorsNotDead is the end-to-end check for the
// Pydantic detector: validators and serializers are invoked by the model
// machinery, so they must not be reported dead, while an uncalled helper
// on the same model still is.
func TestIndex_PydanticValidatorsNotDead(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "models.py"), `from pydantic import BaseModel, field_serializer, field_validator, model_validator


class Offer(BaseModel):
    price: float

    @field_validator("price")
    @classmethod
    def _positive(cls, value: float) -> float:
        return value

    @model_validator(mode="after")
    def _consistent(self) -> "Offer":
        return self

    @field_serializer("price")
    def _money_out(self, value: float) -> str:
        return f"{value:.2f}"

    def _unused_helper(self) -> None:
        pass
`)
	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewPythonExtractor())
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	dead := map[string]bool{}
	for _, d := range analysis.FindDeadCode(g, nil, nil) {
		dead[d.Name] = true
	}
	require.False(t, dead["_positive"], "@field_validator is invoked by pydantic, not dead")
	require.False(t, dead["_consistent"], "@model_validator is invoked by pydantic, not dead")
	require.False(t, dead["_money_out"], "@field_serializer is invoked by pydantic, not dead")
	require.True(t, dead["_unused_helper"], "an uncalled helper on the model must still be reported dead")
}
