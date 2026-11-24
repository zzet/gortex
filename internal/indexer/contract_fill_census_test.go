package indexer

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContractFillCensusMemoAcrossUnchangedObservations(t *testing.T) {
	initial := map[string]bool{
		"repo/a.go": true,
		"repo/b.go": true,
		"repo/c.go": true,
		"repo/d.go": true,
	}
	var c contractConvergence
	c.clock.observe(nil, initial)
	same := map[string]bool{"repo/d.go": true, "repo/c.go": true, "repo/b.go": true, "repo/a.go": true}
	paths := []string{"repo/b.go"}
	stamps := map[string]ContractReceiptStamp{"repo/b.go": {Fingerprint: "updated"}}
	want := contractFillCensus(initial)
	require.Equal(t, want, c.fillCensus())

	for _, observation := range []struct {
		name string
		call func()
	}{
		{"same membership in new full scan", func() { c.clock.observe(nil, same) }},
		{"point stamp changes", func() { c.clock.observePoint(paths, stamps) }},
	} {
		t.Run(observation.name, func(t *testing.T) {
			observationAllocs := testing.AllocsPerRun(10, observation.call)
			allocs := testing.AllocsPerRun(10, func() {
				observation.call()
				if c.fillCensus() != want {
					panic("stable census fingerprint changed")
				}
			})
			require.Equal(t, observationAllocs, allocs, "a warm lookup must not allocate census sort input")
		})
	}
}

func observeContractFillCensusJoin(c *contractConvergence, path string) {
	census := maps.Clone(c.clock.lastCensus)
	census[path] = true
	c.clock.observe(c.clock.last, census)
}

func TestContractFillCensusMemoTracksMembershipChurn(t *testing.T) {
	var c contractConvergence
	require.Equal(t, contractFillCensus(nil), c.fillCensus())
	stamp := map[string]ContractReceiptStamp{"repo/source.go": {Fingerprint: "unchanged"}}
	for _, row := range []struct {
		name   string
		census map[string]bool
		moves  bool
	}{
		{"empty scan", map[string]bool{}, false},
		{"false is absent", map[string]bool{"repo/source.go": false}, false},
		{"joins", map[string]bool{"repo/source.go": true}, true},
		{"unchanged independent scan", map[string]bool{"repo/source.go": true}, false},
		{"second path joins", map[string]bool{"repo/source.go": true, "repo/new.go": true}, true},
		{"marked absent", map[string]bool{"repo/source.go": false, "repo/new.go": true}, true},
		{"last file leaves", nil, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			before := c.clock.censusChangedAt
			c.clock.observe(stamp, row.census)
			require.Equal(t, row.moves, c.clock.censusChangedAt != before)
			require.Equal(t, contractFillCensus(row.census), c.fillCensus())
		})
	}

	c.clock.observe(stamp, map[string]bool{"repo/source.go": true})
	clone := contractConvergence{clock: cloneContractClock(c.clock)}
	original := c.fillCensus()
	observeContractFillCensusJoin(&clone, "repo/clone.go")
	require.Equal(t, original, c.fillCensus())
	require.NotEqual(t, original, clone.fillCensus())
}
