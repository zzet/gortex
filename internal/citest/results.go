package citest

import (
	"encoding/json"
	"fmt"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const raceReport = "WARNING: DATA RACE"
const timeoutReport = "panic: test timed out"

var crashReport = regexp.MustCompile(`(?m)^(panic: |fatal error: |SIG[A-Z]+: |Exception 0x[0-9a-fA-F]+ )|signal arrived during`)
var resultLine = regexp.MustCompile(`^--- (PASS|FAIL|SKIP): (\S+) \(([\d.]+)s\)`)

func hardFailure(output string) bool {
	return strings.Contains(output, raceReport) || crashReport.MatchString(output)
}
func censusNames(output string) ([]string, error) {
	seen := set{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "Test") || strings.HasPrefix(line, "Example") || strings.HasPrefix(line, "Fuzz") {
			if !token.IsIdentifier(line) || seen.has(line) {
				return nil, fmt.Errorf("invalid or duplicate compiled test census line %q", line)
			}
			seen.add(line)
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("compiled test census must be nonempty and unique")
	}
	return seen.sorted(), nil
}

type patternTrie struct {
	terminal bool
	children map[rune]*patternTrie
}

func exactPattern(names []string) string {
	root := &patternTrie{}
	for _, name := range names {
		node := root
		for _, char := range name {
			if node.children == nil {
				node.children = map[rune]*patternTrie{}
			}
			if node.children[char] == nil {
				node.children[char] = &patternTrie{}
			}
			node = node.children[char]
		}
		node.terminal = true
	}
	var render func(*patternTrie) string
	render = func(node *patternTrie) string {
		bySuffix := map[string][]rune{}
		for char, child := range node.children {
			suffix := render(child)
			bySuffix[suffix] = append(bySuffix[suffix], char)
		}
		suffixes := make([]string, 0, len(bySuffix))
		for suffix := range bySuffix {
			suffixes = append(suffixes, suffix)
		}
		slices.Sort(suffixes)
		alternatives := []string{}
		for _, suffix := range suffixes {
			chars := bySuffix[suffix]
			slices.Sort(chars)
			prefix := regexp.QuoteMeta(string(chars))
			if len(chars) > 1 {
				prefix = "[" + prefix + "]"
			}
			alternatives = append(alternatives, prefix+suffix)
		}
		if len(alternatives) == 0 {
			return ""
		}
		body := alternatives[0]
		if len(alternatives) > 1 {
			body = "(?:" + strings.Join(alternatives, "|") + ")"
		}
		if node.terminal {
			return "(?:" + body + ")?"
		}
		return body
	}
	return "^(?:" + render(root) + ")$"
}
func planBatches(names []string, shard, count, batches int) ([][]string, error) {
	if count < 1 || shard < 0 || shard >= count || batches < 1 {
		return nil, fmt.Errorf("invalid batch/shard bounds")
	}
	selected := []string{}
	for i := shard; i < len(names); i += count {
		selected = append(selected, names[i])
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("every shard must have at least one compiled test")
	}
	batches = min(batches, len(selected))
	out := make([][]string, batches)
	for i, name := range selected {
		out[i%batches] = append(out[i%batches], name)
	}
	return out, nil
}

type testResult struct {
	status  string
	seconds float64
}

func parseResults(output string) map[string]testResult {
	out := map[string]testResult{}
	for _, line := range strings.Split(output, "\n") {
		m := resultLine.FindStringSubmatch(line)
		if len(m) == 0 || strings.Contains(m[2], "/") {
			continue
		}
		seconds, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		status := m[1]
		if out[m[2]].status == "FAIL" {
			status = "FAIL"
		}
		out[m[2]] = testResult{status, seconds}
	}
	return out
}
func testOutput(output, name string) string {
	lines := strings.Split(output, "\n")
	start, end := -1, -1
	for i, line := range lines {
		if start < 0 && line == "=== RUN   "+name {
			start = i
		}
		if end < 0 && strings.HasPrefix(line, "--- FAIL: "+name+" (") {
			end = i
		}
	}
	if start < 0 || end < start {
		return output
	}
	tail := len(lines)
	for i := end + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "=== RUN   ") && !strings.Contains(lines[i], "/") {
			tail = i
			break
		}
	}
	return strings.Join(lines[start:tail], "\n")
}

type testKey struct{ pkg, name string }
type testEvents struct {
	unattributed  string
	tests         map[testKey]testResult
	packages      map[string]testResult
	output        map[testKey]string
	packageOutput map[string]string
}

func parseEvents(output string) testEvents {
	e := testEvents{tests: map[testKey]testResult{}, packages: map[string]testResult{}, output: map[testKey]string{}, packageOutput: map[string]string{}}
	pkgText := map[string]*strings.Builder{}
	testText := map[testKey]*strings.Builder{}
	appendPackage := func(pkg, text string) {
		if pkgText[pkg] == nil {
			pkgText[pkg] = &strings.Builder{}
		}
		pkgText[pkg].WriteString(text)
	}
	for _, line := range strings.Split(output, "\n") {
		var event struct {
			Action, Package, Test, Output, ImportPath string
			Elapsed                                   float64
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			e.unattributed += line + "\n"
			continue
		}
		key := testKey{event.Package, event.Test}
		switch event.Action {
		case "output":
			appendPackage(event.Package, event.Output)
			if event.Test != "" && !strings.Contains(event.Test, "/") {
				if testText[key] == nil {
					testText[key] = &strings.Builder{}
				}
				testText[key].WriteString(event.Output)
			}
		case "pass", "fail", "skip":
			if event.Test == "" {
				e.packages[event.Package] = testResult{event.Action, event.Elapsed}
			} else if !strings.Contains(event.Test, "/") {
				status := event.Action
				if e.tests[key].status == "fail" {
					status = "fail"
				}
				e.tests[key] = testResult{status, event.Elapsed}
			}
		case "build-output":
			appendPackage(event.ImportPath, event.Output)
		}
	}
	for pkg, text := range pkgText {
		e.packageOutput[pkg] = text.String()
	}
	for key, text := range testText {
		e.output[key] = text.String()
	}
	return e
}
func mergeCoverage(paths []string, destination string) (bool, error) {
	mode := ""
	blocks := map[string]uint64{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSuffix(line, "\r")
			if strings.HasPrefix(line, "mode: ") {
				if mode != "" && mode != line {
					return false, fmt.Errorf("coverage modes differ: %s and %s", mode, line)
				}
				mode = line
			} else if line != "" {
				i := strings.LastIndex(line, " ")
				if i < 0 {
					return false, fmt.Errorf("invalid coverage block %q", line)
				}
				count, err := strconv.ParseUint(line[i+1:], 10, 64)
				if err != nil {
					return false, err
				}
				key := line[:i]
				if ^uint64(0)-blocks[key] < count {
					return false, fmt.Errorf("coverage counter overflow")
				}
				blocks[key] += count
			}
		}
	}
	if mode == "" {
		return false, nil
	}
	keys := make([]string, 0, len(blocks))
	for key := range blocks {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var text strings.Builder
	text.WriteString(mode + "\n")
	for _, key := range keys {
		fmt.Fprintf(&text, "%s %d\n", key, blocks[key])
	}
	if err := os.WriteFile(destination, []byte(text.String()), 0600); err != nil {
		return false, err
	}
	return true, nil
}
