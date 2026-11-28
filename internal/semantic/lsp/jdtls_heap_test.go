package lsp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// clearJVMHeapEnv isolates a test from the host's JVM options and RAM size.
func clearJVMHeapEnv(t *testing.T) {
	t.Helper()
	for _, name := range append([]string{JdtlsMaxHeapEnv}, jvmOptionsEnvVars...) {
		t.Setenv(name, "")
	}
	orig := hostMemoryBytes
	hostMemoryBytes = func() uint64 { return 0 }
	t.Cleanup(func() { hostMemoryBytes = orig })
}

func TestJdtlsHeapArgs(t *testing.T) {
	tests := []struct {
		name      string
		daemonEnv map[string]string
		serverEnv []string
		args      []string
		want      []string
		source    string
		rejected  string
		overrides string
		hostRAM   uint64
	}{
		{
			name:   "nothing configured: default cap, ahead of the launcher args",
			args:   []string{"-data", "/ws"},
			want:   []string{"--jvm-arg=-Xmx2G", "-data", "/ws"},
			source: "default",
		},
		{
			name:      "env sets the size",
			daemonEnv: map[string]string{JdtlsMaxHeapEnv: "4096m"},
			want:      []string{"--jvm-arg=-Xmx4096m"},
			source:    JdtlsMaxHeapEnv,
		},
		{
			name:      "env OFF passes no cap",
			daemonEnv: map[string]string{JdtlsMaxHeapEnv: "OFF"},
			source:    JdtlsMaxHeapEnv,
		},
		{
			name:      "env false passes no cap",
			daemonEnv: map[string]string{JdtlsMaxHeapEnv: "false"},
			source:    JdtlsMaxHeapEnv,
		},
		{
			name:      "malformed size falls back to the default",
			daemonEnv: map[string]string{JdtlsMaxHeapEnv: "2GB"},
			want:      []string{"--jvm-arg=-Xmx2G"},
			source:    "default",
			rejected:  "2GB",
		},
		{
			name:      "size below the launcher's -Xms1G falls back to the default",
			daemonEnv: map[string]string{JdtlsMaxHeapEnv: "512m"},
			want:      []string{"--jvm-arg=-Xmx2G"},
			source:    "default",
			rejected:  "512m",
		},
		{
			name:      "configured args that size the heap win",
			daemonEnv: map[string]string{JdtlsMaxHeapEnv: "3G"},
			args:      []string{"--jvm-arg=-Xmx6G"},
			want:      []string{"--jvm-arg=-Xmx6G"},
			source:    "args",
		},
		{
			name:   "a bare -Xmx in the args goes to jdtls, not the JVM, and does not count",
			args:   []string{"-Xmx6G"},
			want:   []string{"--jvm-arg=-Xmx2G", "-Xmx6G"},
			source: "default",
		},
		{
			name:      "a JVM options variable that sizes the heap is not overridden",
			daemonEnv: map[string]string{"JDK_JAVA_OPTIONS": "-Xss4m -Xmx3g"},
			source:    "JDK_JAVA_OPTIONS",
		},
		{
			name:      "the env knob overrides a JVM options variable",
			daemonEnv: map[string]string{"JDK_JAVA_OPTIONS": "-Xmx3g", JdtlsMaxHeapEnv: "5G"},
			want:      []string{"--jvm-arg=-Xmx5G"},
			source:    JdtlsMaxHeapEnv,
		},
		{
			name:      "the server env is read before the daemon env, last entry winning",
			daemonEnv: map[string]string{JdtlsMaxHeapEnv: "3G"},
			serverEnv: []string{JdtlsMaxHeapEnv + "=5G", JdtlsMaxHeapEnv + "=4G"},
			want:      []string{"--jvm-arg=-Xmx4G"},
			source:    JdtlsMaxHeapEnv,
		},
		{
			name:      "a JVM options variable in the server env is respected",
			serverEnv: []string{"JAVA_TOOL_OPTIONS=-Xmx6G"},
			source:    "JAVA_TOOL_OPTIONS",
		},
		{
			name:      "_JAVA_OPTIONS sizing the heap is reported as overriding the cap",
			daemonEnv: map[string]string{"_JAVA_OPTIONS": "-Xmx1g", JdtlsMaxHeapEnv: "3G"},
			want:      []string{"--jvm-arg=-Xmx3G"},
			source:    JdtlsMaxHeapEnv,
			overrides: "_JAVA_OPTIONS",
		},
		{
			name:      "_JAVA_OPTIONS is reported as overriding a cap from the args",
			serverEnv: []string{"_JAVA_OPTIONS=-XX:MaxHeapSize=1g"},
			args:      []string{"--jvm-arg=-Xmx6G"},
			want:      []string{"--jvm-arg=-Xmx6G"},
			source:    "args",
			overrides: "_JAVA_OPTIONS",
		},
		{
			name:      "a RAM-derived flag in _JAVA_OPTIONS does not override an explicit cap",
			daemonEnv: map[string]string{"_JAVA_OPTIONS": "-XX:MaxRAMPercentage=50", JdtlsMaxHeapEnv: "3G"},
			want:      []string{"--jvm-arg=-Xmx3G"},
			source:    JdtlsMaxHeapEnv,
		},
		{
			name:      "precedence: _JAVA_OPTIONS before JDK_JAVA_OPTIONS",
			daemonEnv: map[string]string{"JDK_JAVA_OPTIONS": "-Xmx6G", "_JAVA_OPTIONS": "-Xmx1g"},
			source:    "_JAVA_OPTIONS",
		},
		{
			name:      "precedence: JDK_JAVA_OPTIONS before JAVA_TOOL_OPTIONS",
			daemonEnv: map[string]string{"JAVA_TOOL_OPTIONS": "-Xmx6G", "JDK_JAVA_OPTIONS": "-Xmx1g"},
			source:    "JDK_JAVA_OPTIONS",
		},
		{
			name:    "small host: the default follows the JVM's own ceiling",
			hostRAM: 4 << 30,
			want:    []string{"--jvm-arg=-Xmx1G"},
			source:  "default",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearJVMHeapEnv(t)
			for k, v := range tt.daemonEnv {
				t.Setenv(k, v)
			}
			if tt.hostRAM != 0 {
				hostMemoryBytes = func() uint64 { return tt.hostRAM }
			}
			args, d := jdtlsHeapArgs(tt.args, tt.serverEnv)
			assert.Equal(t, tt.want, args)
			assert.Equal(t, tt.source, d.Source)
			assert.Equal(t, tt.rejected, d.Rejected)
			assert.Equal(t, tt.overrides, d.OverriddenBy)
		})
	}
}

