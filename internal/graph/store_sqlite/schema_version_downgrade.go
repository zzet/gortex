package store_sqlite

import (
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
	probe, err := probeStoreSchema(path)
	if err != nil {
		return err
	}
	return probe.refuseNewer(supported)
}

// storeProbe is what the read-only schema probe learned about an on-disk
// store before Open reaches it with a writer connection. The zero value
// describes an in-memory database or a file that does not exist yet.
type storeProbe struct {
	filename string // main database file; empty for an in-memory database
	uri      string // read-only, WAL-aware URI over filename
	exists   bool   // filename was present when the probe ran
	stored   int    // PRAGMA user_version, including a version held in WAL
}

// refuseNewer rejects a stored schema this binary cannot interpret.
func (p storeProbe) refuseNewer(supported int) error {
	if p.exists && p.stored > supported {
		return &SchemaTooNewError{Stored: p.stored, Supported: supported}
	}
	return nil
}

// probeStoreSchema reads the stored schema version of the database at path
// through a read-only connection, so nothing about the file (journal mode,
// WAL contents, size) changes. Open uses the result to refuse a newer schema
// and to size a pending upgrade before its writer connection exists.
func probeStoreSchema(path string) (storeProbe, error) {
	var probe storeProbe
	if path == ":memory:" {
		return probe, nil
	}
	var uri *url.URL
	var filename string
	if strings.HasPrefix(path, "file:") {
		var err error
		uri, err = url.Parse(path)
		if err != nil {
			return storeProbe{}, fmt.Errorf("sqlite schema probe URI: %w", err)
		}
		query, err := url.ParseQuery(uri.RawQuery)
		if err != nil {
			return storeProbe{}, fmt.Errorf("sqlite schema probe query: %w", err)
		}
		if len(query["mode"]) > 1 {
			return storeProbe{}, errors.New("sqlite schema probe: duplicate mode parameters")
		}
		if uri.Opaque == ":memory:" || query.Get("mode") == "memory" {
			return probe, nil
		}
		if uri.Host != "" && uri.Host != "localhost" {
			return storeProbe{}, fmt.Errorf("sqlite schema probe: unsupported file authority %q", uri.Host)
		}
		filename = uri.Path
		if uri.Opaque != "" {
			filename, err = url.PathUnescape(uri.Opaque)
			if err != nil {
				return storeProbe{}, fmt.Errorf("sqlite schema probe path: %w", err)
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
			return storeProbe{}, fmt.Errorf("sqlite schema probe path: %w", err)
		}
		filename = absolute
		slashed := filepath.ToSlash(absolute)
		if !strings.HasPrefix(slashed, "/") {
			slashed = "/" + slashed
		}
		uri = &url.URL{Scheme: "file", Path: slashed, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	}
	uri.Fragment = ""
	probe.filename = filename
	probe.uri = uri.String()
	if _, err := os.Stat(filename); err != nil {
		if os.IsNotExist(err) {
			return probe, nil // A new store has no on-disk schema to reject.
		}
		return storeProbe{}, fmt.Errorf("sqlite schema probe stat: %w", err)
	}
	probe.exists = true
	db, err := openSQLiteStoreDB(probe.uri)
	if err != nil {
		return storeProbe{}, fmt.Errorf("sqlite schema probe open: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	probe.stored, err = readUserVersion(db)
	if err != nil {
		return storeProbe{}, fmt.Errorf("sqlite schema probe version: %w", err)
	}
	return probe, nil
}
