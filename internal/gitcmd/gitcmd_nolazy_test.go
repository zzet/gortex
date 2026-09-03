package gitcmd

import (
	"context"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/testutil/gitpromisor"
)

func TestNoLazyGitEnvDeduplicatesFixedOverrides(t *testing.T) {
	base := []string{
		"KEEP=first",
		"GIT_NO_LAZY_FETCH=0",
		"GIT_TERMINAL_PROMPT=1",
		"KEEP=second",
		"GIT_NO_LAZY_FETCH=maybe",
		"GIT_OPTIONAL_LOCKS=1",
	}
	env := make([]string, 0, len(base)+len(fixedNoLazyGitEnv))
	for _, entry := range base {
		key := entry
		for i := range entry {
			if entry[i] == '=' {
				key = entry[:i]
				break
			}
		}
		if !isFixedNoLazyGitEnvKey(key) {
			env = append(env, entry)
		}
	}
	env = append(env, fixedNoLazyGitEnv...)
	want := []string{
		"KEEP=first",
		"KEEP=second",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	}
	if !slices.Equal(env, want) {
		t.Fatalf("deduped no-lazy environment = %#v, want %#v", env, want)
	}
}

func TestRunNoLazyNeverFetchesPromisedBlob(t *testing.T) {
	fixture := gitpromisor.New(t)
	subject := fixture.Clone(t, "blob:none")
	if subject.ObjectPresent(t, fixture.NestedBlobOID) {
		t.Fatalf("blob:none fixture unexpectedly contains %s", fixture.NestedBlobOID)
	}
	control := fixture.Clone(t, "blob:none")
	control.FetchAndRequireRequest(t, fixture.NestedBlobOID)

	if _, err := RunNoLazy(context.Background(), subject.Dir, "cat-file", "-p", fixture.NestedBlobOID); err == nil {
		t.Fatal("RunNoLazy() unexpectedly read a promised missing blob")
	}
	if got := subject.RequestCount(t); got != 0 {
		t.Fatalf("upload-pack requests = %d, want 0", got)
	}
	if subject.ObjectPresent(t, fixture.NestedBlobOID) {
		t.Fatal("RunNoLazy() materialized the promised blob")
	}
}

func BenchmarkRunNoLazyPromisorBlob(b *testing.B) {
	fixture := gitpromisor.New(b)
	complete := fixture.Clone(b, "blob:limit=1m")
	missing := fixture.Clone(b, "blob:none")
	benchmarkRunNoLazyBlob(b, "complete", complete, fixture.NestedBlobOID, false)
	benchmarkRunNoLazyBlob(b, "missing", missing, fixture.NestedBlobOID, true)
}

func benchmarkRunNoLazyBlob(b *testing.B, name string, client *gitpromisor.Client, oid string, wantError bool) {
	b.Helper()
	b.Run(name, func(b *testing.B) {
		client.ResetRequests(b)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			_, err := RunNoLazy(context.Background(), client.Dir, "cat-file", "-p", oid)
			if (err != nil) != wantError {
				b.Fatalf("RunNoLazy() error = %v, want error=%t", err, wantError)
			}
		}
		b.StopTimer()
		requests := client.RequestCount(b)
		b.ReportMetric(float64(requests)/float64(b.N), "upload-pack/op")
		if requests != 0 {
			b.Fatalf("upload-pack requests = %d, want 0", requests)
		}
	})
}