func TestSetsJVMHeap(t *testing.T) {
	for _, opt := range []string{"-Xmx3g", "-XX:MaxHeapSize=3g", "-XX:MaxRAM=8g", "-XX:MaxRAMPercentage=50", "-XX:MaxRAMFraction=4"} {
		assert.True(t, setsJVMHeap("-Xss4m "+opt), opt)
	}
	assert.False(t, setsJVMHeap("-XX:SoftMaxHeapSize=1g -Xss4m"))
}

func TestJdtlsDefaultMaxHeapFor(t *testing.T) {
	for ram, want := range map[uint64]string{
		0:        "2G", // unknown
		16 << 30: "2G",
		6 << 30:  "1536m",
		4 << 30:  "1G",
		2 << 30:  "1G", // never below the launcher's -Xms1G
	} {
		assert.Equal(t, want, jdtlsDefaultMaxHeapFor(ram), ram)
	}
}

func TestParseJVMSize(t *testing.T) {
	for in, want := range map[string]int64{
		"1073741824": 1 << 30, "1048576k": 1 << 30, "1024M": 1 << 30, "2g": 2 << 30, "1T": 1 << 40,
	} {
		got, ok := parseJVMSize(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"", "2GB", "1.5G", "-2G", "0", "99999999999T"} {
		_, ok := parseJVMSize(in)
		assert.False(t, ok, in)
	}
}
