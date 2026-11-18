package source

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Embedded unsafe methods panic if the helper attempts a generic fallback.
type unsafeRegularSource struct{ ContentSource }

type panicRegularSource struct{ ContentSource }

func (panicRegularSource) ReadRegularFile(context.Context, string, int64) ([]byte, FileMeta, error) {
	panic("invalid byte limit reached optional source capability")
}

func TestReadRegularFileRejectsUnrepresentableLimits(t *testing.T) {
	for _, limit := range []int64{-1, int64(math.MaxInt), math.MaxInt64} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			data, _, err := ReadRegularFile(t.Context(), panicRegularSource{}, "go.mod", limit)
			if data != nil || err == nil || errors.Is(err, ErrRegularReadUnsupported) {
				t.Fatalf("invalid limit reached capability or succeeded: %q, %v", data, err)
			}
			// No protocol fields exist: invalid limits must return before touching them.
			data, err = (&gitBatch{}).readBounded(strings.Repeat("1", 40), limit)
			if data != nil || err == nil {
				t.Fatalf("invalid Git limit succeeded: %q, %v", data, err)
			}
		})
	}
}

func TestReadRegularFileRejectsUnsupportedAdapters(t *testing.T) {
	data, _, err := ReadRegularFile(t.Context(), unsafeRegularSource{}, "go.mod", 64)
	if data != nil || !errors.Is(err, ErrRegularReadUnsupported) {
		t.Fatalf("read = %q, %v", data, err)
	}
}

func TestReadRegularFileFilesystemUnsupportedBeforeMetadata(t *testing.T) {
	if regularFilesystemReadSupported {
		t.Skip("platform supports safe filesystem descriptor reads")
	}
	// A nil root panics if the unsupported capability touches metadata.
	src := &FilesystemSource{}
	data, _, err := ReadRegularFile(t.Context(), src, "nested/go.mod", 64)
	if data != nil || !errors.Is(err, ErrRegularReadUnsupported) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unsupported filesystem = %q, %v", data, err)
	}
}

