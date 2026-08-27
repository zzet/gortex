package store_sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"testing"
)

func TestConnectionPoolBoundsPerConnectionMemory(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_STABLE_READ_POOL", "")
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	expectedMax := runtime.NumCPU()
	if expectedMax > sqliteMaxOpenConns {
		expectedMax = sqliteMaxOpenConns
	}
	if got := store.db.Stats().MaxOpenConnections; got != expectedMax {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, expectedMax)
	}

	// Hold every allowed connection concurrently, then return them together.
	// database/sql must close all but one immediately; otherwise each retained
	// modernc connection keeps its own mmap and page cache alive while idle.
	ctx := context.Background()
	conns := make([]*sql.Conn, 0, expectedMax)
	for range expectedMax {
		conn, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			t.Fatalf("close connection: %v", err)
		}
	}

	stats := store.db.Stats()
	if stats.Idle > sqliteMaxIdleConns {
		t.Fatalf("idle connections = %d, want <= %d", stats.Idle, sqliteMaxIdleConns)
	}
	if stats.OpenConnections > sqliteMaxIdleConns {
		t.Fatalf("open connections after burst = %d, want <= %d", stats.OpenConnections, sqliteMaxIdleConns)
	}
}

func TestStableReadPoolModeRetainsBoundedConnections(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_STABLE_READ_POOL", "true")
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if store.db == store.writerDB {
		t.Fatal("stable read-pool mode removed the dedicated read pool")
	}
	expectedMax := runtime.NumCPU()
	if expectedMax > sqliteMaxOpenConns {
		expectedMax = sqliteMaxOpenConns
	}
	if got := store.db.Stats().MaxOpenConnections; got != expectedMax {
		t.Fatalf("MaxOpenConnections = %d, want %d", got, expectedMax)
	}

	ctx := context.Background()
	conns := make([]*sql.Conn, 0, expectedMax)
	for range expectedMax {
		conn, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			t.Fatalf("close connection: %v", err)
		}
	}

	stats := store.db.Stats()
	if stats.Idle != expectedMax {
		t.Fatalf("idle connections = %d, want %d", stats.Idle, expectedMax)
	}
	if stats.OpenConnections != expectedMax {
		t.Fatalf("open connections after burst = %d, want %d", stats.OpenConnections, expectedMax)
	}
}
