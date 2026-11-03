package mcp

import (
	"errors"
	"fmt"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

type projectionLatencyProbe struct {
	Tool  string `json:"tool"`
	Query string `json:"query"`
	ID    string `json:"expected_id"`
	File  string `json:"expected_file"`
	Line  int    `json:"expected_line"`
}

type projectionLatencySample struct {
	Pair      int                    `json:"pair"`
	Entries   int                    `json:"entries"`
	Probe     projectionLatencyProbe `json:"probe"`
	ElapsedNS *int64                 `json:"elapsed_ns"`
	Error     string                 `json:"error,omitempty"`
}

// Each adjacent pair uses the identical request; population order rotates within
// each tool and reverses for that probe on the next repetition. Fixture
// setup, population switching and validation sit outside the timed call. The
// call includes per-request deadline/request assembly, registered-tool lookup
// and the public handler equally for both populations. Every refusal, error
// and wrong answer retains its offered sample.
func sampleProjectionHandlers(probes []projectionLatencyProbe, repeats int, selectPopulation func(int) (func() error, error), call func(int, projectionLatencyProbe) (*mcplib.CallToolResult, error), validate func(projectionLatencyProbe, *mcplib.CallToolResult) error, emitters ...func(projectionLatencySample) error) ([]projectionLatencySample, error) {
	if len(probes) == 0 || repeats < 1 {
		return nil, errors.New("matched projection comparison requires probes and positive repetitions")
	}
	var samples []projectionLatencySample
	var failures []error
	// Keep tool position separate from the alternating tool list. Using the
	// global pair parity fixes every symbol at 0-first and every text at
	// 512-first. Repetition parity also balances each repeated query.
	withinTool := make(map[string]int)
	probeOrdinals := make([]int, len(probes))
	for i, probe := range probes {
		probeOrdinals[i] = withinTool[probe.Tool]
		withinTool[probe.Tool]++
	}
	pair := 0
	for repetition := range repeats {
		for probeIndex, probe := range probes {
			order := []int{0, 512}
			if (repetition+probeOrdinals[probeIndex])%2 != 0 {
				order[0], order[1] = order[1], order[0]
			}
			for _, entries := range order {
				sample := projectionLatencySample{Pair: pair, Entries: entries, Probe: probe}
				verify, err := selectPopulation(entries)
				if err != nil || verify == nil {
					if err == nil {
						err = errors.New("missing population verification")
					}
					sample.Error = "population unavailable: " + err.Error()
				} else {
					start := time.Now()
					result, err := call(entries, probe)
					end := time.Now()
					elapsed := end.Sub(start).Nanoseconds()
					sample.ElapsedNS = &elapsed
					switch {
					case err != nil:
						sample.Error = err.Error()
					case result == nil:
						sample.Error = "missing handler response"
					case result.IsError:
						sample.Error = "handler refused the request"
					default:
						if err := validate(probe, result); err != nil {
							sample.Error = err.Error()
						}
					}
					if err := verify(); err != nil {
						sample.Error = "population changed: " + err.Error()
					}
					if elapsed < 0 {
						sample.Error = "invalid handler clock order"
					}
				}
				if sample.Error != "" {
					failures = append(failures, fmt.Errorf("pair%d population%d: %s", pair, entries, sample.Error))
				}
				for _, emit := range emitters {
					if err := emit(sample); err != nil {
						failures = append(failures, err)
					}
				}
				samples = append(samples, sample)
			}
			pair++
		}
	}
	return samples, errors.Join(failures...)
}

func TestProjectionHandlerComparisonKeepsMatchedOrderAndCompleteSamples(t *testing.T) {
	probes := []projectionLatencyProbe{{Tool: "search_symbols", Query: "One"}, {Tool: "search_text", Query: "Two"}}
	var calls []int
	samples, err := sampleProjectionHandlers(probes, 2, func(int) (func() error, error) { return func() error { return nil }, nil }, func(entries int, _ projectionLatencyProbe) (*mcplib.CallToolResult, error) {
		calls = append(calls, entries)
		return &mcplib.CallToolResult{}, nil
	}, func(projectionLatencyProbe, *mcplib.CallToolResult) error { return nil })
	require.NoError(t, err)
	require.Equal(t, []int{0, 512, 0, 512, 512, 0, 512, 0}, calls)
	require.Len(t, samples, 8)
	for pair := range 4 {
		require.Equal(t, samples[pair*2].Probe, samples[pair*2+1].Probe)
		require.NotEqual(t, samples[pair*2].Entries, samples[pair*2+1].Entries)
	}
}

func TestProjectionHandlerComparisonNeverDropsRefusedWrongOrMissingSamples(t *testing.T) {
	for _, fault := range []string{"population", "changed_population", "call", "missing", "refusal", "wrong"} {
		t.Run(fault, func(t *testing.T) {
			samples, err := sampleProjectionHandlers([]projectionLatencyProbe{{Tool: "search_symbols", Query: "One"}}, 2, func(int) (func() error, error) {
				if fault == "population" {
					return nil, errors.New("only511 entries")
				}
				return func() error {
					if fault == "changed_population" {
						return errors.New("entry retired during handler")
					}
					return nil
				}, nil
			}, func(int, projectionLatencyProbe) (*mcplib.CallToolResult, error) {
				switch fault {
				case "call":
					return nil, errors.New("call failed")
				case "missing":
					return nil, nil
				case "refusal":
					return &mcplib.CallToolResult{IsError: true}, nil
				}
				return &mcplib.CallToolResult{}, nil
			}, func(projectionLatencyProbe, *mcplib.CallToolResult) error {
				if fault == "wrong" {
					return errors.New("wrong symbol/line")
				}
				return nil
			})
			require.Error(t, err)
			require.Len(t, samples, 4, "failure must retain both populations and both repetitions")
			for _, sample := range samples {
				require.NotEmpty(t, sample.Error)
			}
		})
	}
}

func TestProjectionHandlerScheduleBalancesOrderWithinEveryTool(t *testing.T) {
	var probes []projectionLatencyProbe
	for i := range 20 {
		for _, tool := range []string{"search_symbols", "search_text"} {
			probes = append(probes, projectionLatencyProbe{Tool: tool, Query: fmt.Sprintf("probe-%02d", i), ID: fmt.Sprintf("file%02d.go::Probe", i), File: fmt.Sprintf("file%02d.go", i), Line: i + 1})
		}
	}
	for _, cell := range []struct {
		name                        string
		repeats, records, eachOrder int
	}{{"warmup", 1, 80, 10}, {"measured", 50, 4000, 500}} {
		t.Run(cell.name, func(t *testing.T) {
			var emitted []projectionLatencySample
			samples, err := sampleProjectionHandlers(probes, cell.repeats, func(int) (func() error, error) { return func() error { return nil }, nil }, func(int, projectionLatencyProbe) (*mcplib.CallToolResult, error) {
				return &mcplib.CallToolResult{}, nil
			}, func(projectionLatencyProbe, *mcplib.CallToolResult) error { return nil }, func(sample projectionLatencySample) error { emitted = append(emitted, sample); return nil })
			require.NoError(t, err)
			require.Len(t, samples, cell.records)
			require.Equal(t, samples, emitted, "raw emission retains every offered sample")
			firstCounts := map[string]map[int]int{}
			perProbeFirstCounts := map[string]map[int]int{}
			perProbePopulation := map[string]map[int]int{}
			for pair := range len(samples) / 2 {
				first, second := samples[2*pair], samples[2*pair+1]
				require.Equal(t, pair, first.Pair)
				require.Equal(t, pair, second.Pair)
				require.Equal(t, probes[pair%len(probes)], first.Probe, "identical probe order and offered denominator")
				require.Equal(t, first.Probe, second.Probe)
				require.NotEqual(t, first.Entries, second.Entries)
				tool := first.Probe.Tool
				if firstCounts[tool] == nil {
					firstCounts[tool] = map[int]int{}
				}
				firstCounts[tool][first.Entries]++
				firstKey := tool + "/" + first.Probe.Query
				if perProbeFirstCounts[firstKey] == nil {
					perProbeFirstCounts[firstKey] = map[int]int{}
				}
				perProbeFirstCounts[firstKey][first.Entries]++
				for _, sample := range []projectionLatencySample{first, second} {
					require.Empty(t, sample.Error)
					require.NotNil(t, sample.ElapsedNS)
					require.GreaterOrEqual(t, *sample.ElapsedNS, int64(0))
					key := sample.Probe.Tool + "/" + sample.Probe.Query
					if perProbePopulation[key] == nil {
						perProbePopulation[key] = map[int]int{}
					}
					perProbePopulation[key][sample.Entries]++
				}
			}
			for _, tool := range []string{"search_symbols", "search_text"} {
				require.Equal(t, cell.eachOrder, firstCounts[tool][0])
				require.Equal(t, cell.eachOrder, firstCounts[tool][512])
			}
			require.Len(t, perProbePopulation, len(probes))
			for _, counts := range perProbePopulation {
				require.Equal(t, cell.repeats, counts[0])
				require.Equal(t, cell.repeats, counts[512])
			}
			if cell.repeats%2 == 0 {
				for _, counts := range perProbeFirstCounts {
					require.Equal(t, cell.repeats/2, counts[0], "each repeated query must exercise both orders")
					require.Equal(t, cell.repeats/2, counts[512])
				}
			}
		})
	}
}