func TestReadRegularFileFilesystem(t *testing.T) {
	root := t.TempDir()
	regularWrite(t, root, "go.mod", "module example.org/test\n")
	regularWrite(t, root, "large.mod", strings.Repeat("x", 65))
	if err := os.Mkdir(filepath.Join(root, "directory.mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := regularFilesystem(t, root)
	data, meta, err := ReadRegularFile(t.Context(), src, "go.mod", 64)
	if errors.Is(err, ErrRegularReadUnsupported) {
		t.Skip("platform has no safe descriptor opener")
	}
	if err != nil || string(data) != "module example.org/test\n" || meta.Path != "go.mod" {
		t.Fatalf("regular read = %q, %+v, %v", data, meta, err)
	}
	for _, tc := range []struct {
		name string
		want error
	}{
		{"missing.mod", fs.ErrNotExist}, {"directory.mod", ErrNotRegularFile},
		{"large.mod", ErrRegularFileTooLarge}, {"../outside.mod", ErrOutsideRoot},
		{".", ErrNotInSource}, {"", ErrNotInSource}, {"pkg/..", ErrNotInSource}, {"/", ErrOutsideRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _, err := ReadRegularFile(t.Context(), src, tc.name, 64)
			if data != nil || !errors.Is(err, tc.want) {
				t.Fatalf("read = %q, %v; want %v", data, err, tc.want)
			}
			if tc.want == fs.ErrNotExist && !errors.Is(err, ErrNotInSource) {
				t.Fatalf("absence lost compatibility: %v", err)
			}
			if tc.want != fs.ErrNotExist && errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("non-absence mislabeled: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	data, _, err = ReadRegularFile(ctx, src, "go.mod", 64)
	if data != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = %q, %v", data, err)
	}
}

func TestReadRegularFileLayeredOwnership(t *testing.T) {
	lowerRoot, upperRoot := t.TempDir(), t.TempDir()
	regularWrite(t, lowerRoot, "go.mod", "module lower\n")
	regularWrite(t, upperRoot, "other.mod", "module upper\n")
	if err := os.Mkdir(filepath.Join(upperRoot, "directory.mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	regularWrite(t, lowerRoot, "directory.mod", "must not escape upper boundary")
	lower, upper := regularFilesystem(t, lowerRoot), regularFilesystem(t, upperRoot)
	if _, _, err := ReadRegularFile(t.Context(), lower, "go.mod", 64); errors.Is(err, ErrRegularReadUnsupported) {
		t.Skip("platform has no safe descriptor opener")
	}
	for _, tc := range []struct {
		name       string
		upper      ContentSource
		owns       bool
		path, want string
		wantErr    error
	}{
		{"lower", upper, false, "go.mod", "module lower\n", nil},
		{"upper", upper, true, "other.mod", "module upper\n", nil},
		{"upper deletion", upper, true, "go.mod", "", fs.ErrNotExist},
		{"upper nonregular", upper, true, "directory.mod", "", ErrNotRegularFile},
		{"unsupported upper", unsafeRegularSource{}, true, "go.mod", "", ErrRegularReadUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layered := &LayeredSource{upper: tc.upper, lower: lower, upperOwns: func(string) bool { return tc.owns }}
			data, _, err := ReadRegularFile(t.Context(), layered, tc.path, 64)
			if !errors.Is(err, tc.wantErr) || string(data) != tc.want || (err != nil && data != nil) {
				t.Fatalf("layered = %q, %v; want %q, %v", data, err, tc.want, tc.wantErr)
			}
		})
	}
}

func regularFilesystem(t testing.TB, root string) *FilesystemSource {
	t.Helper()
	src, err := NewFilesystemSource(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := src.Close(); err != nil {
			t.Error(err)
		}
	})
	return src
}
func regularWrite(t testing.TB, root, name, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
func regularGit(t testing.TB, root, input string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func regularGitRepo(t testing.TB) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	regularGit(t, root, "", "init", "-q")
	regularGit(t, root, "", "config", "user.name", "Gortex Test")
	regularGit(t, root, "", "config", "user.email", "test@gortex.invalid")
	return root
}
func regularGitTree(t testing.TB, root, tree string) *GitTreeSource {
	t.Helper()
	src, err := NewGitTreeSource(context.Background(), root, tree)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := src.Close(); err != nil {
			t.Error(err)
		}
	})
	return src
}

func TestReadRegularFileGitInventoryAndAllocationGates(t *testing.T) {
	root := regularGitRepo(t)
	small := regularGit(t, root, "module example.org/test\n", "hash-object", "-w", "--stdin")
	large := regularGit(t, root, strings.Repeat("x", 128), "hash-object", "-w", "--stdin")
	empty := regularGit(t, root, "", "mktree")
	commit := regularGit(t, root, "", "commit-tree", empty, "-m", "gitlink fixture")
	tree := regularGit(t, root, fmt.Sprintf(
		"100644 blob %s\tgo.mod\n100644 blob %s\tlarge.mod\n120000 blob %s\tlink.mod\n040000 tree %s\tempty.mod\n160000 commit %s\tgitlink.mod\n",
		small, large, large, empty, commit), "mktree")
	src := regularGitTree(t, root, tree)
	for _, tc := range []struct {
		name string
		want error
	}{
		{"absent.mod", fs.ErrNotExist}, {"empty.mod", ErrNotRegularFile}, {"gitlink.mod", ErrNotRegularFile},
		{"link.mod", ErrNotRegularFile}, {"large.mod", ErrRegularFileTooLarge},
		{"go.mod/go.mod", ErrNotRegularFile}, {"link.mod/go.mod", ErrNotRegularFile},
		{"gitlink.mod/go.mod", ErrNotRegularFile}, {"gitlink.mod/pkg/go.mod", ErrNotRegularFile},
		{"empty.mod/go.mod", fs.ErrNotExist},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _, err := ReadRegularFile(t.Context(), src, tc.name, 64)
			if data != nil || !errors.Is(err, tc.want) {
				t.Fatalf("read = %q, %v; want %v", data, err, tc.want)
			}
			if tc.want == fs.ErrNotExist && !errors.Is(err, ErrNotInSource) {
				t.Fatalf("absence lost compatibility: %v", err)
			}
			if tc.want != fs.ErrNotExist && errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("unsupported entry labeled absent: %v", err)
			}
			if got := src.spawnCount(); got != 0 {
				t.Fatalf("metadata rejection spawned %d readers", got)
			}
		})
	}
	data, _, err := ReadRegularFile(t.Context(), src, "go.mod", 64)
	if err != nil || string(data) != "module example.org/test\n" {
		t.Fatalf("regular Git read = %q, %v", data, err)
	}
	if got := walkPaths(t, src); !slices.Equal(got, []string{"go.mod", "large.mod", "link.mod"}) {
		t.Fatalf("legacy Walk = %v", got)
	}
	for _, name := range []string{"empty.mod", "gitlink.mod"} {
		if _, err := src.Stat(name); !errors.Is(err, ErrNotInSource) {
			t.Fatalf("legacy Stat(%s) = %v", name, err)
		}
		rc, _, err := src.Open(name)
		if rc != nil {
			rc.Close()
			t.Fatalf("legacy Open(%s) returned reader", name)
		}
		if !errors.Is(err, ErrNotInSource) {
			t.Fatalf("legacy Open(%s) = %v", name, err)
		}
	}
}
func TestReadRegularFileGitEmptyRootHasCompleteInventory(t *testing.T) {
	root := regularGitRepo(t)
	src := regularGitTree(t, root, regularGit(t, root, "", "mktree"))
	data, _, err := ReadRegularFile(t.Context(), src, "go.mod", 64)
	if data != nil || !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, ErrNotInSource) {
		t.Fatalf("empty-tree absence = %q, %v", data, err)
	}
	if src.spawnCount() != 0 {
		t.Fatal("absence read a blob")
	}
}
func TestReadRegularFileGitMissingBlobHasUnknownSize(t *testing.T) {
	root := regularGitRepo(t)
	oid := regularGit(t, root, "module unavailable\n", "hash-object", "-w", "--stdin")
	tree := regularGit(t, root, fmt.Sprintf("100644 blob %s\tgo.mod\n", oid), "mktree")
	if err := os.Remove(filepath.Join(root, ".git", "objects", oid[:2], oid[2:])); err != nil {
		t.Fatal(err)
	}
	src := regularGitTree(t, root, tree)
	data, meta, err := ReadRegularFile(t.Context(), src, "go.mod", 64)
	if data != nil || meta.Size >= 0 || !errors.Is(err, ErrRegularFileSizeUnknown) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unknown size = %q, %+v, %v", data, meta, err)
	}
	if src.spawnCount() != 0 {
		t.Fatal("unknown size materialized a blob")
	}
}

