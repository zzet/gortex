package lsp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// clearJVMHeapEnv isolates a test from the host's JVM options.
func clearJVMHeapEnv(t *testing.T) {
	t.Helper()
	for _, name := range append([]string{JdtlsMaxHeapEnv}, jvmOptionsEnvVars...) {
		t.Setenv(name, "")
	}
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
			name:      "env off passes no cap",
			daemonEnv: map[string]string{JdtlsMaxHeapEnv: "off"},
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
			name:      "the server env is read before the daemon env",
			daemonEnv: map[string]string{JdtlsMaxHeapEnv: "3G"},
			serverEnv: []string{JdtlsMaxHeapEnv + "=4G"},
			want:      []string{"--jvm-arg=-Xmx4G"},
			source:    JdtlsMaxHeapEnv,
		},
		{
			name:      "a JVM options variable in the server env is respected",
			serverEnv: []string{"JAVA_TOOL_OPTIONS=-Xmx6G"},
			source:    "JAVA_TOOL_OPTIONS",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearJVMHeapEnv(t)
			for k, v := range tt.daemonEnv {
				t.Setenv(k, v)
			}
			args, d := jdtlsHeapArgs(tt.args, tt.serverEnv)
			assert.Equal(t, tt.want, args)
			assert.Equal(t, tt.source, d.Source)
			assert.Equal(t, tt.rejected, d.Rejected)
		})
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
