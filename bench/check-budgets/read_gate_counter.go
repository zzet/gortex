package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

type statusCapture struct {
	Index    int       `json:"index"`
	PID      int       `json:"owned_pid"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
}
type statusCaptureWire struct {
	Version  int             `json:"schema_version"`
	Count    int             `json:"count"`
	Captures []statusCapture `json:"captures"`
}

func validateOwnedStatusCensus(p phase) (int, error) {
	var wire statusCaptureWire
	d := json.NewDecoder(bytes.NewReader(p.Gate["status_capture_v1"]))
	d.DisallowUnknownFields()
	if d.Decode(&wire) != nil || d.Decode(new(any)) != io.EOF || wire.Version != 1 || wire.Count < 2 || wire.Count != len(wire.Captures) || wire.Count != len(p.Status) {
		return 0, errors.New("missing or incompatible complete owned status capture census")
	}
	var window struct {
		Started time.Time `json:"started"`
		Ended   time.Time `json:"ended"`
	}
	if json.Unmarshal(p.Gate["process_counter_window"], &window) != nil || window.Started.IsZero() || !window.Ended.After(window.Started) {
		return 0, errors.New("missing phase counter window")
	}
	pid := wire.Captures[0].PID
	previous := wire.Captures[0].Started
	for i, c := range wire.Captures {
		if c.Index != i || pid <= 0 || c.PID != pid || c.Started.IsZero() || c.Started.Before(previous) || c.Finished.Before(c.Started) {
			return 0, errors.New("foreign or reordered owned status capture")
		}
		previous = c.Finished
		var status struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(p.Status[i], &status) != nil || status.PID != pid {
			return 0, errors.New("foreign daemon status identity")
		}
	}
	if wire.Captures[0].Finished.After(window.Started) || wire.Captures[len(wire.Captures)-1].Started.Before(window.Ended) {
		return 0, errors.New("owned status captures do not bracket phase counter window")
	}
	return pid, nil
}

func measureReadGateClosures(p phase) (float64, error) {
	if _, err := validateOwnedStatusCensus(p); err != nil {
		return 0, err
	}
	var first, last int64
	for i, raw := range p.Status {
		var status struct {
			Storage struct {
				Reclaim struct {
					PauseCount *int64 `json:"pause_count"`
				} `json:"wal_reclaim"`
			} `json:"storage"`
		}
		if json.Unmarshal(raw, &status) != nil || status.Storage.Reclaim.PauseCount == nil || *status.Storage.Reclaim.PauseCount < 0 {
			return 0, errors.New("missing daemon read-gate counter")
		}
		count := *status.Storage.Reclaim.PauseCount
		if i == 0 {
			first = count
		} else if count < last {
			return 0, errors.New("read-gate counter reset during phase")
		}
		last = count
	}
	if last-first > 1<<53 {
		return 0, errors.New("read-gate delta exceeds exact metric integer range")
	}
	value := float64(last - first)
	aggregate, err := number(p.Metrics["reclaim_gate_closures"], "closures")
	if err != nil || aggregate != value {
		return 0, errors.New("raw read-gate counters disagree with producer aggregate")
	}
	return value, nil
}
