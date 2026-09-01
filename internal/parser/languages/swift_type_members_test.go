package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// init / deinit / subscript declared in a type body are members of that type.
func TestSwiftExtractor_InitDeinitSubscript(t *testing.T) {
	const swift = `final class Cache {
    private var store: [String: Int] = [:]

    /// Builds an empty cache.
    init() {
        self.store = [:]
    }

    init(seed: [String: Int]) {
        self.store = seed
    }

    deinit {
        store.removeAll()
    }

    subscript(key: String) -> Int? {
        return store[key]
    }
}
`
	res, err := NewSwiftExtractor().Extract("Cache.swift", []byte(swift))
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}

	ctor := byID["Cache.swift::Cache.<init>"]
	require.NotNil(t, ctor, "an initializer must be emitted")
	assert.Equal(t, graph.KindMethod, ctor.Kind)
	assert.Equal(t, "Cache.<init>", ctor.Name)
	assert.Equal(t, "Cache", ctor.Meta["receiver"])
	assert.Equal(t, "init", ctor.Meta["kind"])
	assert.Equal(t, "init()", ctor.Meta["signature"])
	assert.Equal(t, "Builds an empty cache.", ctor.Meta["doc"])

	require.NotNil(t, byID["Cache.swift::Cache.<init>_L9"],
		"a second initializer takes a line-suffixed id")

	dtor := byID["Cache.swift::Cache.deinit"]
	require.NotNil(t, dtor, "a deinitializer must be emitted")
	assert.Equal(t, "deinit", dtor.Name)
	assert.Equal(t, "deinit", dtor.Meta["kind"])

	sub := byID["Cache.swift::Cache.subscript"]
	require.NotNil(t, sub, "a subscript must be emitted")
	assert.Equal(t, "subscript", sub.Name)
	assert.Equal(t, "subscript(key: String) -> Int?", sub.Meta["signature"])

	memberOf := map[string]string{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeMemberOf {
			memberOf[e.From] = e.To
		}
	}
	for _, id := range []string{ctor.ID, dtor.ID, sub.ID} {
		assert.Equal(t, "Cache.swift::Cache", memberOf[id], "%s is member_of Cache", id)
	}

	// A body that used to have no enclosing symbol dropped its call edges.
	var removeAll bool
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeCalls && e.From == dtor.ID && e.To == "unresolved::*.removeAll" {
			removeAll = true
		}
	}
	assert.True(t, removeAll, "a call inside deinit attributes to the deinit node")
}

// Every member kind declared in an `extension Type { … }` block — method,
// computed and stored static property, initializer, subscript — qualifies
// under the extended type, including when the extension is the only
// declaration naming that type in the file.
func TestSwiftExtractor_ExtensionMemberKinds(t *testing.T) {
	const swift = `import Foundation

public struct Rule {
    let name: String
}

public extension Validator {
    public static var email: Validator {
        Validator(pattern: "@")
    }

    static let shared = Validator(pattern: "")

    var isEmpty: Bool { pattern.isEmpty }

    func check(_ s: String) -> Bool {
        return matches(s)
    }

    init(raw: String) {
        self.init(pattern: raw)
    }

    subscript(i: Int) -> Character {
        return pattern[i]
    }
}
`
	res, err := NewSwiftExtractor().Extract("Validator.swift", []byte(swift))
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}

	for _, tc := range []struct {
		id   string
		kind graph.NodeKind
	}{
		{"Validator.swift::Validator.email", graph.KindField},
		{"Validator.swift::Validator.shared", graph.KindField},
		{"Validator.swift::Validator.isEmpty", graph.KindField},
		{"Validator.swift::Validator.check", graph.KindMethod},
		{"Validator.swift::Validator.<init>", graph.KindMethod},
		{"Validator.swift::Validator.subscript", graph.KindMethod},
	} {
		t.Run(tc.id, func(t *testing.T) {
			n := byID[tc.id]
			require.NotNil(t, n, "extension member %s must be emitted", tc.id)
			assert.Equal(t, tc.kind, n.Kind)
			assert.Equal(t, "Validator", n.Meta["receiver"])
		})
	}

	assert.Equal(t, "Validator", byID["Validator.swift::Validator.email"].Meta["field_type"])
	assert.Equal(t, VisibilityPublic, byID["Validator.swift::Validator.email"].Meta["visibility"])

	// The struct in the same file keeps its own members.
	require.NotNil(t, byID["Validator.swift::Rule.name"])
	assert.Equal(t, "Rule", byID["Validator.swift::Rule.name"].Meta["receiver"])

	memberOf := map[string]string{}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeMemberOf {
			memberOf[e.From] = e.To
		}
	}
	assert.Equal(t, "Validator.swift::Validator", memberOf["Validator.swift::Validator.<init>"])
	assert.Equal(t, "Validator.swift::Validator", memberOf["Validator.swift::Validator.check"])

	ids := map[string]bool{}
	for _, n := range res.Nodes {
		require.False(t, ids[n.ID], "duplicate node id %s", n.ID)
		ids[n.ID] = true
	}
}

// A protocol's init requirement has no type body to attribute to and must not
// land on the file or on an unrelated type.
func TestSwiftExtractor_ProtocolInitRequirement(t *testing.T) {
	const swift = `protocol Buildable {
    init(raw: String)
}

struct Widget: Buildable {
    init(raw: String) {}
}
`
	res, err := NewSwiftExtractor().Extract("Buildable.swift", []byte(swift))
	require.NoError(t, err)

	byID := map[string]*graph.Node{}
	for _, n := range res.Nodes {
		byID[n.ID] = n
	}
	require.NotNil(t, byID["Buildable.swift::Widget.<init>"])
	assert.Nil(t, byID["Buildable.swift::Buildable.<init>"],
		"a protocol requirement declares no body to own")

	for _, n := range res.Nodes {
		if n.Kind == graph.KindMethod {
			assert.Equal(t, "Widget", n.Meta["receiver"])
		}
	}
}
