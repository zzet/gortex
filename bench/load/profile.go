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
	profileCtx, cancel := context.WithTimeout(ctx, duration+15*time.Second)
	defer cancel()
	url := "http://" + receipt.Addr + "/debug/pprof/profile?seconds=" + strconv.FormatInt(int64(duration/time.Second), 10)
	req, err := http.NewRequestWithContext(profileCtx, http.MethodGet, url, nil)
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
		copyErr = errors.New("CPU profile exceeded128MiB")
	}
	return errors.Join(copyErr, file.Close())
}
