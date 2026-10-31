package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The render tests in daemon_service_test.go check fragments. These pin
// whole unit files, byte for byte, so a change to how they are rendered
// cannot move a newline, an indent or an escape unnoticed.

// launchdDefaultPATH is the plist PATH when the installing shell's PATH
// contributes nothing: the launchd defaults, joined with the host's list
// separator.
func launchdDefaultPATH() string {
	return strings.Join([]string{"/usr/local/bin", "/opt/homebrew/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator))
}

func TestRenderLaunchdPlist_Golden(t *testing.T) {
	const head = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.zzet.gortex</string>
    <key>ProgramArguments</key>
    <array>
        <string>/opt/a&amp;b/gortex</string>
        <string>daemon</string>
        <string>start</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>/var/o&#39;neil/&lt;logs&gt;/daemon.stderr.log</string>
    <key>StandardErrorPath</key>
    <string>/var/o&#39;neil/&lt;logs&gt;/daemon.stderr.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>`
	const tail = `
    </dict>
</dict>
</plist>
`
	for _, tc := range []struct {
		name string
		env  []serviceEnvVar
		vars string
	}{
		{name: "no env"},
		{
			name: "env",
			env: []serviceEnvVar{
				{Key: "XDG_CONFIG_HOME", Value: `/srv/a&b/"config"`},
				{Key: "GORTEX_DAEMON_LOGFILE", Value: "/srv/a&b/daemon.log"},
			},
			vars: `
        <key>XDG_CONFIG_HOME</key>
        <string>/srv/a&amp;b/&#34;config&#34;</string>
        <key>GORTEX_DAEMON_LOGFILE</key>
        <string>/srv/a&amp;b/daemon.log</string>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", "")
			out, err := renderLaunchdPlist("com.zzet.gortex", "/opt/a&b/gortex", "/var/o'neil/<logs>/daemon.stderr.log", tc.env)
			require.NoError(t, err)
			require.Equal(t, head+launchdDefaultPATH()+"</string>"+tc.vars+tail, out)
			assertWellFormedXML(t, out)
		})
	}
}

func TestRenderSystemdUnit_Golden(t *testing.T) {
	const head = `[Unit]
Description=Gortex code intelligence daemon
Documentation=https://github.com/zzet/gortex
After=network.target

[Service]
Type=simple
ExecStart=/home/u/.local/bin/gortex daemon start`
	const tail = `
Restart=on-failure
RestartSec=2
StandardOutput=append:/home/u/100%% logs/daemon.stderr.log
StandardError=append:/home/u/100%% logs/daemon.stderr.log

[Install]
WantedBy=default.target
`
	for _, tc := range []struct {
		name string
		env  []serviceEnvVar
		vars string
	}{
		{name: "no env"},
		{
			name: "env",
			env: []serviceEnvVar{
				{Key: "XDG_CACHE_HOME", Value: "/home/u/.cache"},
				{Key: "XDG_DATA_HOME", Value: "/home/u/has space"},
				{Key: "XDG_CONFIG_HOME", Value: `/home/u/"q"\x`},
				{Key: "GORTEX_DAEMON_LOGFILE", Value: "/home/u/100% logs/daemon.log"},
			},
			vars: `
Environment=XDG_CACHE_HOME=/home/u/.cache
Environment="XDG_DATA_HOME=/home/u/has space"
Environment="XDG_CONFIG_HOME=/home/u/\"q\"\\x"
Environment="GORTEX_DAEMON_LOGFILE=/home/u/100%% logs/daemon.log"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", "")
			out, err := renderSystemdUnit("/home/u/.local/bin/gortex", "/home/u/100% logs/daemon.stderr.log", tc.env)
			require.NoError(t, err)
			require.Equal(t, head+tc.vars+tail, out)
		})
	}
}
