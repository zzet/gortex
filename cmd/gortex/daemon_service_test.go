package main

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertWellFormedXML fails if s is not parseable XML — a stronger
// guarantee than substring checks that the env injection didn't corrupt
// the plist into something launchctl can't load.
func assertWellFormedXML(t *testing.T, s string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(s))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		require.NoError(t, err, "plist must be well-formed XML")
	}
}

// TestRenderLaunchdPlist_NoXDG proves the launchd plist is valid and
// carries the substitutions it must — binary path, log path, label —
// and that with no XDG override captured it has no extra env entries
// (the pre-fix baseline behaviour, unchanged for the common case).
func TestRenderLaunchdPlist_NoXDG(t *testing.T) {
	out, err := renderLaunchdPlist(
		"com.zzet.gortex",
		"/usr/local/bin/gortex",
		"/Users/testuser/.gortex/cache/daemon.log",
		nil,
	)
	require.NoError(t, err)

	assert.Contains(t, out, "<key>Label</key>\n    <string>com.zzet.gortex</string>")
	assert.Contains(t, out, "<string>/usr/local/bin/gortex</string>")
	assert.Contains(t, out, "<string>daemon</string>")
	assert.Contains(t, out, "<string>start</string>")
	assert.Contains(t, out, "<key>RunAtLoad</key>")
	assert.Contains(t, out, "<key>KeepAlive</key>")
	assert.Contains(t, out, "/Users/testuser/.gortex/cache/daemon.log")
	// Homebrew paths must be on PATH so launchd can find a Homebrew-
	// installed gortex binary even when the LaunchAgent env is minimal.
	assert.Contains(t, out, "/opt/homebrew/bin")
	assert.NotContains(t, out, "XDG_", "no env entries when none captured")
	assertWellFormedXML(t, out)
}

// TestRenderLaunchdPlist_PropagatesXDG is the regression test for the
// service-ignores-XDG bug: when an XDG override is in effect at install
// time it must be baked into the plist so the supervised daemon resolves
// the same paths as the installing shell instead of falling back to
// ~/.gortex.
func TestRenderLaunchdPlist_PropagatesXDG(t *testing.T) {
	env := []serviceEnvVar{
		{Key: "XDG_CONFIG_HOME", Value: "/Users/testuser/.config"},
		{Key: "XDG_DATA_HOME", Value: "/Users/testuser/.local/share"},
	}
	out, err := renderLaunchdPlist(
		"com.zzet.gortex",
		"/usr/local/bin/gortex",
		"/Users/testuser/.config/gortex/daemon.log",
		env,
	)
	require.NoError(t, err)

	assert.Contains(t, out, "<key>XDG_CONFIG_HOME</key>\n        <string>/Users/testuser/.config</string>")
	assert.Contains(t, out, "<key>XDG_DATA_HOME</key>\n        <string>/Users/testuser/.local/share</string>")
	// PATH must still be present alongside the captured XDG vars.
	assert.Contains(t, out, "<key>PATH</key>")
	assertWellFormedXML(t, out)
}

// TestRenderLaunchdPlist_EscapesXML guards against a home path with an
// XML metacharacter producing a malformed, unloadable plist.
func TestRenderLaunchdPlist_EscapesXML(t *testing.T) {
	env := []serviceEnvVar{{Key: "XDG_CONFIG_HOME", Value: "/Users/a&b/.config"}}
	out, err := renderLaunchdPlist("com.zzet.gortex", "/opt/a&b/gortex", "/log&path", env)
	require.NoError(t, err)

	assert.Contains(t, out, "/Users/a&amp;b/.config")
	assert.Contains(t, out, "/opt/a&amp;b/gortex")
	assert.NotContains(t, out, "a&b", "raw ampersand must be escaped")
	assertWellFormedXML(t, out)
}

