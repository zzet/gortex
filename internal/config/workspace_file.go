package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// workspaceParseError marks a `.gortex.yaml` that exists but is not valid
// YAML (or does not type-check against Config). Callers use it to separate
// "could not read the file" from "the file is malformed" in their warnings.
type workspaceParseError struct {
	err error
}

func (e *workspaceParseError) Error() string {
	return fmt.Sprintf("parsing: %v", e.err)
}

func (e *workspaceParseError) Unwrap() error { return e.err }

// ParseWorkspaceFileInto parses the repo-level `.gortex.yaml` at path into
// cfg with yaml.Unmarshal — the daemon's acceptance semantics. It is the
// single parser behind every surface that reports whether a workspace file
// is honored (the daemon loader, `gortex init`'s warning, and
// `config exclude list`), so those surfaces cannot disagree about which
// files are accepted and which are ignored: viper's weak decode accepts a
// scalar `exclude:` that yaml.Unmarshal rejects, so routing init through
// config.Load let a silently-ignored file produce no warning at all.
//
// A missing file is not a parse failure — the wrapped os.ErrNotExist is
// returned and each caller decides what absence means for it. Read errors
// surface as the raw os error; parse failures as a *workspaceParseError.
// The seed passed in cfg is the caller's choice: Default() for read-only
// surfaces, a zero value where the result is mutated and written back
// (persisting computed defaults into the file would be wrong).
func ParseWorkspaceFileInto(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return &workspaceParseError{err: err}
	}
	return nil
}

// ParseWorkspaceFile parses the repo-level `.gortex.yaml` at path over
// Default() — the daemon's overlay semantics, where the file's settings
// layer on top of the built-in defaults instead of replacing them (a
// zero-value seed turned the file's mere presence into a wholesale
// replacement: every field a partial `.gortex.yaml` didn't mention lost
// its documented default). Missing file → (nil, os.ErrNotExist). Read or
// parse failure → (nil, err).
func ParseWorkspaceFile(path string) (*Config, error) {
	cfg := Default()
	if err := ParseWorkspaceFileInto(path, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
