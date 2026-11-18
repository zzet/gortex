package doctor

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// antigravityNativeReadTools are the built-in Antigravity file/search tools
// whose work Gortex's graph tools replace. `run_command` is counted separately
// so `ShellRead` only increments when the shell command matches `readLike`.
var antigravityNativeReadTools = map[string]bool{
	"read_file":         true,
	"view_file":         true,
	"view_file_outline": true,
	"view_code_item":    true,
	"grep_search":       true,
	"find_by_name":      true,
	"list_dir":          true,
}

func antigravityBrainRoots(home string) []string {
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".gemini", "antigravity-cli", "brain"),
		filepath.Join(home, ".gemini", "antigravity", "brain"),
	}
}

// ScanAntigravitySessions tallies tool calls across Antigravity conversation
// transcripts (~/.gemini/antigravity-cli/brain/<id>/.system_generated/logs/transcript.jsonl
// and ~/.gemini/antigravity/brain/<id>/.system_generated/logs/transcript.jsonl).
//
// Antigravity invokes lazy MCP tools via `call_mcp_tool` with `ServerName: "gortex"`
// and `ToolName: "<op>"`, alongside eager `mcp_gortex_<op>` calls and native
// `read_file` / `grep_search` / `run_command` calls.
func ScanAntigravitySessions(home string, since time.Time, limit int) Adoption {
	roots := antigravityBrainRoots(home)
	primaryRoot := ""
	if len(roots) > 0 {
		primaryRoot = roots[0]
		for _, r := range roots {
			if _, err := os.Stat(r); err == nil {
				primaryRoot = r
				break
			}
		}
	}
	out := Adoption{
		Root:        primaryRoot,
		Tools:       map[string]int{},
		Models:      map[string]int{},
		CLIVersions: map[string]int{},
	}
	if home == "" {
		return out
	}

	type candidate struct {
		path string
		mod  time.Time
	}
	var files []candidate
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() || filepath.Base(path) != "transcript.jsonl" {
				return nil
			}
			info, statErr := d.Info()
			if statErr != nil {
				return nil
			}
			if info.ModTime().Before(since) {
				return nil
			}
			files = append(files, candidate{path: path, mod: info.ModTime()})
			return nil
		})
	}
	out.FilesFound = len(files)
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if len(files) > maxSessionFiles {
		files = files[:maxSessionFiles]
	}

	for _, file := range files {
		row, ok := scanAntigravityTranscript(file.path)
		if !ok {
			continue
		}
		out.InWindow++
		out.GortexCalls += row.Gortex
		out.OtherMCP += row.OtherMCP
		out.ShellCalls += row.Shell
		out.ShellReads += row.ShellRead
		if row.Model != "" {
			out.Models[row.Model]++
		}
		for name, n := range row.tools {
			out.Tools[name] += n
		}
		if len(out.Sessions) < limit {
			out.Sessions = append(out.Sessions, row.SessionRow)
		}
	}
	return out
}

func scanAntigravityTranscript(path string) (rollupRow, bool) {
	handle, err := os.Open(path)
	if err != nil {
		return rollupRow{}, false
	}
	defer handle.Close()

	row := rollupRow{tools: map[string]int{}}
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64<<10), maxRolloutLine)
	for scanner.Scan() {
		var rec struct {
			Type      string `json:"type"`
			CreatedAt string `json:"created_at"`
			ToolCalls []struct {
				Name string         `json:"name"`
				Args map[string]any `json:"args"`
			} `json:"tool_calls"`
		}
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		if row.Started == "" && rec.CreatedAt != "" {
			row.Started = rec.CreatedAt
		}
		for _, tc := range rec.ToolCalls {
			name := strings.TrimSpace(tc.Name)
			if name == "" {
				continue
			}
			if row.CWD == "" {
				if cwd := antigravityTranscriptArg(tc.Args, "Cwd", "cwd", "SearchDirectory", "DirectoryPath", "SearchPath"); cwd != "" {
					row.CWD = cwd
				}
			}
			lower := strings.ToLower(name)
			switch {
			case lower == "call_mcp_tool":
				server := antigravityTranscriptArg(tc.Args, "ServerName", "server_name", "serverName")
				tool := antigravityTranscriptArg(tc.Args, "ToolName", "tool_name", "toolName")
				if strings.EqualFold(server, "gortex") {
					row.Gortex++
					if tool == "" {
						tool = "gortex"
					}
					row.tools[tool]++
				} else if server != "" {
					row.OtherMCP++
				}
			case strings.HasPrefix(lower, "mcp_gortex_"):
				row.Gortex++
				row.tools[name[len("mcp_gortex_"):]]++
			case strings.HasPrefix(lower, "mcp__gortex__"):
				row.Gortex++
				row.tools[name[len("mcp__gortex__"):]]++
			case strings.HasPrefix(lower, "mcp_") || strings.HasPrefix(lower, "mcp__"):
				row.OtherMCP++
			case lower == "run_command":
				row.Shell++
				cmdLine := antigravityTranscriptArg(tc.Args, "CommandLine", "command", "cmd")
				if readLike.MatchString(cmdLine) {
					row.ShellRead++
				}
			case antigravityNativeReadTools[lower]:
				row.Shell++
				row.ShellRead++
			}
		}
	}
	if row.Gortex == 0 && row.Shell == 0 && row.OtherMCP == 0 {
		return rollupRow{}, false
	}
	return row, true
}

func antigravityTranscriptArg(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if raw, ok := args[k].(string); ok {
			s := strings.TrimSpace(raw)
			if strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
				var unquoted string
				if json.Unmarshal([]byte(s), &unquoted) == nil {
					s = strings.TrimSpace(unquoted)
				} else {
					s = strings.Trim(s, `"`)
				}
			}
			if s != "" {
				return s
			}
		}
	}
	return ""
}