// TestRenderSystemdUnit_NoXDG confirms the Type=simple + Restart=on-failure
// contract and that no Environment= line is emitted when nothing was
// captured.
func TestRenderSystemdUnit_NoXDG(t *testing.T) {
	t.Setenv("PATH", "")
	out, err := renderSystemdUnit(
		"/home/u/.local/bin/gortex",
		"/home/u/.gortex/cache/daemon.log",
		nil,
	)
	require.NoError(t, err)

	assert.Contains(t, out, "ExecStart=/home/u/.local/bin/gortex daemon start")
	assert.Contains(t, out, "Type=simple")
	assert.Contains(t, out, "Restart=on-failure")
	assert.Contains(t, out, "StandardOutput=append:/home/u/.gortex/cache/daemon.log")
	assert.Contains(t, out, "WantedBy=default.target")
	assert.NotContains(t, out, "Environment=", "no env line when none captured")
}

// TestRenderSystemdUnit_PropagatesXDG is the Linux analog of the launchd
// regression test, and also covers systemd quoting for a value with a
// space.
func TestRenderSystemdUnit_PropagatesXDG(t *testing.T) {
	env := []serviceEnvVar{
		{Key: "XDG_CACHE_HOME", Value: "/home/u/.cache"},
		{Key: "XDG_DATA_HOME", Value: "/home/u/has space"},
	}
	out, err := renderSystemdUnit(
		"/home/u/.local/bin/gortex",
		"/home/u/.cache/gortex/daemon.log",
		env,
	)
	require.NoError(t, err)

	assert.Contains(t, out, "Environment=XDG_CACHE_HOME=/home/u/.cache")
	// An assignment containing whitespace is double-quoted as a whole,
	// because systemd recognizes a quote only at the start of an item.
	assert.Contains(t, out, `Environment="XDG_DATA_HOME=/home/u/has space"`)

	// Environment lines must sit inside [Service], ahead of [Install].
	svcStart := strings.Index(out, "[Service]")
	installStart := strings.Index(out, "[Install]")
	require.Greater(t, svcStart, -1)
	require.Greater(t, installStart, svcStart)
	svc := out[svcStart:installStart]
	assert.Contains(t, svc, "Environment=XDG_CACHE_HOME=")
}

// TestXDGServiceEnv_OnlyAbsoluteSet pins the capture contract: only XDG
// vars that are set to an absolute path are propagated. Unset and
// relative values are ignored (the latter per the XDG spec, matching
// platform.unifiedDir).
func TestXDGServiceEnv_OnlyAbsoluteSet(t *testing.T) {
	// The absolute fixture has to be native: filepath.IsAbs("/abs/config")
	// is false on Windows, where an absolute path carries a volume.
	absConfig := filepath.Join(t.TempDir(), "config")
	t.Setenv("XDG_CONFIG_HOME", absConfig)
	t.Setenv("XDG_DATA_HOME", filepath.Join("relative", "data")) // ignored — not absolute
	t.Setenv("XDG_CACHE_HOME", "")                               // ignored — empty/unset

	got := map[string]string{}
	for _, e := range xdgServiceEnv() {
		got[e.Key] = e.Value
	}

	assert.Equal(t, absConfig, got["XDG_CONFIG_HOME"])
	_, hasData := got["XDG_DATA_HOME"]
	assert.False(t, hasData, "relative XDG_DATA_HOME must be ignored")
	_, hasCache := got["XDG_CACHE_HOME"]
	assert.False(t, hasCache, "empty XDG_CACHE_HOME must be ignored")
}

