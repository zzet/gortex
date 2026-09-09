package store_sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ErrSchemaTooNew means this binary cannot safely interpret the stored schema.
// WithRebuild never overrides this refusal: the store also holds durable
// checkout metadata, and a newer schema is not an authorized rebuild target.
var ErrSchemaTooNew = errors.New("sqlite store schema is newer than this binary supports")

// SchemaTooNewError identifies the incompatible versions without suggesting
// deletion as a recovery step. Callers can match ErrSchemaTooNew with errors.Is.
type SchemaTooNewError struct {
	Stored    int
	Supported int
}

func (e *SchemaTooNewError) Error() string {
	return fmt.Sprintf("%v: stored v%d, supported v%d; database retained, use a compatible newer binary", ErrSchemaTooNew, e.Stored, e.Supported)
}

func (e *SchemaTooNewError) Unwrap() error { return ErrSchemaTooNew }

// checkSchemaDowngrade runs before the writer DSN can change journal mode or
// checkpoint a newer database. It uses SQLite's read-only WAL-aware view, not
// the main-file header or immutable=1 (either can miss a version held in WAL).
// Open still rechecks on its writer handle before planning any schema changes.
// The caller must hold exclusive store ownership across both checks and Open:
// the second check prevents a rebuild, but cannot undo writer PRAGMAs if another
// process swaps or upgrades the database after this read-only probe.
func checkSchemaDowngrade(path string, supported int) error {
	if path == ":memory:" {
		return nil
	}
	var uri *url.URL
	var filename string
	if strings.HasPrefix(path, "file:") {
		var err error
		uri, err = url.Parse(path)
		if err != nil {
			return fmt.Errorf("sqlite schema probe URI: %w", err)
		}
		query, err := url.ParseQuery(uri.RawQuery)
		if err != nil {
			return fmt.Errorf("sqlite schema probe query: %w", err)
		}
		if len(query["mode"]) > 1 {
			return errors.New("sqlite schema probe: duplicate mode parameters")
		}
		if uri.Opaque == ":memory:" || query.Get("mode") == "memory" {
			return nil
		}
		if uri.Host != "" && uri.Host != "localhost" {
			return fmt.Errorf("sqlite schema probe: unsupported file authority %q", uri.Host)
		}
		filename = uri.Path
		if uri.Opaque != "" {
			filename, err = url.PathUnescape(uri.Opaque)
			if err != nil {
				return fmt.Errorf("sqlite schema probe path: %w", err)
			}
		}
		if trimmed := strings.TrimPrefix(filename, "/"); filepath.VolumeName(trimmed) != "" {
			filename = trimmed
		}
		filename = filepath.FromSlash(filename)
		// Preserve URI routing, but never inherit pragmas, immutable snapshots,
		// disabled locks, or a writable mode from the eventual writer DSN.
		query.Del("_pragma")
		query.Del("_txlock")
		query.Del("immutable")
		query.Del("nolock")
		query.Set("mode", "ro")
		query.Add("_pragma", "busy_timeout(5000)")
		uri.RawQuery = query.Encode()
	} else {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("sqlite schema probe path: %w", err)
		}
		filename = absolute
		slashed := filepath.ToSlash(absolute)
		if !strings.HasPrefix(slashed, "/") {
			slashed = "/" + slashed
		}
		uri = &url.URL{Scheme: "file", Path: slashed, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	}
	if _, err := os.Stat(filename); err != nil {
		if os.IsNotExist(err) {
			return nil // A new store has no on-disk schema to reject.
		}
		return fmt.Errorf("sqlite schema probe stat: %w", err)
	}
	uri.Fragment = ""
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return fmt.Errorf("sqlite schema probe open: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	stored, err := readUserVersion(db)
	if err != nil {
		return fmt.Errorf("sqlite schema probe version: %w", err)
	}
	if stored > supported {
		return &SchemaTooNewError{Stored: stored, Supported: supported}
	}
	return nil
}
