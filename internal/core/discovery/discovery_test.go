package discovery_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"berth/internal/core/browsersig"
	"berth/internal/core/discovery"
	"berth/internal/core/ownerchain"
	"berth/internal/core/ports"
	"berth/internal/core/session"
	"berth/internal/testutil"
)

const (
	me    = 501
	other = 502
	shell = "/Users/u/Library/Caches/ms-playwright/chromium_headless_shell-1248/chrome-headless-shell-mac-arm64/chrome-headless-shell"
	chrom = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	help  = "/Applications/Google Chrome.app/Contents/Frameworks/Google Chrome Framework.framework/Versions/1/Helpers/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper"
)

var t0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func proc(pid, ppid int, uid uint32, exe string, args ...string) testutil.FakeProcess {
	return testutil.FakeProcess{
		ProcessInfo: ports.ProcessInfo{ProcessRef: ports.ProcessRef{PID: pid, Start: t0.Add(time.Duration(pid) * time.Microsecond)}, PPID: ppid, UID: uid, Executable: exe},
		Args:        args,
	}
}

func table() []testutil.FakeProcess {
	launchd := proc(1, 0, 0, "/sbin/launchd")
	launchd.Opaque = true
	pwMain := proc(100, 80, me, shell, shell, "--headless", "--remote-debugging-pipe", "--user-data-dir=/tmp/playwright_chromiumdev_profile-x", "--proxy-server=http://user:canary@proxy")
	pwMain.CPU = time.Second
	renderer := proc(101, 100, me, shell, shell, "--type=renderer")
	renderer.CPU = 2 * time.Second
	theirs := proc(300, 1, other, shell)
	theirs.Opaque = true
	return []testutil.FakeProcess{
		launchd,
		proc(30, 1, me, "/Applications/Ghostty.app/Contents/MacOS/ghostty"),
		proc(40, 30, me, "/bin/zsh", "-zsh"),
		proc(80, 40, me, "/opt/homebrew/bin/node", "node", "/repo/node_modules/.bin/playwright", "test"),
		pwMain, renderer,
		proc(102, 101, me, shell, shell, "--type=gpu-process"),
		proc(200, 1, me, chrom, chrom),
		proc(201, 200, me, help, help, "--type=renderer"),
		theirs,
		proc(400, 1, me, "/Applications/Slack.app/Contents/Frameworks/Slack Helper.app/Contents/MacOS/Slack Helper"),
	}
}

func scan(t *testing.T, rows []testutil.FakeProcess) map[int]discovery.Observed {
	t.Helper()
	src := &testutil.Processes{}
	src.Set(rows...)
	got, err := discovery.NewScanner(src, me).Scan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	byRoot := map[int]discovery.Observed{}
	for _, o := range got {
		byRoot[o.Root.PID] = o
	}
	return byRoot
}

func pids(ms []discovery.Member) []int {
	var out []int
	for _, m := range ms {
		out = append(out, m.PID)
	}
	return out
}

func TestScanGroupsSessions(t *testing.T) {
	got := scan(t, table())
	if len(got) != 3 {
		t.Fatalf("sessions %v, want roots 100, 200 and 300", got)
	}
	pw := got[100]
	if pw.Signature.Driver != browsersig.Playwright || !pw.Signature.Headless || pw.Signature.CDP != browsersig.CDPPipe || pw.OtherUser {
		t.Errorf("Playwright session signature %+v", pw.Signature)
	}
	if pw.Owner.Kind != ownerchain.Terminal || pw.Owner.Name != "Ghostty" || pw.Owner.Process.PID != 30 {
		t.Errorf("Playwright owner %+v", pw.Owner)
	}
	if !slices.Equal(pids(pw.Members), []int{100, 101, 102}) || pw.CPU != 3*time.Second {
		t.Errorf("Playwright members %v cpu %v", pids(pw.Members), pw.CPU)
	}
	if len(pw.Chain) != 4 || pw.Chain[0].PID != 80 || pw.Chain[3].PID != 1 {
		t.Errorf("Playwright chain %+v", pw.Chain)
	}
	if pw.ID != "100-1794916800000100" {
		t.Errorf("id %s", pw.ID)
	}
	person := got[200]
	if person.Owner.Kind != ownerchain.Person || !slices.Equal(pids(person.Members), []int{200, 201}) {
		t.Errorf("person's Chrome %+v", person)
	}
	theirs := got[300]
	if !theirs.OtherUser || theirs.Signature.ArgsKnown || theirs.CPU != 0 {
		t.Errorf("other user's browser %+v", theirs)
	}
}

