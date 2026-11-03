package main

import (
 "encoding/json"
 "time"
)

type statusCapture struct {
 Index int `json:"index"`
 PID int `json:"owned_pid"`
 Started time.Time `json:"started"`
 Finished time.Time `json:"finished"`
}
type statusCaptureWire struct {
 Version int `json:"schema_version"`
 Count int `json:"count"`
 Captures []statusCapture `json:"captures"`
}

// Add provenance to the existing raw status vector, without changing or
// synthesizing any daemon counter. This explicit versioned extension is
// independent of the optional nightly seed and never enters workload hashes.
func recordStatusCapture(phase *Phase, pid int, started, finished time.Time) {
 wire:=statusCaptureWire{Version:1}
 if raw:=phase.Gate["status_capture_v1"];len(raw)>0 {
  if err:=json.Unmarshal(raw,&wire);err!=nil {phase.Errors=append(phase.Errors,"invalid status capture journal: "+err.Error());return}
 }
 wire.Captures=append(wire.Captures,statusCapture{Index:len(phase.Status)-1,PID:pid,Started:started,Finished:finished})
 wire.Count=len(wire.Captures)
 if phase.Gate==nil {phase.Gate=make(map[string]json.RawMessage)}
 raw,err:=json.Marshal(wire)
 if err!=nil {phase.Errors=append(phase.Errors,"status capture journal: "+err.Error());return}
 phase.Gate["status_capture_v1"]=raw
}
