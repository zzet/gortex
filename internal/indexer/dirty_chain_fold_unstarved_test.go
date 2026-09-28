package indexer

import (
	"fmt"
	"testing"
)

// chainBurstEdit writes the i-th edit of a burst: one of four files of the
// fixture tree, round-robin, each time with one more statement in its body.
func chainBurstEdit(t *testing.T, f *coordinatorFixture, i int) {
	t.Helper()
	files := []string{"island.go", "helper.go", "caller.go", "core.go"}
	names := map[string]string{"island.go": "Island", "helper.go": "Helper", "caller.go": "Run", "core.go": "Compute"}
	file := files[i%len(files)]
	body := ""
	for k := 0; k <= i; k++ {
		body += fmt.Sprintf("\t_ = %d\n", k)
	}
	content := fmt.Sprintf("package fixture\n\nfunc %s() {\n%s}\n", names[file], body)
	if file == "core.go" {
		content = fmt.Sprintf("package fixture\n\ntype Options struct{}\n\nfunc Compute(o Options) {\n%s\tHelper()\n}\n", body)
	}
	if file == "caller.go" {
		content = fmt.Sprintf("package fixture\n\nfunc Run() {\n%s\tCompute(Options{})\n}\n", body)
	}
	builderWriteFile(t, f.worktree, file, content)
}
