package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWorktreeStartupRacePreplanningFailureRegression(t *testing.T) {
	raw := json.RawMessage(preplanningActualStatus)
	parse := func(s string) time.Time {
		v, e := time.Parse(time.RFC3339Nano, s)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	if e := worktreeStartupRace(raw, "01a1ef66-c155-7941-9900-0c7ebac9a02a", parse("2026-11-18T17:04:10.0148005+01:00"), parse("2026-11-18T17:04:17.6792577+01:00"), parse("2026-11-18T17:04:18.0651729+01:00")); e != nil {
		t.Fatal(e)
	}
}

const preplanningActualStatus = `{"publication_phases":[{"key":"observed-4","checkout_id":"01a1ef66-c155-7941-9900-0c7ebac9a02a","source":"observed_change","origin_wall":"2026-11-18T16:04:14.4359763Z","clock":"monotonic","terminal":true,"phases":[{"phase":"received","offset_ns":0,"offset_ms":0},{"phase":"change_observed","offset_ns":406881700,"offset_ms":406.881},{"phase":"admitted","offset_ns":406881700,"offset_ms":406.881},{"phase":"ticket_failed","offset_ns":406881700,"offset_ms":406.881}]},{"key":"observed-5","checkout_id":"01a1ef66-c155-7941-9900-0c7ebac9a02a","source":"observed_change","origin_wall":"2026-11-18T16:04:14.9931176Z","clock":"monotonic","terminal":true,"phases":[{"phase":"received","offset_ns":0,"offset_ms":0},{"phase":"change_observed","offset_ns":394459100,"offset_ms":394.459},{"phase":"admitted","offset_ns":394459100,"offset_ms":394.459},{"phase":"planned","offset_ns":2623212400,"offset_ms":2623.212},{"phase":"extracted","offset_ns":7833621300,"offset_ms":7833.621},{"phase":"semantic_done","offset_ns":7905871200,"offset_ms":7905.871},{"phase":"payload_flushed","offset_ns":8614685700,"offset_ms":8614.685},{"phase":"published","offset_ns":8775500000,"offset_ms":8775.5},{"phase":"ticket_failed","offset_ns":9431856300,"offset_ms":9431.856}]},{"key":"observed-6","checkout_id":"01a1ef66-c155-7941-9900-0c7ebac9a02a","source":"observed_change","dirty_generation_id":8,"origin_wall":"2026-11-18T16:04:24.5751367Z","clock":"monotonic","terminal":true,"phases":[{"phase":"received","offset_ns":0,"offset_ms":0},{"phase":"change_observed","offset_ns":1153809100,"offset_ms":1153.809},{"phase":"admitted","offset_ns":1153809100,"offset_ms":1153.809},{"phase":"planned","offset_ns":3392312500,"offset_ms":3392.312},{"phase":"extracted","offset_ns":8939085400,"offset_ms":8939.085},{"phase":"semantic_done","offset_ns":9025066300,"offset_ms":9025.066},{"phase":"payload_flushed","offset_ns":9840885200,"offset_ms":9840.885},{"phase":"published","offset_ns":9930727800,"offset_ms":9930.727},{"phase":"route_flipped","offset_ns":10700860400,"offset_ms":10700.86},{"phase":"ticket_completed","offset_ns":10700860400,"offset_ms":10700.86}]}]}`

func TestWorktreeStartupRacePreplanningFailureRefusesAmbiguity(t *testing.T) {
	for _, name := range []string{"not terminal", "already planned", "published without plan", "failure after switch", "later plan", "unknown phase"} {
		t.Run(name, func(t *testing.T) {
			var body map[string]any
			if e := json.Unmarshal([]byte(preplanningActualStatus), &body); e != nil {
				t.Fatal(e)
			}
			records := body["publication_phases"].([]any)
			first := records[0].(map[string]any)
			phases := first["phases"].([]any)
			switch name {
			case "not terminal":
				first["terminal"] = false
			case "already planned":
				first["phases"] = append(phases, map[string]any{"phase": "planned", "offset_ns": 100000000})
			case "published without plan":
				first["phases"] = append(phases, map[string]any{"phase": "published", "offset_ns": 100000000})
			case "failure after switch":
				phases[len(phases)-1].(map[string]any)["offset_ns"] = 6000000000
			case "later plan":
				for _, p := range records[1].(map[string]any)["phases"].([]any) {
					m := p.(map[string]any)
					if m["phase"] == "planned" {
						m["offset_ns"] = 4000000000
					}
				}
			case "unknown phase":
				first["phases"] = append(phases, map[string]any{"phase": "unrecognized_stage", "offset_ns": 100000000})
			}
			raw, e := json.Marshal(body)
			if e != nil {
				t.Fatal(e)
			}
			parse := func(s string) time.Time {
				v, e := time.Parse(time.RFC3339Nano, s)
				if e != nil {
					t.Fatal(e)
				}
				return v
			}
			if e = worktreeStartupRace(raw, "01a1ef66-c155-7941-9900-0c7ebac9a02a", parse("2026-11-18T17:04:10.0148005+01:00"), parse("2026-11-18T17:04:17.6792577+01:00"), parse("2026-11-18T17:04:18.0651729+01:00")); e == nil {
				t.Fatal("ambiguous or missed initial build was accepted")
			}
		})
	}
}
