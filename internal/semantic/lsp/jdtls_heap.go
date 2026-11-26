package lsp

import (
	"os"
	"strconv"
	"strings"
)

// JdtlsMaxHeapEnv sets the maximum Java heap of every jdtls Gortex spawns, in
// the JVM's -Xmx syntax ("2G", "3072m"). "off" (or "0" / "none") passes no cap
// and leaves the JVM's own default in force. It is read from the server's
// configured env first, then from the daemon's environment.
const JdtlsMaxHeapEnv = "GORTEX_LSP_JDTLS_XMX"

// jdtlsDefaultMaxHeap caps jdtls when the operator configured nothing. Without
// a cap the JVM takes a quarter of physical RAM as its heap ceiling, and G1
// lets the heap grow to it before collecting: one mid-size Gradle repo was
// measured at a 16.4 GB committed heap that a forced GC shrank to 98 MB used.
const jdtlsDefaultMaxHeap = "2G"

// jdtlsLauncherMinHeap is the -Xms the jdtls launcher always passes. A smaller
// -Xmx makes the JVM refuse to start, so such a value is rejected.
const jdtlsLauncherMinHeap = 1 << 30

// jvmOptionsEnvVars are the variables the JVM reads extra options from.
var jvmOptionsEnvVars = []string{"JDK_JAVA_OPTIONS", "JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS"}

// jdtlsHeapDecision records what jdtlsHeapArgs did and why, for the spawn log.
type jdtlsHeapDecision struct {
	// MaxHeap is the -Xmx value added to the launch, or "" when none was.
	MaxHeap string
	// Source names where the outcome came from: "default", the env var, the
	// configured args, or the JVM options variable that already sizes the heap.
	Source string
	// Rejected holds an invalid JdtlsMaxHeapEnv value that was ignored in
	// favour of the default.
	Rejected string
}

// jdtlsHeapArgs prepends `--jvm-arg=-Xmx<size>` to a jdtls launch argv, so the
// launcher hands the JVM a bounded heap. env is the server's configured extra
// environment, which the spawned process sees on top of the daemon's.
//
// Nothing is added when the configured args already size the heap, or when a
// JVM options variable does and the operator did not set JdtlsMaxHeapEnv: a
// command-line -Xmx overrides JDK_JAVA_OPTIONS / JAVA_TOOL_OPTIONS and would
// silently undo an operator's own cap (_JAVA_OPTIONS wins regardless, so
// adding a flag there would only make the log misleading).
func jdtlsHeapArgs(baseArgs, env []string) ([]string, jdtlsHeapDecision) {
	for _, a := range baseArgs {
		if setsJVMHeap(a) {
			return baseArgs, jdtlsHeapDecision{Source: "args"}
		}
	}
	size, decision := jdtlsMaxHeap(env)
	if size == "" {
		return baseArgs, decision
	}
	out := make([]string, 0, len(baseArgs)+1)
	out = append(out, "--jvm-arg=-Xmx"+size)
	return append(out, baseArgs...), decision
}

// jdtlsMaxHeap resolves the heap cap from the effective environment.
func jdtlsMaxHeap(env []string) (string, jdtlsHeapDecision) {
	raw := strings.TrimSpace(effectiveEnv(env, JdtlsMaxHeapEnv))
	switch strings.ToLower(raw) {
	case "":
		// Respect a heap size the operator already set for the JVM.
		for _, name := range jvmOptionsEnvVars {
			if setsJVMHeap(effectiveEnv(env, name)) {
				return "", jdtlsHeapDecision{Source: name}
			}
		}
		return jdtlsDefaultMaxHeap, jdtlsHeapDecision{MaxHeap: jdtlsDefaultMaxHeap, Source: "default"}
	case "off", "0", "none":
		return "", jdtlsHeapDecision{Source: JdtlsMaxHeapEnv}
	}
	if n, ok := parseJVMSize(raw); ok && n >= jdtlsLauncherMinHeap {
		return raw, jdtlsHeapDecision{MaxHeap: raw, Source: JdtlsMaxHeapEnv}
	}
	return jdtlsDefaultMaxHeap, jdtlsHeapDecision{MaxHeap: jdtlsDefaultMaxHeap, Source: "default", Rejected: raw}
}

// setsJVMHeap reports whether a JVM option string sizes the maximum heap:
// -Xmx, its long form -XX:MaxHeapSize, or the RAM-derived ceilings that a
// command-line -Xmx would override. -XX:SoftMaxHeapSize is only a target and
// does not count.
func setsJVMHeap(opts string) bool {
	for _, flag := range []string{"-Xmx", "-XX:MaxHeapSize=", "-XX:MaxRAM=", "-XX:MaxRAMPercentage=", "-XX:MaxRAMFraction="} {
		if strings.Contains(opts, flag) {
			return true
		}
	}
	return false
}

// effectiveEnv returns the value name has in the spawned process: the last
// matching entry of the configured env (os/exec keeps the last duplicate),
// else the daemon's own value.
func effectiveEnv(env []string, name string) string {
	prefix := name + "="
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return env[i][len(prefix):]
		}
	}
	return os.Getenv(name)
}

// parseJVMSize parses a JVM memory size ("2G", "512m", "1073741824") to bytes.
func parseJVMSize(s string) (int64, bool) {
	if s == "" || s[0] < '0' || s[0] > '9' {
		return 0, false
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k', 'K':
		mult = 1 << 10
	case 'm', 'M':
		mult = 1 << 20
	case 'g', 'G':
		mult = 1 << 30
	case 't', 'T':
		mult = 1 << 40
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)/mult {
		return 0, false
	}
	return n * mult, true
}
