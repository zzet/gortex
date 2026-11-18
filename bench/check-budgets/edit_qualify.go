package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
)

const mixedBackgroundPolicy = "shared-mixed-background-b-only-v1"

type editQualificationInputs struct {
	Version      int    `json:"schema_version"`
	ReportSHA256 string `json:"report_sha256"`
	Statuses     []struct {
		Ordinal int    `json:"ordinal"`
		Path    string `json:"path"`
		SHA256  string `json:"sha256"`
	} `json:"statuses"`
}

type mixedBackgroundQualification struct {
	Ordinal           int      `json:"ordinal"`
	Outcome           string   `json:"qualified_outcome"`
	RawOutcome        string   `json:"raw_outcome"`
	OriginalErrors    []string `json:"original_errors"`
	StatusSHA256      string   `json:"status_sha256"`
	RawEvidenceSHA256 string   `json:"raw_evidence_sha256"`
	Background        metric   `json:"requested_background_ms"`
}

type editQualificationResult struct {
	Version             int                            `json:"schema_version"`
	Policy              string                         `json:"policy"`
	ReportSHA256        string                         `json:"report_sha256"`
	RawComplete         bool                           `json:"raw_complete"`
	RawErrors           []string                       `json:"raw_errors"`
	RawCrashes          map[string]*int                `json:"raw_crashes"`
	AggregateAcceptance bool                           `json:"aggregate_acceptance"`
	Qualifications      []mixedBackgroundQualification `json:"qualifications"`
}

// editQualifyCommand writes only a versioned qualification overlay. It does not
// rewrite a report, upgrade raw completion/wall flags or certify repeat gates.
func editQualifyCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("check-budgets edit-qualify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("report", "", "unchanged strict report")
	manifest := flags.String("statuses", "", "schema1 exact-report and original status pins")
	output := flags.String("out", "", "new versioned qualification overlay")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *source == "" || *manifest == "" || *output == "" {
		fmt.Fprintln(stderr, "--report, --statuses and --out are required")
		return 2
	}
	result, err := qualifyMixedBackgroundReport(*source, *manifest)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err = writeNewJSON(*output, result); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s: %d separately qualified mixed-B edits; raw report unchanged; aggregate acceptance not certified\n", result.Policy, len(result.Qualifications))
	return 0
}

func qualifyMixedBackgroundReport(source, manifest string) (editQualificationResult, error) {
	result := editQualificationResult{Version: 1, Policy: mixedBackgroundPolicy}
	b, err := readQualificationFile(manifest, 64<<10)
	if err != nil {
		return result, err
	}
	var pins editQualificationInputs
	if err = decodeStrictJSON(b, &pins); err != nil {
		return result, err
	}
	if pins.Version != 1 || len(pins.Statuses) == 0 || len(pins.Statuses) > 1000 {
		return result, errors.New("qualification needs schema1 and bounded exact status pins")
	}
	b, err = readQualificationFile(source, 64<<20)
	if err != nil {
		return result, err
	}
	result.ReportSHA256 = fmt.Sprintf("%x", sha256.Sum256(b))
	if pins.ReportSHA256 != result.ReportSHA256 {
		return result, errors.New("qualification report hash mismatch")
	}
	r, err := decodeReport(b)
	if err != nil || r.Version != 1 || len(r.Phases) != 1 || r.Phases[0].Name != "edit-and-probe" {
		return result, errors.New("qualification requires one schema1 edit-probe report")
	}
	if !cleanMixedBackgroundCrashCensus(r.Crashes) {
		return result, errors.New("raw report crash census is missing, unknown or nonzero")
	}
	result.RawComplete, result.RawErrors, result.RawCrashes = r.Complete, r.Errors, r.Crashes
	var wire editLoopWire
	if json.Unmarshal(r.Phases[0].EditProbe, &wire) != nil || len(wire.Edits) != wire.Plan.Count || wire.Plan.Count < 100 || len(wire.Plan.Steps) != wire.Plan.Count || !sharedPhysicalEditLoop(wire) {
		return result, errors.New("qualification requires complete shared physical-worktree denominator")
	}
	// This validation projection is never emitted as a rewritten report. It
	// allows existing strict outcome validators to check the unaffected rows.
	projection := wire
	projection.Edits = slices.Clone(wire.Edits)
	seen := map[int]bool{}
	total := 0
	qualifiedErrors := make(map[int]string)
	for _, pin := range pins.Statuses {
		if pin.Ordinal < 0 || pin.Ordinal >= len(wire.Edits) || seen[pin.Ordinal] || pin.Path == "" {
			return result, errors.New("duplicate or out-of-range status pin")
		}
		seen[pin.Ordinal] = true
		path := pin.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(manifest), path)
		}
		raw, readErr := readQualificationFile(path, 16<<20)
		total += len(raw)
		if readErr != nil || total > 64<<20 || len(raw) != wire.Edits[pin.Ordinal].EvidenceBytes || fmt.Sprintf("%x", sha256.Sum256(raw)) != pin.SHA256 {
			return result, errors.New("missing, oversized or hash-mismatched original status")
		}
		status, statusErr := latestMixedBackgroundStatus(raw)
		if statusErr != nil {
			return result, statusErr
		}
		if !mixedBackgroundRecord(wire, pin.Ordinal, status) {
			return result, fmt.Errorf("edit %d does not satisfy the B-only qualification", pin.Ordinal)
		}
		record := wire.Edits[pin.Ordinal]
		qualifiedErrors[pin.Ordinal] = fmt.Sprintf("edit %d: diagnostic_failure (%s)", pin.Ordinal, strings.Join(record.Errors, "; "))
		result.Qualifications = append(result.Qualifications, mixedBackgroundQualification{Ordinal: pin.Ordinal, Outcome: "qualified_mixed_background_unavailable", RawOutcome: record.Outcome, OriginalErrors: record.Errors, StatusSHA256: fmt.Sprintf("%x", sha256.Sum256(status)), RawEvidenceSHA256: pin.SHA256, Background: metric{Unit: "ms", Reason: "mixed requested background includes earlier edits; unavailable"}})
		projection.Edits[pin.Ordinal].Outcome = "complete"
		projection.Edits[pin.Ordinal].Errors = nil
	}
	expectedErrors := make([]string, 0, len(qualifiedErrors))
	for ordinal := range wire.Edits {
		if message, ok := qualifiedErrors[ordinal]; ok {
			expectedErrors = append(expectedErrors, message)
		}
	}
	joined := strings.Join(expectedErrors, "\n")
	if r.Complete || len(r.Errors) != 1 || r.Errors[0] != "edit-and-probe: "+joined || len(r.Phases[0].Errors) != 1 || r.Phases[0].Errors[0] != joined {
		return result, errors.New("raw report contains an unqualified aggregate failure")
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return result, err
	}
	projectedPhase := r.Phases[0]
	projectedPhase.EditProbe = encoded
	if _, err = decodeEditLoopWire(projectedPhase); err != nil {
		return result, fmt.Errorf("unqualified or invalid remaining edit evidence: %w", err)
	}
	return result, nil
}

func readQualificationFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > limit {
		return nil, errors.New("qualification input is not a bounded regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	after, statErr := f.Stat()
	if err != nil || statErr != nil || len(b) > int(limit) || int64(len(b)) != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return nil, errors.New("qualification input changed or exceeded its bound")
	}
	return b, nil
}

func decodeStrictJSON(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("qualification input needs exactly one JSON document")
	}
	return nil
}

// Consume the already retained bounded artifact. Only its last status RPC can
// qualify; a later refusal or malformed frame never falls back to older data.
func latestMixedBackgroundStatus(raw []byte) (json.RawMessage, error) {
	var last struct {
		Event   string `json:"event"`
		Purpose string `json:"purpose"`
		Tool    string `json:"tool"`
		WireID  int64  `json:"wire_id"`
		Error   string `json:"error"`
		Payload struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Structured json.RawMessage `json:"structuredContent"`
		} `json:"payload"`
	}
	var final json.RawMessage
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var kind struct {
			Event   string `json:"event"`
			Purpose string `json:"purpose"`
		}
		if json.Unmarshal(line, &kind) != nil {
			return nil, errors.New("malformed raw evidence frame")
		}
		if kind.Event == "rpc" && kind.Purpose == "status" {
			final = append(final[:0], line...)
		}
	}
	if len(final) == 0 || json.Unmarshal(final, &last) != nil {
		return nil, errors.New("malformed or missing latest status RPC")
	}
	if last.WireID <= 0 || last.Tool != "change.receipt" || last.Error != "" || last.Payload.IsError || len(last.Payload.Content) != 1 || last.Payload.Content[0].Type != "text" {
		return nil, errors.New("latest status RPC is missing or refused")
	}
	body := json.RawMessage(last.Payload.Content[0].Text)
	decoded, decodeErr := exactMixedStatusJSON(body)
	if decodeErr != nil {
		return nil, errors.New("latest status body is malformed")
	}
	if !absentEditLoopTiming(last.Payload.Structured) {
		structured, structuredErr := exactMixedStatusJSON(last.Payload.Structured)
		if structuredErr != nil || !reflect.DeepEqual(decoded, structured) {
			return nil, errors.New("latest status content/structured body mismatch")
		}
	}
	return body, nil
}

func cleanMixedBackgroundCrashCensus(crashes map[string]*int) bool {
	for _, name := range []string{"panic_lines", "sigabrt_lines", "oom_log_lines"} {
		if crashes[name] == nil || *crashes[name] != 0 {
			return false
		}
	}
	for name, value := range crashes {
		switch name {
		case "panic_lines", "sigabrt_lines", "oom_log_lines", "oom_kills":
		default:
			return false
		}
		if value == nil || *value != 0 {
			return false
		}
	}
	return true
}

type exactMixedStatusNumber string

// UseNumber preserves every integer/decimal digit before semantic comparison.
// Rational normalization accepts equivalent numeric forms such as 1 and 1.0.
func exactMixedStatusJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("status body needs exactly one JSON document")
	}
	return normalizeMixedStatusNumbers(value)
}

func normalizeMixedStatusNumbers(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		number, ok := new(big.Rat).SetString(string(v))
		if !ok {
			return nil, errors.New("invalid exact status number")
		}
		return exactMixedStatusNumber(number.RatString()), nil
	case map[string]any:
		for key, item := range v {
			normalized, err := normalizeMixedStatusNumbers(item)
			if err != nil {
				return nil, err
			}
			v[key] = normalized
		}
	case []any:
		for index, item := range v {
			normalized, err := normalizeMixedStatusNumbers(item)
			if err != nil {
				return nil, err
			}
			v[index] = normalized
		}
	}
	return value, nil
}
