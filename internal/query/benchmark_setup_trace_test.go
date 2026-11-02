package query

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type queryBenchmarkSetupTrace struct {
	Version        int       `json:"schema_version"`
	Started        time.Time `json:"started"`
	Finished       time.Time `json:"finished"`
	IndexStarted   time.Time `json:"index_started"`
	IndexFinished  time.Time `json:"index_finished"`
	IndexSeconds   float64   `json:"index_seconds"`
	ElapsedSeconds float64   `json:"elapsed_seconds"`
	Files          int       `json:"files"`
	Workers        int       `json:"workers"`
	Error          string    `json:"error"`
}

func writeQueryBenchmarkSetupTrace(prefix, phase string, trace queryBenchmarkSetupTrace) error {
	if prefix == "" {
		return nil
	}
	if phase != "start" && phase != "finish" {
		return errors.New("unknown query setup trace phase")
	}
	data, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(prefix+"."+phase+".json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return errors.Join(err, f.Close())
}

func TestQueryBenchmarkSetupTraceRetainsTimingAndRefusesOverwrite(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "setup")
	start := queryBenchmarkSetupTrace{Version: 1, Started: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := writeQueryBenchmarkSetupTrace(prefix, "start", start); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prefix + ".finish.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("start receipt invented completed setup")
	}
	finish := start
	finish.IndexStarted, finish.IndexFinished = start.Started.Add(time.Second), start.Started.Add(119*time.Second)
	finish.IndexSeconds = 118
	finish.Finished, finish.ElapsedSeconds, finish.Files, finish.Workers = start.Started.Add(120*time.Second), 120, 100, 4
	if err := writeQueryBenchmarkSetupTrace(prefix, "finish", finish); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(prefix + ".finish.json")
	if err != nil {
		t.Fatal(err)
	}
	var got queryBenchmarkSetupTrace
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != finish {
		t.Fatalf("setup timing or fixture census changed: %+v", got)
	}
	if err := writeQueryBenchmarkSetupTrace(prefix, "finish", start); !errors.Is(err, os.ErrExist) {
		t.Fatal("diagnosis overwrote an existing completion receipt")
	}
	if err := writeQueryBenchmarkSetupTrace("", "start", start); err != nil {
		t.Fatal(err)
	}
}