func TestReusedPidIsANewSession(t *testing.T) {
	rows := table()
	before := scan(t, rows)[200].ID
	rows[7].Start = rows[7].Start.Add(time.Hour)
	rows[8].PPID = 200
	if after := scan(t, rows)[200].ID; after == before {
		t.Fatalf("a pid started later kept the id %s", after)
	}
}

func step(t *testing.T, tr discovery.Tracked, o *discovery.Observed, at time.Duration, want ...session.EventKind) discovery.Tracked {
	t.Helper()
	return stepWith(t, func(ports.ProcessRef) bool { return true }, tr, o, at, want...)
}

func stepWith(t *testing.T, running func(ports.ProcessRef) bool, tr discovery.Tracked, o *discovery.Observed, at time.Duration, want ...session.EventKind) discovery.Tracked {
	t.Helper()
	next, events := discovery.Events(tr, o, running, t0.Add(at))
	var kinds []session.EventKind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if !slices.Equal(kinds, want) {
		t.Fatalf("at %v in %s: events %v, want %v", at, tr.State, kinds, want)
	}
	return next
}

func TestBusyThenQuiet(t *testing.T) {
	o := scan(t, table())[100]
	tr := step(t, discovery.Tracked{State: session.Discovered}, &o, 0)
	o.CPU += discovery.BusyCPU - time.Millisecond
	tr = step(t, tr, &o, 2*time.Second)
	o.CPU += discovery.BusyCPU
	tr = step(t, tr, &o, 4*time.Second, session.Busy)
	tr.State = session.Active
	tr = step(t, tr, &o, 4*time.Second+discovery.QuietAfter-time.Second)
	tr = step(t, tr, &o, 4*time.Second+discovery.QuietAfter, session.Quiet)
	tr.State = session.Idle
	step(t, tr, &o, 2*discovery.QuietAfter)
}

// scanner scans rows again and again with one Scanner, as berthd does.
type scanner struct {
	t   *testing.T
	src *testutil.Processes
	s   *discovery.Scanner
}

func newScanner(t *testing.T) *scanner {
	src := &testutil.Processes{}
	return &scanner{t: t, src: src, s: discovery.NewScanner(src, me)}
}

// scan returns the Playwright session.
func (sc *scanner) scan(rows []testutil.FakeProcess) *discovery.Observed {
	sc.t.Helper()
	sc.src.Set(rows...)
	got, err := sc.s.Scan(sc.t.Context())
	if err != nil {
		sc.t.Fatal(err)
	}
	i := slices.IndexFunc(got, func(o discovery.Observed) bool { return o.Root.PID == 100 })
	if i < 0 {
		sc.t.Fatal("no Playwright session")
	}
	return &got[i]
}

func TestOwnerExits(t *testing.T) {
	rows := table()
	sc := newScanner(t)
	o := sc.scan(rows)
	tr := stepWith(t, sc.s.Running, discovery.Tracked{State: session.Active}, o, 0)
	rows[1].Start = rows[1].Start.Add(time.Hour) // the terminal exited and its pid was reused
	o = sc.scan(rows)
	tr = stepWith(t, sc.s.Running, tr, o, 2*time.Second, session.OwnerExited)
	tr.State = session.Leftover
	o.CPU += time.Second
	stepWith(t, sc.s.Running, tr, o, 4*time.Second)
}

// agentTable runs the Playwright browser under a claude agent, through a shell and node.
func agentTable() []testutil.FakeProcess {
	rows := table()
	rows[2].PPID = 35 // zsh now runs under claude
	return append(rows, proc(35, 30, me, "/Users/u/.local/share/claude/versions/2.1.322", "claude"))
}