type regularBatchInput struct{ bytes.Buffer }

func (*regularBatchInput) Close() error { return nil }

func TestGitBatchBoundedHeaderRejectsBeforePayloadAllocation(t *testing.T) {
	oid := strings.Repeat("1", 40)
	input := &regularBatchInput{}
	batch := &gitBatch{stdin: input, out: bufio.NewReader(strings.NewReader(oid + " blob 1099511627776\n")), stderr: &syncBuffer{}, delim: '\n'}
	data, err := batch.readBounded(oid, 64)
	if data != nil || !errors.Is(err, ErrRegularFileTooLarge) || batch.broken == nil {
		t.Fatalf("bounded header = %q, %v; broken=%v", data, err, batch.broken)
	}
	// No body exists: a drain would return EOF instead of the size error.
	written := input.Len()
	if _, err := batch.read(oid); !errors.Is(err, ErrRegularFileTooLarge) || input.Len() != written {
		t.Fatalf("broken batch reused: err=%v bytes=%d->%d", err, written, input.Len())
	}
}
func TestReadRegularFileGitRetiresOversizedBatchAndRecreates(t *testing.T) {
	root := regularGitRepo(t)
	small := regularGit(t, root, "module small\n", "hash-object", "-w", "--stdin")
	large := regularGit(t, root, strings.Repeat("x", 128), "hash-object", "-w", "--stdin")
	tree := regularGit(t, root, fmt.Sprintf("100644 blob %s\tgo.mod\n100644 blob %s\tlarge.mod\n", small, large), "mktree")
	src := regularGitTree(t, root, tree)
	// Synthetic corrupt metadata; the actual Git header must still gate size.
	src.entries["large.mod"].meta.Size = 1
	data, _, err := ReadRegularFile(t.Context(), src, "large.mod", 64)
	if data != nil || !errors.Is(err, ErrRegularFileTooLarge) {
		t.Fatalf("oversized header = %q, %v", data, err)
	}
	if src.batch != nil || src.spawnCount() != 1 {
		t.Fatalf("failed batch not retired: %p, spawns=%d", src.batch, src.spawnCount())
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, _, err := ReadRegularFile(t.Context(), src, "go.mod", 64)
			if err != nil || string(data) != "module small\n" {
				errs <- fmt.Errorf("recovery read %q: %w", data, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if src.spawnCount() != 2 {
		t.Fatalf("recovery spawned %d batches", src.spawnCount())
	}
	if content, _ := readAll(t, src, "go.mod"); content != "module small\n" {
		t.Fatalf("legacy recovery=%q", content)
	}
}
func TestReadRegularFileGitCanceledBeforeReadDoesNotSpawn(t *testing.T) {
	root := regularGitRepo(t)
	oid := regularGit(t, root, "module small\n", "hash-object", "-w", "--stdin")
	tree := regularGit(t, root, fmt.Sprintf("100644 blob %s\tgo.mod\n", oid), "mktree")
	src := regularGitTree(t, root, tree)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	data, _, err := ReadRegularFile(ctx, src, "go.mod", 64)
	if data != nil || !errors.Is(err, context.Canceled) || src.spawnCount() != 0 {
		t.Fatalf("canceled read=%q,%v; spawns=%d", data, err, src.spawnCount())
	}
}
func TestGitBatchBoundedHeaderRejectsUnboundedFramingAndMismatches(t *testing.T) {
	oid := strings.Repeat("1", 40)
	for _, tc := range []struct{ name, header, want string }{
		{"overlong", strings.Repeat("x", 1024), "header exceeds"},
		{"wrong object", strings.Repeat("2", 40) + " blob 16\n", "unexpected response"},
		{"not a blob", oid + " tree 16\n", "unexpected response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := &regularBatchInput{}
			batch := &gitBatch{stdin: input, out: bufio.NewReader(strings.NewReader(tc.header)), stderr: &syncBuffer{}, delim: '\n'}
			data, err := batch.readBounded(oid, 64)
			if data != nil || err == nil || !strings.Contains(err.Error(), tc.want) || batch.broken == nil {
				t.Fatalf("bad header=%q,%v; broken=%v", data, err, batch.broken)
			}
			if tc.name == "overlong" && batch.out.Buffered() != len(tc.header)-maxBoundedGitHeaderBytes {
				t.Fatalf("overlong header drained: %d buffered", batch.out.Buffered())
			}
			written := input.Len()
			if _, err := batch.readBounded(oid, 64); err == nil || input.Len() != written {
				t.Fatalf("desynchronized process reused: %v", err)
			}
		})
	}
}

func BenchmarkRegularGitInventory(b *testing.B) {
	for _, filesPerDir := range []int{1, 10} {
		b.Run(fmt.Sprintf("100dirs_%dfiles", filesPerDir), func(b *testing.B) {
			root := regularGitRepo(b)
			regularWrite(b, root, "go.mod", "module example.org/benchmark\n")
			for dir := range 100 {
				for file := range filesPerDir {
					regularWrite(b, root, fmt.Sprintf("pkg%03d/file%03d.go", dir, file), "package fixture\n")
				}
			}
			regularGit(b, root, "", "add", "--", ".")
			tree := regularGit(b, root, "", "write-tree")
			oldListing, err := runGit(b.Context(), root, "ls-tree", "-r", "-l", "-z", "--full-tree", tree)
			if err != nil {
				b.Fatal(err)
			}
			fullListing, err := runGit(b.Context(), root, "ls-tree", "-r", "-t", "-l", "-z", "--full-tree", tree)
			if err != nil {
				b.Fatal(err)
			}
			for _, complete := range []bool{false, true} {
				label := "blob_only"
				if complete {
					label = "complete"
				}
				b.Run("constructor_"+label, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						var src *GitTreeSource
						var err error
						if complete {
							src, err = NewGitTreeSource(b.Context(), root, tree)
						} else {
							src, err = regularLegacyGitConstructor(b.Context(), root, tree)
						}
						if err != nil {
							b.Fatal(err)
						}
						if len(src.order) != 100*filesPerDir+1 {
							b.Fatalf("blob inventory=%d", len(src.order))
						}
						if err := src.Close(); err != nil {
							b.Fatal(err)
						}
					}
				})
				b.Run("parse_"+label, func(b *testing.B) {
					listing := oldListing
					if complete {
						listing = fullListing
					}
					b.ReportAllocs()
					b.ReportMetric(float64(len(listing)), "listing_bytes")
					for b.Loop() {
						order, _, metadata, err := parseTreeInventory(listing, complete)
						if err != nil {
							b.Fatal(err)
						}
						if len(order) != 100*filesPerDir+1 {
							b.Fatalf("blob inventory=%d", len(order))
						}
						if complete && len(metadata) != 100 {
							b.Fatalf("non-content inventory=%d", len(metadata))
						}
					}
				})
			}
		})
	}
}

