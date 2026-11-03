package main

import (
 "encoding/json"
 "testing"
 "time"
)

func TestStatusCapturePreservesRawCounterBytesAndOwnedIndices(t *testing.T) {
 start:=time.Date(2026,11,3,0,0,0,0,time.UTC)
 first:=json.RawMessage(`{"pid":123,"storage":{"wal_reclaim":{"pause_count":7}}}`)
 last:=json.RawMessage(`{"pid":123,"storage":{"wal_reclaim":{"pause_count":9}}}`)
 p:=&Phase{Status:[]json.RawMessage{first}}
 recordStatusCapture(p,123,start,start.Add(time.Millisecond))
 p.Status=append(p.Status,last)
 recordStatusCapture(p,123,start.Add(time.Second),start.Add(time.Second+time.Millisecond))
 var wire statusCaptureWire
 if err:=json.Unmarshal(p.Gate["status_capture_v1"],&wire);err!=nil {t.Fatal(err)}
 if wire.Version!=1||wire.Count!=2||len(wire.Captures)!=2||wire.Captures[0].Index!=0||wire.Captures[1].Index!=1||wire.Captures[0].PID!=123||wire.Captures[1].PID!=123 {t.Fatalf("lost owned census: %+v",wire)}
 if string(p.Status[0])!=string(first)||string(p.Status[1])!=string(last) {t.Fatal("journal rewrote daemon counter payload")}
}

func TestStatusCaptureMalformedJournalRetainsFailure(t *testing.T) {
 p:=&Phase{Gate:map[string]json.RawMessage{"status_capture_v1":json.RawMessage(`{`)}}
 recordStatusCapture(p,0,time.Now(),time.Now())
 if len(p.Errors)!=1||string(p.Gate["status_capture_v1"])!="{" {t.Fatal("invalid journal overwritten as success")}
}