// An ancestor between the owner and the browser exiting, or the owner's file being replaced by
// an update, leaves a live agent's browser alone: leftovers --stop would kill it.
func TestALiveOwnersBrowserIsNeverALeftover(t *testing.T) {
	rows := agentTable()
	sc := newScanner(t)
	o := sc.scan(rows)
	if o.Owner.Kind != ownerchain.Agent || o.Owner.Process.PID != 35 {
		t.Fatalf("owner %+v", o.Owner)
	}
	tr := stepWith(t, sc.s.Running, discovery.Tracked{State: session.Active}, o, 0)
	rows[len(rows)-1].Executable = "/Users/u/.local/share/claude/versions/2.1.322.old"
	tr = stepWith(t, sc.s.Running, tr, sc.scan(rows), 2*time.Second)
	rows = slices.Delete(rows, 2, 3) // zsh exits, so node is reparented to launchd
	rows[2].PPID = 1
	o = sc.scan(rows)
	if o.Owner.Process.PID == 35 {
		t.Fatalf("owner %+v is still the agent", o.Owner)
	}
	tr = stepWith(t, sc.s.Running, tr, o, 4*time.Second)
	rows = slices.DeleteFunc(rows, func(r testutil.FakeProcess) bool { return r.PID == 35 })
	stepWith(t, sc.s.Running, tr, sc.scan(rows), 6*time.Second, session.OwnerExited)
}

func TestReparentedToLaunchdIsLeftoverAtOnce(t *testing.T) {
	rows := table()
	rows[4].PPID = 1
	o := scan(t, rows)[100]
	if o.Owner.Kind != ownerchain.None {
		t.Fatalf("owner %+v, want None", o.Owner)
	}
	step(t, discovery.Tracked{State: session.Discovered}, &o, 0, session.OwnerExited)
}

// A session with no row gets one at first sight, before anything else that scan saw; one that
// already has rows gets none.
func TestFirstSightOfAnUnsavedSessionIsSeen(t *testing.T) {
	rows := table()
	o := scan(t, rows)[100]
	tr := step(t, discovery.Tracked{State: session.Discovered, Unsaved: true}, &o, 0, session.Seen)
	step(t, tr, &o, 2*time.Second)
	step(t, discovery.Tracked{State: session.Active, Unsaved: false}, &o, 0)
	rows[4].PPID = 1
	o = scan(t, rows)[100]
	step(t, discovery.Tracked{State: session.Discovered, Unsaved: true}, &o, 0, session.Seen, session.OwnerExited)
}

func TestGoneIsExitedOnce(t *testing.T) {
	step(t, discovery.Tracked{State: session.Stopping}, nil, 0, session.Exited)
	step(t, discovery.Tracked{State: session.Stopped}, nil, 0)
}

// Raw arguments never leave the scan: only the parsed signature does.
func TestArgumentsStayInside(t *testing.T) {
	o := scan(t, table())[100]
	if o.Signature.UserDataDir != "/tmp/playwright_chromiumdev_profile-x" {
		t.Fatalf("user data dir %q", o.Signature.UserDataDir)
	}
	for _, field := range []any{o.Root, o.Chain, o.Members, o.Owner, o.Signature} {
		if s := fmt.Sprintf("%+v", field); strings.Contains(s, "canary") {
			t.Fatalf("an argument leaked into %s", s)
		}
	}
}

// A CDP port alone is no evidence of a driver: a person can open their own browser with one, so
// under launchd it stays the person's and never becomes a leftover.
func TestPersonsBrowserWithADebugPortIsNotALeftover(t *testing.T) {
	rows := table()
	rows[7].Args = []string{chrom, "--remote-debugging-port=9222"}
	o := scan(t, rows)[200]
	if o.Owner.Kind != ownerchain.Person || o.Signature.CDP != browsersig.CDPPort {
		t.Fatalf("owner %+v, signature %+v", o.Owner, o.Signature)
	}
	tr := step(t, discovery.Tracked{State: session.Discovered}, &o, 0)
	step(t, tr, &o, 2*time.Second)
	rows[7].Args = []string{chrom, "--remote-debugging-pipe"}
	if o := scan(t, rows)[200]; o.Owner.Kind != ownerchain.None {
		t.Fatalf("a CDP pipe under launchd should be a leftover, owner %+v", o.Owner)
	}
}

// One process must not make berthd keep megabytes: every kept string is clipped.
func TestKeptTextIsClipped(t *testing.T) {
	rows := table()
	huge := "/tmp/playwright_chromiumdev_profile-" + strings.Repeat("é", 1<<19)
	rows[4].Args = []string{shell, "--headless", "--user-data-dir=" + huge}
	o := scan(t, rows)[100]
	got := o.Signature.UserDataDir
	if len(got) > discovery.MaxText || !strings.HasSuffix(got, discovery.Truncated) || !utf8.ValidString(got) {
		t.Fatalf("user data dir kept %d bytes, valid %v, ends %q", len(got), utf8.ValidString(got), got[max(0, len(got)-20):])
	}
	if o.Signature.Driver != browsersig.Playwright {
		t.Fatalf("classified on the clipped text: %v", o.Signature.Driver)
	}
	if discovery.Clip("/short") != "/short" {
		t.Fatal("a short string changed")
	}
}

