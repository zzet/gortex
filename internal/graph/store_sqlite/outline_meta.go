package store_sqlite

import (
	"bytes"
	"database/sql/driver"
	"fmt"

	"modernc.org/sqlite"
)

// Register one immutable deterministic function before any store connections
// open. The SQL CASE guard calls it only for candidate metadata; ordinary flat
// edges stay entirely inside SQLite. No function or state is persisted.
// SQL passes BLOB arguments because the driver truncates copied TEXT at NUL.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("gortex_outline_via_is", 2, outlineViaSQL)
}

func outlineViaSQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	var blob []byte
	switch value := args[0].(type) {
	case []byte:
		blob = value
	case string:
		blob = []byte(value)
	case nil:
		return int64(0), nil
	default:
		return nil, fmt.Errorf("unsupported outline metadata type %T", value)
	}
	var via string
	switch value := args[1].(type) {
	case []byte:
		via = string(value)
	case string:
		via = value
	default:
		return nil, fmt.Errorf("unsupported outline via type %T", value)
	}
	excluded, err := outlineMetaVia(blob, via)
	if err != nil {
		return nil, err
	}
	if excluded {
		return int64(1), nil
	}
	return int64(0), nil
}

func outlineMetaVia(blob []byte, via string) (bool, error) {
	if isFlatMeta(blob) {
		// The prefilter can match a different field or a longer string.
		// Read the actual top-level via value before subtracting anything.
		d := &metaDecoder{buf: blob[2:]}
		count, err := d.readCount()
		if err != nil {
			return false, err
		}
		for range count {
			key, err := d.readRawString()
			if err != nil {
				return false, err
			}
			if bytes.Equal(key, []byte("via")) {
				value, err := d.readValue()
				return value == via, err
			}
			if err := d.skipValue(); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	meta, err := decodeMeta(blob)
	return meta["via"] == via, err
}
