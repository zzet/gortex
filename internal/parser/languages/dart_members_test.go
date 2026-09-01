package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// Instance, static, inferred and multi-name field declarations in a class,
// a mixin and an extension body all become member nodes qualified by their
// owning type.
func TestDartExtractor_Fields(t *testing.T) {
	src := []byte(`class Account {
  /// The account handle.
  final String id;
  int balance = 0;
  var scratch = 1;
  String? nickname, alias;
  static final Logger log = Logger();
  static const int maxRetries = 3;

  void deposit(int n) {}
}

mixin Auditable {
  DateTime? auditedAt;
}

extension Limits on String {
  static const int cap = 8;
}
`)
	res, err := NewDartExtractor().Extract("account.dart", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}

	for _, tc := range []struct {
		id        string
		kind      graph.NodeKind
		name      string
		receiver  string
		fieldType string
		static    bool
	}{
		{"account.dart::Account.id", graph.KindField, "id", "Account", "String", false},
		{"account.dart::Account.balance", graph.KindField, "balance", "Account", "int", false},
		{"account.dart::Account.scratch", graph.KindField, "scratch", "Account", "", false},
		{"account.dart::Account.nickname", graph.KindField, "nickname", "Account", "String?", false},
		{"account.dart::Account.alias", graph.KindField, "alias", "Account", "String?", false},
		{"account.dart::Account.log", graph.KindField, "log", "Account", "Logger", true},
		{"account.dart::Account.maxRetries", graph.KindConstant, "maxRetries", "Account", "int", true},
		{"account.dart::Auditable.auditedAt", graph.KindField, "auditedAt", "Auditable", "DateTime?", false},
		{"account.dart::Limits.cap", graph.KindConstant, "cap", "Limits", "int", true},
	} {
		t.Run(tc.id, func(t *testing.T) {
			n := byID[tc.id]
			require.NotNil(t, n, "field %s must be emitted", tc.id)
			assert.Equal(t, tc.kind, n.Kind)
			assert.Equal(t, tc.name, n.Name)
			assert.Equal(t, tc.receiver, n.Meta["receiver"])
			if tc.fieldType == "" {
				assert.NotContains(t, n.Meta, "field_type", "an inferred binding carries no declared type")
			} else {
				assert.Equal(t, tc.fieldType, n.Meta["field_type"])
			}
			if tc.static {
				assert.Equal(t, true, n.Meta["static"])
			} else {
				assert.NotContains(t, n.Meta, "static")
			}
		})
	}

	assert.Equal(t, "The account handle.", byID["account.dart::Account.id"].Meta["doc"])
	assert.Equal(t, VisibilityPublic, byID["account.dart::Account.id"].Meta["visibility"])

	// Every field is member_of its owning type and defined by the file.
	memberOf := map[string]string{}
	defines := map[string]bool{}
	for _, e := range res.Edges {
		switch e.Kind {
		case graph.EdgeMemberOf:
			memberOf[e.From] = e.To
		case graph.EdgeDefines:
			defines[e.To] = true
		}
	}
	assert.Equal(t, "account.dart::Account", memberOf["account.dart::Account.id"])
	assert.Equal(t, "account.dart::Auditable", memberOf["account.dart::Auditable.auditedAt"])
	assert.Equal(t, "account.dart::Limits", memberOf["account.dart::Limits.cap"])
	assert.True(t, defines["account.dart::Account.balance"])

	// A field must not displace the method extractor's members.
	require.NotNil(t, byID["account.dart::Account.deposit"])
	assert.Equal(t, graph.KindMethod, byID["account.dart::Account.deposit"].Kind)

	ids := map[string]bool{}
	for _, n := range res.Nodes {
		require.False(t, ids[n.ID], "duplicate node id %s", n.ID)
		ids[n.ID] = true
	}
}

