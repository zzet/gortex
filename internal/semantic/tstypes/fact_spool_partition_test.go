package tstypes

import (
	"context"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func sampleFacts() *fileFacts {
	return &fileFacts{
		file: "a/b.cs", repoPrefix: "repo",
		imports: []Import{{Local: "Sys"}},
		supers:  []superFact{{typeName: "A", superName: "B", kind: graph.EdgeExtends, line: 1}},
		metas:   []metaFact{{key: "return_type", value: "C", owner: "A", name: "M", line: 2}},
		calls: []callFact{{line: 3, method: "M", recvType: "C", owner: "Q",
			recvChain: &callFact{line: 3, method: "N"}, argCount: 2, argKnown: true}},
		// aliases deliberately empty — must be absent from the payload map
	}
}

func TestMarshalClassPayloadsRoundTrip(t *testing.T) {
	src := sampleFacts()
	payloads, err := marshalClassPayloads(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := payloads[classAliases]; ok {
		t.Fatal("empty aliases class must be omitted")
	}
	for _, class := range []factClass{classImports, classSupers, classMetas, classCalls} {
		if len(payloads[class]) == 0 {
			t.Fatalf("class %d missing payload", class)
		}
	}
	dst := &fileFacts{file: src.file, repoPrefix: src.repoPrefix}
	for class, payload := range payloads {
		if err := unmarshalClassPayload(dst, class, payload); err != nil {
			t.Fatalf("class %d: %v", class, err)
		}
	}
	if len(dst.imports) != 1 || dst.imports[0].Local != "Sys" {
		t.Fatalf("imports lost: %+v", dst.imports)
	}
	if len(dst.supers) != 1 || dst.supers[0].superName != "B" {
		t.Fatalf("supers lost: %+v", dst.supers)
	}
	if len(dst.metas) != 1 || dst.metas[0].value != "C" {
		t.Fatalf("metas lost: %+v", dst.metas)
	}
	if len(dst.calls) != 1 || dst.calls[0].recvChain == nil || dst.calls[0].recvChain.method != "N" {
		t.Fatalf("calls (incl. chain) lost: %+v", dst.calls)
	}
	if dst.calls[0].owner != "Q" {
		t.Fatalf("authored owner lost across the spool: %+v", dst.calls[0])
	}
	if len(dst.aliases) != 0 {
		t.Fatalf("aliases should stay empty: %+v", dst.aliases)
	}
}

func TestAppendFilesWritesClassRows(t *testing.T) {
	spool, err := newFactSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	record, err := stageFileFacts(sampleFacts())
	if err != nil {
		t.Fatal(err)
	}
	if record.bytes <= 0 {
		t.Fatal("staged bytes must sum class payloads")
	}
	if err := spool.appendFiles([]stagedFileFacts{record}); err != nil {
		t.Fatal(err)
	}
	var fileRows, classRows, aliasClassRows int
	if err := spool.db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&fileRows); err != nil {
		t.Fatal(err)
	}
	if err := spool.db.QueryRow(`SELECT COUNT(*) FROM file_facts`).Scan(&classRows); err != nil {
		t.Fatal(err)
	}
	if err := spool.db.QueryRow(`SELECT COUNT(*) FROM file_facts WHERE class=3`).Scan(&aliasClassRows); err != nil {
		t.Fatal(err)
	}
	if fileRows != 1 || classRows != 4 || aliasClassRows != 0 {
		t.Fatalf("rows: files=%d classes=%d aliases=%d (want 1/4/0)", fileRows, classRows, aliasClassRows)
	}
}

func TestPageClassReturnsOnlyOwnClassPlusImports(t *testing.T) {
	spool, err := newFactSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	full, err := stageFileFacts(sampleFacts())
	if err != nil {
		t.Fatal(err)
	}
	callsOnly, err := stageFileFacts(&fileFacts{
		file: "z/calls_only.cs", repoPrefix: "repo",
		calls: []callFact{{line: 1, method: "Only"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.appendFiles([]stagedFileFacts{full, callsOnly}); err != nil {
		t.Fatal(err)
	}

	supersPage, last, _, err := spool.pageClass(context.Background(), classSupers, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(supersPage) != 1 || supersPage[0].file != "a/b.cs" {
		t.Fatalf("supers walk must skip files without supers: %+v", supersPage)
	}
	got := supersPage[0]
	if len(got.supers) != 1 || len(got.imports) != 1 {
		t.Fatalf("supers page must carry supers+imports: %+v", got)
	}
	if len(got.calls) != 0 || len(got.metas) != 0 || len(got.aliases) != 0 {
		t.Fatalf("other classes must stay empty: %+v", got)
	}
	if next, _, _, err := spool.pageClass(context.Background(), classSupers, last); err != nil || len(next) != 0 {
		t.Fatalf("keyset must terminate: %v %d", err, len(next))
	}

	callsPage, _, _, err := spool.pageClass(context.Background(), classCalls, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(callsPage) != 2 {
		t.Fatalf("calls walk must see both files: %d", len(callsPage))
	}

	filesPage, _, err := spool.pageFiles(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(filesPage) != 2 || filesPage[0].file != "a/b.cs" || filesPage[1].file != "z/calls_only.cs" {
		t.Fatalf("files walk must list every staged file in order: %+v", filesPage)
	}
}

func TestPageClassRespectsByteCap(t *testing.T) {
	spool, err := newFactSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	big := strings.Repeat("x", 3<<20) // two ~3 MiB call payloads exceed the 4 MiB page cap
	staged := make([]stagedFileFacts, 0, 2)
	for _, name := range []string{"a/one.cs", "a/two.cs"} {
		record, err := stageFileFacts(&fileFacts{
			file: name, repoPrefix: "repo",
			calls: []callFact{{line: 1, method: big}},
		})
		if err != nil {
			t.Fatal(err)
		}
		staged = append(staged, record)
	}
	if err := spool.appendFiles(staged); err != nil {
		t.Fatal(err)
	}
	page, last, _, err := spool.pageClass(context.Background(), classCalls, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || last != "a/one.cs" {
		t.Fatalf("byte cap must stop after the first oversized row: %d files, last=%q", len(page), last)
	}
}
