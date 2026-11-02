//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

const issue767RemovalEvidenceLimit = 64 << 10

// retainRemovalFailure only observes the failed live-removal operation. It
// never stops a child, retries Git, opens the store, or changes a worktree.
func (f *issue767Fixture) retainRemovalFailure(dir string, args []string, output []byte, failure error) {
	pid := f.pid()
	logPath := f.logPath()
	logTail, logErr := issue767RemovalLogTail(logPath)
	processes, processErr := f.removalProcessTree(pid)
	report := struct {
		CapturedAt  time.Time `json:"captured_at"`
		Directory   string    `json:"directory"`
		Command     []string  `json:"command"`
		Error       string    `json:"error"`
		Output      string    `json:"output"`
		TracePath   string    `json:"git_trace2_path"`
		DaemonPID   int       `json:"daemon_pid"`
		ProcessTree string    `json:"process_tree"`
		ProcessErr  string    `json:"process_tree_error,omitempty"`
		HandleOwner string    `json:"handle_owner"`
		OwnerNote   string    `json:"handle_owner_note"`
		LogPath     string    `json:"daemon_log_path"`
		DaemonLog   string    `json:"daemon_log"`
		LogErr      string    `json:"daemon_log_error,omitempty"`
	}{
		CapturedAt: time.Now().UTC(), Directory: dir, Command: append([]string{"git"}, args...),
		Error: failure.Error(), Output: string(output[:min(len(output), issue767RemovalEvidenceLimit)]),
		TracePath: filepath.Join(f.root, "worktree-remove-trace.jsonl"),
		DaemonPID: pid, ProcessTree: processes, HandleOwner: "UNKNOWN",
		OwnerNote: "Process membership and logs do not identify the OS handle denying removal; no handle viewer was invoked.",
		LogPath:   logPath, DaemonLog: logTail,
	}
	if logErr != nil {
		report.LogErr = logErr.Error()
	}
	if processErr != nil {
		report.ProcessErr = processErr.Error()
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(f.root, "worktree-remove-failure.json"), data, 0600)
	}
	if err != nil {
		f.t.Logf("retain live-removal diagnostics: %v; original Git failure: %v", err, failure)
	}
}

func issue767RemovalLogTail(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	start := max(int64(0), info.Size()-issue767RemovalEvidenceLimit)
	data, err := io.ReadAll(io.NewSectionReader(file, start, issue767RemovalEvidenceLimit))
	return string(data), err
}

// PowerShell is already supplied by the native Windows runner. CIM reads are
// scoped to this daemon and its descendants (at most 64 entries); no host-wide
// command lines, handle scans, installations, or daemon signals are involved.
func (f *issue767Fixture) removalProcessTree(pid int) (string, error) {
	if runtime.GOOS != "windows" || pid <= 0 {
		return "", fmt.Errorf("process tree unavailable: platform=%s daemon_pid=%d", runtime.GOOS, pid)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(f.t.Context()), 5*time.Second)
	defer cancel()
	script := `$ErrorActionPreference = 'Stop'
$queue = [System.Collections.Generic.Queue[int]]::new()
$seen = [System.Collections.Generic.HashSet[int]]::new()
$queue.Enqueue(` + strconv.Itoa(pid) + `)
$rows = @()
while ($queue.Count -gt 0 -and $seen.Count -lt 64) {
  $next = $queue.Dequeue()
  if (-not $seen.Add($next)) { continue }
  $p = Get-CimInstance Win32_Process -Filter "ProcessId=$next" -OperationTimeoutSec 1
  if ($null -ne $p) {
    $line = [string]$p.CommandLine
    $rows += [pscustomobject]@{pid=$p.ProcessId; parent_pid=$p.ParentProcessId; name=$p.Name; command_line=$line.Substring(0, [Math]::Min(4096, $line.Length))}
    Get-CimInstance Win32_Process -Filter "ParentProcessId=$next" -OperationTimeoutSec 1 | ForEach-Object {
      if ($queue.Count -lt 64) { $queue.Enqueue([int]$_.ProcessId) }
    }
  }
}
ConvertTo-Json -InputObject $rows -Depth 3 -Compress`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Dir, cmd.Env = f.primary, f.env
	cmd.WaitDelay = 500 * time.Millisecond
	var output issue767RemovalOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if output.truncated {
		output.data = append(output.data, []byte("\n[process evidence truncated at 64 KiB]")...)
	}
	return string(output.data), err
}

// Drain the helper's pipes even after the retained output limit is reached.
// Its five-second timeout applies only to the diagnostic helper, never daemon.
type issue767RemovalOutput struct {
	data      []byte
	truncated bool
}

func (o *issue767RemovalOutput) Write(data []byte) (int, error) {
	keep := min(len(data), issue767RemovalEvidenceLimit-len(o.data))
	o.data = append(o.data, data[:keep]...)
	o.truncated = o.truncated || keep < len(data)
	return len(data), nil
}
