//go:build darwin

package store_sqlite

func vacuumOpenIsAnonymous(int32) bool { return false }