func TestServicePath(t *testing.T) {
	sep := string(os.PathListSeparator)
	root := t.TempDir()
	first := filepath.Join(root, "go", "bin")
	second := filepath.Join(root, "dotnet")
	require.NoDirExists(t, first, "PATH entries need not exist at install time")
	defaults := []string{"/usr/local/bin", "/opt/homebrew/bin", "/usr/bin", "/bin"}

	for _, tt := range []struct {
		name     string
		path     string
		defaults []string
		want     string
	}{
		{
			name: "absolute entries only, first occurrence and order kept",
			path: strings.Join([]string{"", ".", second, "relative/bin", first, second, ""}, sep),
			want: second + sep + first,
		},
		{
			name:     "launchd appends missing defaults without changing precedence",
			path:     strings.Join([]string{second, first, second}, sep),
			defaults: []string{first, second, filepath.Join(root, "default")},
			want:     strings.Join([]string{second, first, filepath.Join(root, "default")}, sep),
		},
		{
			name: "systemd has no appended defaults",
			path: first,
			want: first,
		},
		{
			name:     "empty launchd PATH preserves the old defaults",
			defaults: defaults,
			want:     strings.Join(defaults, sep),
		},
		{name: "empty systemd PATH stays empty"},
		{name: "all relative systemd PATH stays empty", path: sep + "." + sep + "relative/bin"},
		{
			name: "entries containing CR or LF are dropped",
			path: strings.Join([]string{first + "\nExecStartPre=/bin/false", second + "\r", first}, sep),
			want: first,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", tt.path)
			assert.Equal(t, tt.want, servicePath(tt.defaults))
		})
	}
}

func TestRenderLaunchdPlist_CapturesPATH(t *testing.T) {
	sep := string(os.PathListSeparator)
	custom := filepath.Join(t.TempDir(), "a&b", "100% tools")
	t.Setenv("PATH", custom)
	t.Setenv("GORTEX_DAEMON_HTTP_TOKEN", "must-not-be-captured")
	out, err := renderLaunchdPlist("com.zzet.gortex", "/bin/gortex", "/log", nil)
	require.NoError(t, err)

	want := strings.ReplaceAll(custom, "&", "&amp;") + sep +
		strings.Join([]string{"/usr/local/bin", "/opt/homebrew/bin", "/usr/bin", "/bin"}, sep)
	assert.Contains(t, out, "<key>PATH</key>\n        <string>"+want+"</string>")
	assert.NotContains(t, out, "a&b")
	assert.NotContains(t, out, "GORTEX_")
	assert.NotContains(t, out, "must-not-be-captured")
	assertWellFormedXML(t, out)
}

func TestRenderLaunchdPlist_EmptyPATH(t *testing.T) {
	t.Setenv("PATH", "")
	out, err := renderLaunchdPlist("com.zzet.gortex", "/bin/gortex", "/log", nil)
	require.NoError(t, err)

	// This is exactly the old PATH on launchd's Unix host; adapt only the
	// path-list separator when these render tests run on Windows.
	want := strings.ReplaceAll("/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin", ":", string(os.PathListSeparator))
	assert.Contains(t, out, "<key>PATH</key>\n        <string>"+want+"</string>")
	assert.Equal(t, 1, strings.Count(out, "<key>PATH</key>"))
	assertWellFormedXML(t, out)
}

func TestRenderSystemdUnit_CapturesPATH(t *testing.T) {
	sep := string(os.PathListSeparator)
	root := t.TempDir()
	first := filepath.Join(root, "a&b", "100% tools")
	second := filepath.Join(root, "bin")
	t.Setenv("PATH", first+sep+second)
	t.Setenv("GORTEX_DAEMON_HTTP_TOKEN", "must-not-be-captured")
	out, err := renderSystemdUnit("/bin/gortex", "/log", nil)
	require.NoError(t, err)

	// systemd quotes whitespace and escapes backslashes within a quoted
	// value (relevant to this native fixture when tests run on Windows).
	want := strings.ReplaceAll(first+sep+second, "\\", "\\\\")
	want = strings.ReplaceAll(want, "%", "%%")
	assert.Contains(t, out, "\nEnvironment=\"PATH="+want+"\"\n")
	assert.Equal(t, 1, strings.Count(out, "PATH="))
	assert.NotContains(t, out, "/opt/homebrew/bin")
	assert.NotContains(t, out, "GORTEX_")
	assert.NotContains(t, out, "must-not-be-captured")
}

