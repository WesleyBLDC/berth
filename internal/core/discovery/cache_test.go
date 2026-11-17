package discovery

import (
	"strings"
	"testing"
	"time"

	"berth/internal/core/ports"
	"berth/internal/testutil"
)

// What the scanner remembers between scans is clipped like what it reports, so one process with
// a huge profile path or argument list cannot make berthd hold megabytes.
func TestScannerCachesOnlyClippedText(t *testing.T) {
	const shell = "/Users/u/Library/Caches/ms-playwright/chromium_headless_shell-1248/chrome-headless-shell-mac-arm64/chrome-headless-shell"
	huge := strings.Repeat("x", 1<<16)
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	p := func(pid, ppid int, exe string, args ...string) testutil.FakeProcess {
		return testutil.FakeProcess{ProcessInfo: ports.ProcessInfo{
			ProcessRef: ports.ProcessRef{PID: pid, Start: at}, PPID: ppid, UID: 501, Executable: exe,
		}, Args: args}
	}
	src := &testutil.Processes{}
	src.Set(
		p(1, 0, "/sbin/launchd"),
		p(80, 1, "/opt/homebrew/bin/node", append([]string{"node" + huge, "--flag", "/repo/" + huge}, make([]string, 1000)...)...),
		p(100, 80, shell, shell, "--headless", "--user-data-dir=/tmp/"+huge),
	)
	s := NewScanner(src, 501)
	if _, err := s.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	for k, c := range s.sigs {
		if len(c.sig.UserDataDir) > MaxText {
			t.Errorf("pid %d: cached a %d byte profile path", k.pid, len(c.sig.UserDataDir))
		}
	}
	for k, a := range s.args {
		size := 0
		for _, arg := range a {
			size += len(arg)
		}
		if len(a) > 2 || size > 2*MaxText {
			t.Errorf("pid %d: cached %d arguments, %d bytes", k.pid, len(a), size)
		}
	}
}
