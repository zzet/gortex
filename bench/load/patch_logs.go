package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"
)

// Patch counters are process-interval activity, not exclusive edit attribution.
// Only the enclosing patch message is selected; summing its nested phase logs
// would double count. Raw events and boundary exclusions remain reviewable.
type patchWriteEvidence struct {
	Window          measurementWindow `json:"window"`
	Method          string            `json:"method"`
	Acknowledged    int               `json:"acknowledged_edits"`
	Visible         int               `json:"visible_edits"`
	Contained       int               `json:"inferred_contained_patch_intervals"`
	Boundary        int               `json:"inferred_boundary_patch_intervals"`
	Overlapping     int               `json:"overlapping_inferred_contained_intervals"`
	Invalid         int               `json:"invalid_patch_records"`
	DuplicateCopies int               `json:"duplicate_log_copies"`
	Totals          map[string]Metric `json:"summed_interval_counters"`
	PerAcknowledged map[string]Metric `json:"amortized_per_acknowledged_edit"`
	Events          []patchWriteEvent `json:"events"`
}

type patchWriteEvent struct {
	Source   string            `json:"source"`
	Started  time.Time         `json:"inferred_started"`
	Ended    time.Time         `json:"logged_end"`
	File     string            `json:"file"`
	Kind     string            `json:"kind"`
	Class    string            `json:"delta_class"`
	Relation string            `json:"window_relation"`
	WALReset *bool             `json:"wal_bytes_reset"`
	Counters map[string]Metric `json:"counters"`
}

var patchCounterUnits = map[string]string{
	"wal_bytes": "bytes", "main_bytes": "bytes", "rows": "rows", "txns": "transactions", "incident_edges": "edges",
	"node_rows": "rows", "edge_rows": "rows", "symbol_fts_rows": "rows", "content_fts_rows": "rows",
}

func collectPatchWrites(artifacts map[string]string, phase *Phase) error {
	evidence := patchWriteEvidence{
		Window: measurementWindow{Started: phase.Started, Ended: phase.Started.Add(time.Duration(phase.DurationSec * float64(time.Second))), Method: "declared workload phase window"},
		Method: "sum only top-level graph patch complete records whose inferred intervals fit the workload window; starts are inferred from log ts minus ms, but ms is captured before final handling/counter capture, so actual counter starts and boundary attribution are uncertain; log timestamp formatting also introduces skew; overlapping intervals can count the same process writes repeatedly; boundary/outside records are retained but excluded; log sampling/rotation can omit records; totals are not exclusive per-edit costs or a reconciliation to whole-process I/O meters",
		Totals: make(map[string]Metric), PerAcknowledged: make(map[string]Metric),
	}
	for _, edit := range phase.Edits {
		if edit.Outcome != "edit_error" {
			evidence.Acknowledged++
		}
		if edit.VisibilityMS.Value != nil {
			evidence.Visible++
		}
	}
	keys := make([]string, 0, len(artifacts))
	for name := range artifacts {
		if strings.HasPrefix(name, "daemon") && strings.HasSuffix(name, ".log") {
			keys = append(keys, name)
		}
	}
	slices.Sort(keys)
	seenSource := make(map[string]string)
	for _, name := range keys {
		path := artifacts[name]
		if strings.HasPrefix(path, "unavailable:") {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), maxWireBytes)
		for scanner.Scan() {
			line := scanner.Text()
			var header struct {
				Message string `json:"msg"`
			}
			if err := json.Unmarshal([]byte(line), &header); err != nil {
				if strings.Contains(line, "graph patch complete") {
					evidence.Invalid++
				}
				continue
			}
			if header.Message != "graph patch complete" {
				continue
			}
			// A logger can tee one exact record to stderr and a file. Do not
			// deduplicate repeated records within the same source log.
			if source, exists := seenSource[line]; exists && source != name {
				evidence.DuplicateCopies++
				continue
			}
			seenSource[line] = name
			event, err := parsePatchWrite([]byte(line), name)
			if err != nil {
				evidence.Invalid++
				continue
			}
			event.Relation = "outside"
			if !event.Started.Before(evidence.Window.Started) && !event.Ended.After(evidence.Window.Ended) {
				event.Relation = "inferred_contained"
				evidence.Contained++
			} else if event.Ended.After(evidence.Window.Started) && event.Started.Before(evidence.Window.Ended) {
				event.Relation = "inferred_boundary"
				evidence.Boundary++
			}
			evidence.Events = append(evidence.Events, event)
		}
		if err := errors.Join(scanner.Err(), file.Close()); err != nil {
			return err
		}
	}
	slices.SortStableFunc(evidence.Events, func(a, b patchWriteEvent) int { return a.Started.Compare(b.Started) })
	var previousEnd time.Time
	for _, event := range evidence.Events {
		if event.Relation != "inferred_contained" {
			continue
		}
		if event.Started.Before(previousEnd) {
			evidence.Overlapping++
		}
		if event.Ended.After(previousEnd) {
			previousEnd = event.Ended
		}
	}
	for key, unit := range patchCounterUnits {
		total := unavailable(unit, "no complete in-window patch counter observations")
		var sum float64
		missing := evidence.Contained == 0 || evidence.Invalid > 0
		for _, event := range evidence.Events {
			if event.Relation == "inferred_contained" {
				value := event.Counters[key]
				if value.Value == nil || (key == "wal_bytes" && (event.WALReset == nil || *event.WALReset)) {
					missing = true
				} else {
					sum += *value.Value
				}
			}
		}
		if !missing {
			total = measured(sum, unit)
		}
		evidence.Totals[key] = total
		perEdit := unavailable(unit+"/acknowledged edit", "no acknowledged edits or complete interval counter observations")
		if total.Value != nil && evidence.Acknowledged > 0 {
			perEdit = measured(*total.Value/float64(evidence.Acknowledged), unit+"/acknowledged edit")
		}
		evidence.PerAcknowledged[key] = perEdit
	}
	data, err := json.Marshal(evidence)
	if err == nil {
		phase.Gate["patch_write_intervals"] = data
	}
	return err
}