type countingSource struct {
	*testutil.Processes
	reads int
}

func (c *countingSource) Args(ctx context.Context, p ports.ProcessRef) ([]string, error) {
	c.reads++
	return c.Processes.Args(ctx, p)
}

// Arguments are read once per process instance, not on every scan, and again when the pid names
// a later process or the process exec'd another executable.
func TestScannerReadsArgumentsOncePerProcess(t *testing.T) {
	rows := table()
	src := &countingSource{Processes: &testutil.Processes{}}
	src.Set(rows...)
	s := discovery.NewScanner(src, me)
	scanOnce := func() map[int]discovery.Observed {
		t.Helper()
		got, err := s.Scan(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		byRoot := map[int]discovery.Observed{}
		for _, o := range got {
			byRoot[o.Root.PID] = o
		}
		return byRoot
	}
	scanOnce()
	src.reads = 0
	if got := scanOnce(); src.reads != 0 || got[100].Signature.Driver != browsersig.Playwright || got[100].Owner.Kind != ownerchain.Terminal {
		t.Fatalf("second scan read arguments %d times, Playwright session %+v", src.reads, got[100])
	}
	rows[3].Start = rows[3].Start.Add(time.Hour)
	rows[3].Args = []string{"node", "/repo/node_modules/@anthropic-ai/claude-code/cli.js"}
	src.Set(rows...)
	if got := scanOnce(); src.reads != 1 || got[100].Owner.Kind != ownerchain.Agent {
		t.Fatalf("a reused pid read arguments %d times, owner %+v", src.reads, got[100].Owner)
	}
	src.reads = 0
	rows[7].Executable, rows[7].Args = shell, []string{shell, "--headless"}
	src.Set(rows...)
	if got := scanOnce(); src.reads != 1 || !got[200].Signature.Headless {
		t.Fatalf("an exec read arguments %d times, signature %+v", src.reads, got[200].Signature)
	}
}

// One invalid UTF-8 byte in a path or a profile folder must not reach berthd: protobuf refuses to
// marshal it, which would break every listing. It shows as U+FFFD instead.
func TestKeptTextIsValidUTF8(t *testing.T) {
	rows := table()
	odd := "/Users/\xff/Library/Caches/ms-playwright/chromium_headless_shell-1248/chrome-headless-shell-mac-arm64/chrome-headless-shell"
	rows[4].Executable = odd
	rows[4].Args = []string{odd, "--headless", "--user-data-dir=/tmp/\xff"}
	o := scan(t, rows)[100]
	if o.Signature.UserDataDir != "/tmp/�" || !utf8.ValidString(o.Root.Executable) || !strings.Contains(o.Root.Executable, "�") {
		t.Fatalf("profile %q, executable %q", o.Signature.UserDataDir, o.Root.Executable)
	}
	if !utf8.ValidString(o.Members[0].Executable) {
		t.Fatalf("member executable %q", o.Members[0].Executable)
	}
}

// The kernel gives no path for a running process whose file was replaced or deleted, as a
// browser or agent update does. That process keeps its last path, marked changed, so its
// browser is neither taken for exited nor given another owner.
func TestReplacedExecutableKeepsItsLastPath(t *testing.T) {
	rows := table()
	src := &testutil.Processes{}
	src.Set(rows...)
	s := discovery.NewScanner(src, me)
	if _, err := s.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows[4].Executable, rows[3].Executable = "", ""
	fresh := proc(500, 1, me, "")
	src.Set(append(rows, fresh)...)
	got, err := s.Scan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(got, func(o discovery.Observed) bool { return o.Root.PID == 100 })
	if i < 0 {
		t.Fatal("the browser whose file was replaced is gone from the scan")
	}
	o := got[i]
	if o.Root.Executable != shell || !o.Root.ExecutableChanged || o.Members[0].Executable != shell {
		t.Fatalf("root %+v", o.Root)
	}
	if o.Chain[0].Executable != "/opt/homebrew/bin/node" || !o.Chain[0].ExecutableChanged || o.Chain[1].ExecutableChanged {
		t.Fatalf("chain %+v", o.Chain)
	}
}