// A top-level variable stays a file-scope symbol — the field pass must not
// re-qualify it under a type.
func TestDartExtractor_TopLevelVariablesUnchanged(t *testing.T) {
	src := []byte(`const version = '1.0.0';
final int retries = 2;

class Holder {
  final int retries = 3;
}
`)
	res, err := NewDartExtractor().Extract("cfg.dart", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}
	require.NotNil(t, byID["cfg.dart::version"])
	require.NotNil(t, byID["cfg.dart::retries"])
	assert.Equal(t, graph.KindVariable, byID["cfg.dart::retries"].Kind)

	holder := byID["cfg.dart::Holder.retries"]
	require.NotNil(t, holder, "the class field is a distinct symbol from the top-level one")
	assert.Equal(t, "Holder", holder.Meta["receiver"])
}

// Enum constants — plain and enhanced (argument-bearing) — are members of the
// enum type.
func TestDartExtractor_EnumConstants(t *testing.T) {
	src := []byte(`enum Status { pending, active, closed }

enum Planet {
  mercury(3.3e23),
  venus(4.87e24);

  const Planet(this.mass);
  final double mass;
}
`)
	res, err := NewDartExtractor().Extract("status.dart", src)
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}

	for _, id := range []string{
		"status.dart::Status.pending",
		"status.dart::Status.active",
		"status.dart::Status.closed",
		"status.dart::Planet.mercury",
		"status.dart::Planet.venus",
	} {
		n := byID[id]
		require.NotNil(t, n, "enum constant %s must be emitted", id)
		assert.Equal(t, graph.KindEnumMember, n.Kind)
	}

	mercury := byID["status.dart::Planet.mercury"]
	assert.Equal(t, "mercury", mercury.Name, "the argument list is not part of the constant name")
	assert.Equal(t, "Planet", mercury.Meta["receiver"])
	assert.Equal(t, "status.dart::Planet", mercury.Meta["enum"])

	// An enhanced enum's field is a field, not a constant of the enum.
	mass := byID["status.dart::Planet.mass"]
	require.NotNil(t, mass)
	assert.Equal(t, graph.KindField, mass.Kind)

	var linked bool
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeMemberOf && e.From == "status.dart::Status.active" && e.To == "status.dart::Status" {
			linked = true
		}
	}
	assert.True(t, linked, "an enum constant is member_of its enum")
}

// A setter is named by the property it writes, not by the `set foo` source
// spelling, so a search for the property name reaches it. The getter of the
// same property keeps the unsuffixed id.
func TestDartExtractor_AccessorNames(t *testing.T) {
	src := []byte(`class Temp {
  double _c = 0;

  double get celsius => _c;
  set celsius(double v) { _c = v; }

  set fahrenheit(double v) { _c = (v - 32) / 1.8; }
}
`)
	res, err := NewDartExtractor().Extract("temp.dart", src)
	require.NoError(t, err)

	byName := map[string][]*graph.Node{}
	for _, n := range res.Nodes {
		byName[n.Name] = append(byName[n.Name], n)
	}

	assert.Empty(t, byName["set celsius"], "a setter must not carry the `set ` prefix in its name")
	require.Len(t, byName["celsius"], 2, "the getter and the setter both answer to the property name")

	var getter, setter *graph.Node
	for _, n := range byName["celsius"] {
		switch n.Meta["accessor"] {
		case "getter":
			getter = n
		case "setter":
			setter = n
		}
	}
	require.NotNil(t, getter, "the getter is tagged accessor=getter")
	require.NotNil(t, setter, "the setter is tagged accessor=setter")
	assert.Equal(t, "temp.dart::Temp.celsius", getter.ID, "the getter keeps the unsuffixed id")
	assert.NotEqual(t, getter.ID, setter.ID, "the colliding accessor gets a line-suffixed id")
	assert.Equal(t, graph.KindMethod, setter.Kind)
	assert.Equal(t, "Temp", setter.Meta["receiver"])

	require.Len(t, byName["fahrenheit"], 1, "a setter with no getter takes the plain id")
	assert.Equal(t, "temp.dart::Temp.fahrenheit", byName["fahrenheit"][0].ID)

	// An ordinary method carries no accessor tag.
	for _, n := range res.Nodes {
		if n.Name == "Temp" {
			assert.NotContains(t, n.Meta, "accessor")
		}
	}
}
