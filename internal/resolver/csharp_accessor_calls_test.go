package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Round-23 catch AC1, end to end: calls from property accessor bodies
// used to vanish at the extractor's owner gate, so nothing here could
// resolve at all. Resolution to a UNIQUE member name proves the whole
// pipeline: the edge exists, survives every demotion pass with a
// property-node caller, and binds. (Cross-TYPE name ambiguity is
// refused by the static tier for method callers too - typed
// disambiguation is the value-parameter pin's subject below.)
func TestResolveCSharp_AccessorBodyCallsResolve(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Gauge.cs": `namespace App {
    public class AcCrank { public int Turn() { return 1; } }
    public class AcGauge {
        private readonly AcCrank _crank = new AcCrank();
        private int _stored;
        public int Reading {
            get { return _crank.Turn(); }
            set { _stored = value + _crank.Turn(); }
        }
        public int Snapshot => _crank.Turn();
        public int Tally() => _crank.Turn();
    }
}`,
	})
	New(g).ResolveAll()

	fromProp := 0
	for _, to := range callsFrom(g, "Gauge.cs::AcGauge.Reading") {
		if to == "Gauge.cs::AcCrank.Turn" {
			fromProp++
		}
	}
	assert.Equal(t, 2, fromProp,
		"the get and set bodies each call Turn through the Crank-typed field")

	for _, row := range []struct{ owner, want string }{
		{"Gauge.cs::AcGauge.Snapshot", "Gauge.cs::AcCrank.Turn"},
		{"Gauge.cs::AcGauge.Tally", "Gauge.cs::AcCrank.Turn"},
	} {
		resolved := false
		for _, to := range callsFrom(g, row.owner) {
			if to == row.want {
				resolved = true
			}
		}
		assert.True(t, resolved, "%s must resolve its Turn call to AcCrank", row.owner)
	}
}

// The implicit `value` parameter is typed by the property declaration,
// so a call on it resolves like a call on any typed local - to the
// property type's method, not the decoy's.
func TestResolveCSharp_SetterValueReceiverResolves(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Sink.cs": `namespace App {
    public class VpCrank { public int Turn() { return 1; } }
    public class VpDecoy { public int Turn() { return 9; } }
    public class VpSink {
        public VpCrank Feed {
            set { _ = value.Turn(); }
        }
    }
}`,
	})
	New(g).ResolveAll()

	resolved := false
	for _, to := range callsFrom(g, "Sink.cs::VpSink.Feed") {
		if to == "Sink.cs::VpCrank.Turn" {
			resolved = true
		}
	}
	assert.True(t, resolved,
		"value is declared VpCrank by the property - its Turn call must resolve there")
}

// A field initializer call resolves from the field node that owns it.
func TestResolveCSharp_FieldInitializerCallResolves(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Config.cs": `namespace App {
    public class FiSeeder { public static int Prime() { return 7; } }
    public class FiConfig {
        private int _seed = FiSeeder.Prime();
    }
}`,
	})
	New(g).ResolveAll()

	resolved := false
	for _, to := range callsFrom(g, "Config.cs::FiConfig._seed") {
		if to == "Config.cs::FiSeeder.Prime" {
			resolved = true
		}
	}
	assert.True(t, resolved,
		"the initializer's static-form call must resolve from the field node")
}
