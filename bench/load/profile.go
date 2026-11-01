package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

func captureCPUProfile(ctx context.Context, status json.RawMessage, duration time.Duration, path string) error {
	return capturePProf(ctx, status, "/debug/pprof/profile?seconds="+strconv.FormatInt(int64(duration/time.Second), 10), duration+15*time.Second, path)
}

// captureHeapProfile saves the daemon's heap profile as of its last completed
// GC. It does not force a collection, which would stop the world mid-load.
func captureHeapProfile(ctx context.Context, status json.RawMessage, path string) error {
	return capturePProf(ctx, status, "/debug/pprof/heap", 30*time.Second, path)
}

// captureCollectedHeapProfile runs one garbage collection in the daemon before
// saving its heap profile, so the profile and the gauges that follow hold only
// what is still reachable. The collection stops the world: idle windows only.
func captureCollectedHeapProfile(ctx context.Context, status json.RawMessage, path string) error {
	return capturePProf(ctx, status, "/debug/pprof/heap?gc=1", 30*time.Second, path)
}

func capturePProf(ctx context.Context, status json.RawMessage, endpoint string, budget time.Duration, path string) error {
	var receipt struct {
		Addr string `json:"pprof_addr"`
	}
	if err := json.Unmarshal(status, &receipt); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(receipt.Addr)
	if err != nil {
		return fmt.Errorf("daemon did not publish usable pprof address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("refusing non-loopback pprof address")
	}
	profileCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	req, err := http.NewRequestWithContext(profileCtx, http.MethodGet, "http://"+receipt.Addr+endpoint, nil)
	if err != nil {
		return err
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("pprof returned HTTP%d", response.StatusCode)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, 128<<20))
	if written >= 128<<20 {
		copyErr = errors.New("profile exceeded 128MiB")
	}
	return errors.Join(copyErr, file.Close())
}
