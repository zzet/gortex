package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// prunedLink is an excerpt of a -dumpdep link in which the linker can still
// drop unused methods. MethodByName with a constant name is fine: the compiler
// records the name, so only matching methods are kept.
const prunedLink = `# github.com/zzet/gortex/cmd/gortex
_ -> runtime.main
_ -> main.main
main.main -> main.runDaemonInstallService
main.runDaemonInstallService -> main.renderSystemdUnit
main.renderSystemdUnit -> fmt.Fprintf
main.renderSystemdUnit -> type:main.serviceEnvVar <UsedInIface>
google.golang.org/protobuf/internal/descfmt.formatListOpt -> reflect.Value.MethodByName
`

func runDumpdep(t *testing.T, input string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(strings.NewReader(input), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestDumpdepAcceptsALinkThatPrunesMethods(t *testing.T) {
	code, stdout, stderr := runDumpdep(t, prunedLink)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "the linker can drop unused methods") {
		t.Fatalf("clean link rejected: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
}

func TestDumpdepRejectsReachableMethodLookups(t *testing.T) {
	input := prunedLink + `main.main -> main.installLaunchd
main.installLaunchd -> main.renderLaunchdPlist
main.renderLaunchdPlist -> text/template.(*Template).execute
text/template.(*Template).execute -> text/template.(*state).walk
text/template.(*state).walk -> text/template.(*state).evalField <ReflectMethod>
text/template.(*state).evalField <ReflectMethod> -> reflect.Value.MethodByName
type:reflect.Value <UsedInIface> -> reflect.(*Value).Method <UsedInIface><ReflectMethod>
`
	code, stdout, stderr := runDumpdep(t, input)
	if code == 0 || stdout != "" {
		t.Fatalf("reachable method lookups accepted: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
	for _, want := range []string{
		"ci-dumpdep: 3 reachable symbols",
		// Execute is usually inlined, so its caller reaches execute directly
		// and the chain shows which first-party code runs a template.
		"\ntext/template.(*Template).execute, reached by:\n" +
			"  main.main\n  main.installLaunchd\n  main.renderLaunchdPlist\n  text/template.(*Template).execute\n",
		"\ntext/template.(*state).evalField, reached by:\n" +
			"  main.main\n  main.installLaunchd\n  main.renderLaunchdPlist\n  text/template.(*Template).execute\n" +
			"  text/template.(*state).walk\n  text/template.(*state).evalField\n",
		"\nreflect.(*Value).Method, reached by:\n  type:reflect.Value\n  reflect.(*Value).Method\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("report is missing %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "MethodByName, reached by") {
		t.Errorf("the lookup function itself is not flagged; only its dynamic callers are:\n%s", stderr)
	}
}

func TestDumpdepRejectsInputThatIsNotALinkListing(t *testing.T) {
	for _, tc := range []struct{ name, input, echoed string }{
		{"empty", "", ""},
		// A failed build must still show why it failed.
		{"build failure", "# github.com/zzet/gortex/cmd/gortex\n./main.go:1:1: expected 'package', found 'EOF'\n",
			"The input began:\n# github.com/zzet/gortex/cmd/gortex\n./main.go:1:1: expected 'package', found 'EOF'\n"},
		{"library", "_ -> runtime.main\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runDumpdep(t, tc.input)
			if code == 0 || stdout != "" || !strings.Contains(stderr, "main.main is not in the input") ||
				!strings.Contains(stderr, tc.echoed) {
				t.Fatalf("non-listing accepted: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
			}
		})
	}
}

// TestDumpdepRejectsAListingWithoutFlags guards against a listing format the
// tool cannot read: if the linker stopped appending attribute flags, a
// <ReflectMethod> would go unseen and every link would pass.
func TestDumpdepRejectsAListingWithoutFlags(t *testing.T) {
	input := strings.ReplaceAll(prunedLink, " <UsedInIface>", "")
	code, stdout, stderr := runDumpdep(t, input)
	if code == 0 || stdout != "" || !strings.Contains(stderr, "not a -dumpdep listing") {
		t.Fatalf("listing without flags accepted: exit=%d\nstdout=%s\nstderr=%s", code, stdout, stderr)
	}
}

// TestCIWorkflowChecksTheReleaseLink keeps the check wired to a -dumpdep link
// with the release build tags and link flags of each platform, with a
// pipeline that fails when the link does.
func TestCIWorkflowChecksTheReleaseLink(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	for name, wants := range map[string][]string{
		"build-linux-static": {
			"go build -tags netgo,osusergo,static_link",
			"-dumpdep",
			"-o /dev/null ./cmd/gortex/ 2>&1 | go run ./scripts/ci-dumpdep",
		},
		// darwin and windows: no tags; windows links its runtime statically.
		"build-native": {
			"shell: bash",
			"extld=-extldflags=-static",
			`go build -ldflags "-s -w $extld -dumpdep"`,
			"./cmd/gortex/ 2>&1 | go run ./scripts/ci-dumpdep",
		},
	} {
		_, job, found := strings.Cut(text, "\n  "+name+":\n")
		if !found {
			t.Fatalf("ci.yml has no %s job", name)
		}
		if next := regexp.MustCompile(`\n  [a-z][a-z0-9-]*:\n`).FindStringIndex(job); next != nil {
			job = job[:next[0]]
		}
		for _, want := range append(wants, "set -o pipefail") {
			if !strings.Contains(job, want) {
				t.Errorf("%s is missing %q", name, want)
			}
		}
	}
}