func parsePatchWrite(line []byte, source string) (patchWriteEvent, error) {
	var raw struct {
		Time     json.RawMessage `json:"ts"`
		Millis   *int64          `json:"ms"`
		File     string          `json:"file"`
		Kind     string          `json:"kind"`
		Class    string          `json:"delta_class"`
		WALReset *bool           `json:"wal_bytes_reset"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return patchWriteEvent{}, err
	}
	var seconds float64
	var ended time.Time
	if err := json.Unmarshal(raw.Time, &seconds); err == nil && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds > 0 && seconds < 1e11 {
		ended = time.Unix(int64(seconds), int64((seconds-math.Floor(seconds))*1e9)).UTC()
	} else if err := json.Unmarshal(raw.Time, &ended); err != nil || ended.IsZero() {
		return patchWriteEvent{}, errors.New("patch timestamp missing/invalid")
	}
	if raw.Millis == nil || *raw.Millis < 0 || *raw.Millis > int64((24*time.Hour)/time.Millisecond) {
		return patchWriteEvent{}, errors.New("patch duration missing/invalid")
	}
	event := patchWriteEvent{Source: source, Ended: ended, Started: ended.Add(-time.Duration(*raw.Millis) * time.Millisecond), File: raw.File, Kind: raw.Kind, Class: raw.Class, WALReset: raw.WALReset, Counters: make(map[string]Metric)}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return event, err
	}
	for key, unit := range patchCounterUnits {
		value := unavailable(unit, "counter absent/invalid in daemon log version")
		var number *int64
		if err := json.Unmarshal(fields[key], &number); err == nil && number != nil && *number >= 0 {
			value = measured(float64(*number), unit)
		}
		event.Counters[key] = value
	}
	if event.File == "" {
		return event, fmt.Errorf("patch source file absent in %s", source)
	}
	return event, nil
}
