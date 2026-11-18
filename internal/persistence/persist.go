package persistence

import "os"

// persistDir and persistFile centralise the restrictive on-disk permissions
// for the daemon's cache files (recent queries, notes, memories, frecency,
// keywords, feedback). They were written with os.MkdirAll(dir, 0o755) and
// os.Create(path) — a world-readable 0644 file inside a world-accessible
// 0755 directory — so on a shared machine any local user could read cached
// data, including query text and notes an agent wrote. Directories are now
// 0700 and files 0600, owner-only.
func persistDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}

func persistFile(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
}
