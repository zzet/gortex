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

// Each adjacent pair uses the identical request; its order alternates. Fixture
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
	pair := 0
	for range repeats {
		for _, probe := range probes {
			order := []int{0, 512}
			if pair%2 != 0 {
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
	require.Equal(t, []int{0, 512, 512, 0, 0, 512, 512, 0}, calls)
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
