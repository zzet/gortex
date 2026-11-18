package citest

import (
	"fmt"
	"slices"
	"strings"
)

func (j *job) report() error {
	type duration struct {
		name    string
		seconds float64
	}
	slowest := make([]duration, 0, len(j.durations))
	for name, seconds := range j.durations {
		slowest = append(slowest, duration{name, seconds})
	}
	slices.SortFunc(slowest, func(a, b duration) int {
		if a.seconds > b.seconds {
			return -1
		}
		if a.seconds < b.seconds {
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	slowest = slowest[:min(20, len(slowest))]
	var summary strings.Builder
	summary.WriteString("### Slowest tests\n\n| seconds | test |\n| ---: | --- |\n")
	j.printf("\nSlowest tests:\n")
	for _, d := range slowest {
		fmt.Fprintf(&summary, "| %.1f | `%s` |\n", d.seconds, d.name)
		j.printf("  %8.1fs %s\n", d.seconds, d.name)
	}
	for _, e := range j.flaky {
		j.printf("\n::group::FLAKY %s (first run's output)\n%s\n::endgroup::\n", e.Title, strings.TrimRight(e.Output, "\n"))
		j.printf("::warning title=Flaky test::%s failed beside other tests and passed when rerun alone\n", e.Title)
	}
	if len(j.flaky) > 0 {
		summary.WriteString("\n### Flaky: failed beside other tests, passed rerun alone\n\n")
		for _, e := range j.flaky {
			fmt.Fprintf(&summary, "- `%s`\n", e.Title)
		}
	}
	for _, e := range j.failures {
		j.printf("\n::group::FAIL %s\n%s\n::endgroup::\n", e.Title, strings.TrimRight(e.Output, "\n"))
		j.printf("::error title=Test failed::%s\n", e.Title)
	}
	if len(j.failures) > 0 {
		summary.WriteString("\n### Failed\n\n")
		for _, e := range j.failures {
			fmt.Fprintf(&summary, "- `%s`\n", e.Title)
		}
	}
	return appendFile(envValue(j.env, "GITHUB_STEP_SUMMARY"), summary.String())
}
