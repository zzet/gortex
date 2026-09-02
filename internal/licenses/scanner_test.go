package licenses

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestScan_Variants(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "go-style",
			src:  "// SPDX-License-Identifier: MIT\npackage main\n",
			want: "MIT",
		},
		{
			name: "python-style",
			src:  "# SPDX-License-Identifier: Apache-2.0\nimport os\n",
			want: "Apache-2.0",
		},
		{
			name: "block-comment-leading-star",
			src:  "/*\n * SPDX-License-Identifier: BSD-3-Clause\n */\n",
			want: "BSD-3-Clause",
		},
		{
			name: "with-expression",
			src:  "// SPDX-License-Identifier: MIT OR Apache-2.0\n",
			want: "MIT OR Apache-2.0",
		},
		{
			name: "no-header",
			src:  "package main\n\nfunc main() {}\n",
			want: "",
		},
		{
			name: "below-window",
			src:  "// line 1\n// 2\n// 3\n// 4\n// 5\n// 6\n// 7\n// 8\n// 9\n// 10\n// SPDX-License-Identifier: MIT\n",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scan([]byte(tc.src))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildGraphArtifacts(t *testing.T) {
	nodes, edges := BuildGraphArtifacts("pkg/foo.go", "MIT", "go")
	if len(nodes) != 1 || nodes[0].Kind != graph.KindLicense {
		t.Fatalf("nodes = %+v", nodes)
	}
	if nodes[0].ID != "license::MIT" {
		t.Errorf("id = %q", nodes[0].ID)
	}
	if nodes[0].Name != "MIT" {
		t.Errorf("name = %q", nodes[0].Name)
	}
	if len(edges) != 1 || edges[0].Kind != graph.EdgeLicensedAs {
		t.Fatalf("edges = %+v", edges)
	}
	if edges[0].From != "pkg/foo.go" || edges[0].To != "license::MIT" {
		t.Errorf("edge endpoints wrong: %s -> %s", edges[0].From, edges[0].To)
	}
}

func TestBuildGraphArtifacts_PreservesCallerPathSpelling(t *testing.T) {
	// The indexer keys eviction and incremental replacement by the
	// exact relPath spelling it hands the builder; a re-spelled edge
	// endpoint dangles from a nonexistent file node on Windows. The
	// spelling is written out, not composed with filepath.Join: on a
	// POSIX runner Join yields exactly what the pre-fix ToSlash call
	// returned, so the assertion would hold either way.
	const rel = `src\data\foo.go`
	nodes, edges := BuildGraphArtifacts(rel, "MIT", "go")
	if len(nodes) != 1 || len(edges) != 1 {
		t.Fatalf("nodes = %d, edges = %d", len(nodes), len(edges))
	}
	if nodes[0].FilePath != rel {
		t.Errorf("node file path = %q, want %q", nodes[0].FilePath, rel)
	}
	if edges[0].From != rel {
		t.Errorf("edge.From = %q, want %q", edges[0].From, rel)
	}
	if edges[0].FilePath != rel {
		t.Errorf("edge file path = %q, want %q", edges[0].FilePath, rel)
	}
}

func TestBuildGraphArtifacts_NoSPDXReturnsEmpty(t *testing.T) {
	nodes, edges := BuildGraphArtifacts("pkg/foo.go", "", "go")
	if len(nodes) != 0 || len(edges) != 0 {
		t.Errorf("expected empty, got %d nodes / %d edges", len(nodes), len(edges))
	}
}