// Successful constructor path before non-content inventory: same resolution
// and single ls-tree subprocess. No production compatibility switch is added.
func regularLegacyGitConstructor(ctx context.Context, root, tree string) (*GitTreeSource, error) {
	abs, resolved, err := resolveGitTreeOID(ctx, root, tree)
	if err != nil {
		return nil, err
	}
	listing, err := runGit(ctx, abs, "ls-tree", "-r", "-l", "-z", "--full-tree", resolved)
	if err != nil {
		return nil, err
	}
	order, entries, err := parseTreeListing(listing)
	if err != nil {
		return nil, err
	}
	return &GitTreeSource{repoDir: abs, treeOID: resolved, order: order, entries: entries}, nil
}

func BenchmarkReadRegularManifest(b *testing.B) {
	root := regularGitRepo(b)
	regularWrite(b, root, "go.mod", "module example.org/benchmark\n\ngo 1.26\n")
	regularGit(b, root, "", "add", "--", ".")
	tree := regularGit(b, root, "", "write-tree")
	fsSource := regularFilesystem(b, root)
	gitSource := regularGitTree(b, root, tree)
	for _, tc := range []struct {
		name string
		src  ContentSource
	}{{"filesystem", fsSource}, {"git", gitSource}} {
		b.Run(tc.name, func(b *testing.B) {
			for _, safe := range []bool{false, true} {
				label := "open"
				if safe {
					label = "safe"
					if _, _, err := ReadRegularFile(b.Context(), tc.src, "go.mod", 64<<10); errors.Is(err, ErrRegularReadUnsupported) {
						continue
					} else if err != nil {
						b.Fatal(err)
					}
				} else {
					// Compare steady reads on both paths; the Git batch process
					// must not be cold only for the legacy Open measurement.
					reader, _, err := tc.src.Open("go.mod")
					if err != nil {
						b.Fatal(err)
					}
					data, readErr := io.ReadAll(reader)
					closeErr := reader.Close()
					if readErr != nil || closeErr != nil || len(data) == 0 {
						b.Fatalf("warm open %d bytes: read=%v close=%v", len(data), readErr, closeErr)
					}
				}
				b.Run(label, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if safe {
							data, _, err := ReadRegularFile(b.Context(), tc.src, "go.mod", 64<<10)
							if err != nil || len(data) == 0 {
								b.Fatalf("safe %d bytes: %v", len(data), err)
							}
						} else {
							reader, _, err := tc.src.Open("go.mod")
							if err != nil {
								b.Fatal(err)
							}
							data, readErr := io.ReadAll(reader)
							closeErr := reader.Close()
							if readErr != nil || closeErr != nil || len(data) == 0 {
								b.Fatalf("open %d bytes: read=%v close=%v", len(data), readErr, closeErr)
							}
						}
					}
				})
			}
		})
	}
}
