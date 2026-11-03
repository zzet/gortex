package main

import (
 "encoding/json"
 "testing"
 "time"
)

// Exact Windows run41612127031 values: mono span70.4689ms, wall
// span70.4677ms. The actual next probe begins at the phase endpoint.
func TestPREditClockWindowsJSONCounterexample(t *testing.T) {
 start,err:=time.Parse(time.RFC3339Nano,"2026-11-03T10:09:49.0343837Z");if err!=nil {t.Fatal(err)}
 finished:=start.Add(70467700*time.Nanosecond)
 phase:=Phase{Attempts:[]Attempt{
 {Agent:0,Step:0,Slot:0,Tool:"edit.file",Role:"workload",Started:start,DurationMS:70.4689},
 {Agent:0,Step:0,Slot:0,Tool:"search.symbols",Role:"probe",Started:finished,DurationMS:0,Outcome:"success"},
 },Edits:[]EditObservation{{Marker:"GortexLoadMarkerA0S0",Started:start,Outcome:"visible",VisibilityMS:measured(70.4689,"ms")}}}
 raw:=map[string]any{"schema_version":2,"started":start,"finished":finished,"offer_deadline":start.Add(30*time.Millisecond),"drain_deadline":start.Add(time.Second),"elapsed_seconds":.0704689,"admitted_calls":1,"admitted_edits":1,"visibility_probe_attempts":1,"admissions":[]prDrainAdmission{{Agent:0,Step:0,Slot:0,Tool:"edit.file",OfferedAt:start}},"edit_clock":map[string]any{"semantics":"live_monotonic_offsets_nanoseconds","elapsed_ns":70468900,"offered_ns":30000000,"drain_ns":1000000000,"phase_window_start_ns":0,"admissions":[]map[string]any{{"agent":0,"step":0,"slot":0,"retry":0,"role":"admission","tool":"edit.file","at":start,"offset_ns":0}},"attempts":[]map[string]any{{"agent":0,"step":0,"slot":0,"retry":0,"role":"workload","tool":"edit.file","at":start,"offset_ns":0},{"agent":0,"step":0,"slot":0,"retry":0,"role":"probe","tool":"search.symbols","at":finished,"offset_ns":70468900}},"edits":[]map[string]any{{"role":"edit","tool":"GortexLoadMarkerA0S0","at":start,"offset_ns":0}}}}
 data,err:=json.Marshal(raw);if err!=nil {t.Fatal(err)}
 var observed prDrainObservation;if err:=json.Unmarshal(data,&observed);err!=nil {t.Fatal(err)}
 // Round-trip the attempts too: all timestamps are wall trace after JSON.
 data,err=json.Marshal(phase);if err!=nil {t.Fatal(err)};if err=json.Unmarshal(data,&phase);err!=nil {t.Fatal(err)}
 if err:=validatePREditDrainObservation(phase,observed,1);err!=nil {t.Fatalf("actual Windows completed mono interval rejected after JSON: %v",err)}
}
