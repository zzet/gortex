package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func collectLogs(root string) (map[string]string, map[string]int, error) {
	artifacts, crashes := make(map[string]string), map[string]int{"panic_lines": 0, "sigabrt_lines": 0, "oom_log_lines": 0}
	dir := filepath.Join(root, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return artifacts, crashes, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return artifacts, crashes, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "daemon") || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		source, err := os.Open(filepath.Join(root, entry.Name()))
		if err != nil {
			return artifacts, crashes, err
		}
		dest := filepath.Join(dir, entry.Name())
		target, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = source.Close()
			return artifacts, crashes, err
		}
		_, copyErr := io.Copy(target, source)
		err = errors.Join(copyErr, source.Close(), target.Close())
		if err != nil {
			return artifacts, crashes, err
		}
		artifacts[entry.Name()] = dest
		file, err := os.Open(dest)
		if err != nil {
			return artifacts, crashes, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), maxWireBytes)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "panic:") || strings.HasPrefix(line, "fatal error:") {
				crashes["panic_lines"]++
			}
			if strings.Contains(line, "SIGABRT:") || strings.Contains(line, "signal SIGABRT") {
				crashes["sigabrt_lines"]++
			}
			if strings.Contains(strings.ToLower(line), "out of memory") {
				crashes["oom_log_lines"]++
			}
		}
		err = errors.Join(scanner.Err(), file.Close())
		if err != nil {
			return artifacts, crashes, err
		}
	}
	for _, name := range []string{"daemon.log", "daemon.stderr.log"} {
		if _, ok := artifacts[name]; !ok {
			artifacts[name] = "unavailable: foreground daemon may log to captured stderr instead"
		}
	}
	return artifacts, crashes, nil
}
