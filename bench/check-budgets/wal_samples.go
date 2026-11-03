package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"time"
)

type walSample struct {
	At    time.Time `json:"at"`
	Bytes *float64  `json:"bytes"`
	Error string    `json:"error,omitempty"`
}
type walSampleWire struct {
	Version  int         `json:"schema_version"`
	Started  time.Time   `json:"started"`
	Finished time.Time   `json:"finished"`
	Count    int         `json:"count"`
	Samples  []walSample `json:"samples"`
}

// Recompute the existing producer's nearest-rank distribution from its actual
// owned WAL stat samples. An absent file/sample is unknown, never zero bytes.
func measureWALP95(p phase) (float64, error) {
	var w walSampleWire
	d := json.NewDecoder(bytes.NewReader(p.Gate["wal_samples_v1"]))
	d.DisallowUnknownFields()
	if d.Decode(&w) != nil || d.Decode(new(any)) != io.EOF || w.Version != 1 || w.Started.IsZero() || !w.Finished.After(w.Started) || w.Count < 1 || len(w.Samples) != w.Count {
		return 0, errors.New("missing or incompatible complete WAL sample census")
	}
	var window struct {
		Started time.Time `json:"started"`
		Ended   time.Time `json:"ended"`
	}
	if json.Unmarshal(p.Gate["process_counter_window"], &window) != nil || !window.Started.Equal(w.Started) || !window.Ended.Equal(w.Finished) {
		return 0, errors.New("WAL samples do not belong to the phase counter window")
	}
	values := make([]float64, 0, w.Count)
	previous := w.Started
	for _, s := range w.Samples {
		if s.At.Before(previous) || s.At.After(w.Finished) || s.Bytes == nil || !finiteNonnegative(*s.Bytes) || *s.Bytes != math.Trunc(*s.Bytes) || s.Error != "" {
			return 0, errors.New("missing, out-of-window or invalid owned WAL sample")
		}
		previous = s.At
		values = append(values, *s.Bytes)
	}
	slices.Sort(values)
	v := values[(95*len(values)+99)/100-1]
	aggregate, err := number(p.Metrics["p95_wal_bytes"], "bytes")
	if err != nil || aggregate != v {
		return 0, errors.New("raw WAL census does not reproduce producer p95")
	}
	return v / (1024 * 1024), nil
}