func TestRenderSystemdUnit_EmptyPATH(t *testing.T) {
	for _, path := range []string{"", ".", string(os.PathListSeparator) + "relative/bin"} {
		t.Run(path, func(t *testing.T) {
			t.Setenv("PATH", path)
			out, err := renderSystemdUnit("/bin/gortex", "/log", nil)
			require.NoError(t, err)
			assert.NotContains(t, out, "PATH=")
		})
	}
}

func TestSystemdEnvValue_QuotesWhitespace(t *testing.T) {
	assert.Equal(t, "/home/u/.config", systemdEnvValue("/home/u/.config"))
	assert.Equal(t, `"/home/u/my data"`, systemdEnvValue("/home/u/my data"))
}

// TestSystemdEnvValue_QuotesEscapesAndQuotes covers characters systemd
// interprets in unquoted text: a backslash starts a C-style escape (`\t`
// would become a tab), and a quote starts a quoted section.
func TestSystemdEnvValue_QuotesEscapesAndQuotes(t *testing.T) {
	assert.Equal(t, `"PATH=/opt/a\\tools"`, systemdEnvValue(`PATH=/opt/a\tools`))
	assert.Equal(t, `"PATH=/opt/\"q\"/bin"`, systemdEnvValue(`PATH=/opt/"q"/bin`))
	assert.Equal(t, `"PATH=/opt/it's/bin"`, systemdEnvValue(`PATH=/opt/it's/bin`))
}

// TestSystemdEnvValue_EscapesPercent guards the systemd specifier escape:
// a literal % in a path must become %% or systemd expands it (e.g. %d)
// and the daemon resolves a different directory than was captured.
func TestSystemdEnvValue_EscapesPercent(t *testing.T) {
	assert.Equal(t, "/home/u/100%%dir", systemdEnvValue("/home/u/100%dir"))
	// percent + whitespace: escaped and quoted.
	assert.Equal(t, `"/home/u/a %%b c"`, systemdEnvValue("/home/u/a %b c"))
}

func TestLaunchdPlistPath_ResolvesUnderHome(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd paths only meaningful on darwin")
	}
	path, err := launchdPlistPath()
	require.NoError(t, err)
	assert.Equal(t, "com.zzet.gortex.plist", filepath.Base(path))
	assert.Contains(t, path, filepath.Join("Library", "LaunchAgents"))
}

func TestSystemdUnitPath_ResolvesUnderHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd paths only meaningful on linux")
	}
	path, err := systemdUnitPath()
	require.NoError(t, err)
	assert.Equal(t, "com.zzet.gortex.service", filepath.Base(path))
	assert.Contains(t, path, filepath.Join(".config", "systemd", "user"))
}

// TestServiceCommands_RejectUnsupportedOS keeps the guard
// runDaemonInstallService uses from silently succeeding on a platform we
// haven't wired (darwin/linux/windows are now all supported).
func TestServiceCommands_RejectUnsupportedOS(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		t.Skip("this test only runs on unsupported platforms")
	}
	err := runDaemonInstallService(daemonInstallServiceCmd, nil)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "not supported"),
		"install must refuse on unsupported OS: %v", err)
}

// TestWindowsTaskCreateArgs pins the schtasks command that registers the
// Windows logon autostart task: a per-user (LIMITED) ONLOGON task running the
// quoted binary with `daemon start`, force-replacing any existing task.
func TestWindowsTaskCreateArgs(t *testing.T) {
	args := windowsTaskCreateArgs("GortexDaemon", `C:\Users\x\scoop\apps\gortex\current\gortex.exe`)
	require.Equal(t, "/Create", args[0])

	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "/TN GortexDaemon")
	assert.Contains(t, joined, "/SC ONLOGON")
	assert.Contains(t, joined, "/RL LIMITED")
	assert.Contains(t, joined, "/F")
	// The run action quotes the exe (so a Program Files path with spaces is one
	// token) and appends the daemon subcommand.
	assert.Contains(t, joined, `"C:\Users\x\scoop\apps\gortex\current\gortex.exe" daemon start`)
}
