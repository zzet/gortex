package entrypoints

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// detectPythonFor runs the real Python extractor over src and then the
// detector, so the test exercises the decorator and import edges the
// extractor actually emits rather than hand-built ones.
func detectPythonFor(t *testing.T, relPath, src string) map[string]*graph.Node {
	t.Helper()
	e := languages.NewPythonExtractor()
	result, err := e.Extract(relPath, []byte(src))
	require.NoError(t, err)
	Detect(relPath, "python", result.Nodes, result.Edges)
	byName := map[string]*graph.Node{}
	for _, n := range result.Nodes {
		if isFnOrMethod(n.Kind) {
			byName[n.Name] = n
		}
	}
	return byName
}

func entryKind(n *graph.Node) any {
	if n == nil || n.Meta == nil {
		return nil
	}
	return n.Meta[MetaEntryKind]
}

// Pydantic invokes validators and serializers itself, so they never carry
// a written call site and every one of them read as dead code.
func TestDetectPydantic_V2Decorators(t *testing.T) {
	byName := detectPythonFor(t, "models.py", `from pydantic import BaseModel, computed_field, field_serializer, field_validator, model_validator


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

    @computed_field
    @property
    def doubled(self) -> float:
        return self.price * 2

    def _helper(self) -> None:
        pass
`)
	assert.Equal(t, "pydantic:validator", entryKind(byName["_positive"]))
	assert.Equal(t, "pydantic:validator", entryKind(byName["_consistent"]))
	assert.Equal(t, "pydantic:serializer", entryKind(byName["_money_out"]))
	assert.Equal(t, "pydantic:computed_field", entryKind(byName["doubled"]))
	assert.Nil(t, entryKind(byName["_helper"]), "an undecorated helper stays a dead-code candidate")
}

func TestDetectPydantic_V1DecoratorsAndModuleImport(t *testing.T) {
	byName := detectPythonFor(t, "models.py", `import pydantic


class Legacy(pydantic.BaseModel):
    name: str

    @pydantic.validator("name")
    def _strip(cls, value):
        return value.strip()

    @pydantic.root_validator
    def _whole(cls, values):
        return values
`)
	assert.Equal(t, "pydantic:validator", entryKind(byName["_strip"]))
	assert.Equal(t, "pydantic:validator", entryKind(byName["_whole"]))
}

func TestDetectPydantic_SubmoduleImport(t *testing.T) {
	byName := detectPythonFor(t, "models.py", `from pydantic.functional_validators import field_validator


class M:
    @field_validator("x")
    def _check(cls, value):
        return value
`)
	assert.Equal(t, "pydantic:validator", entryKind(byName["_check"]))
}

// `validator` is too generic a name to trust on its own: without a
// pydantic import the decorated method stays a dead-code candidate.
func TestDetectPydantic_RequiresPydanticImport(t *testing.T) {
	byName := detectPythonFor(t, "forms.py", `from mylib import validator


class Form:
    @validator("name")
    def _check(self, value):
        return value
`)
	assert.Nil(t, entryKind(byName["_check"]))
}

// A module named like pydantic is not pydantic.
func TestDetectPydantic_LookalikeModuleIsNotPydantic(t *testing.T) {
	byName := detectPythonFor(t, "forms.py", `from pydantic_extra_things import field_validator


class Form:
    @field_validator("name")
    def _check(self, value):
        return value
`)
	assert.Nil(t, entryKind(byName["_check"]))
}

// validate_call wraps a function that application code calls directly,
// so it is not an entry point.
func TestDetectPydantic_ValidateCallIsNotAnEntryPoint(t *testing.T) {
	byName := detectPythonFor(t, "svc.py", `from pydantic import validate_call


@validate_call
def charge(amount: int) -> int:
    return amount
`)
	assert.Nil(t, entryKind(byName["charge"]))
}
