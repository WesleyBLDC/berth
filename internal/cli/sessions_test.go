package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/client"
)

type fakeSessions struct {
	berthv1connect.UnimplementedSessionsServiceHandler
	list    []*berthv1.Session
	listReq *berthv1.ListSessionsRequest
	stopped []string
	stopErr error
}

func (f *fakeSessions) ListSessions(_ context.Context, req *berthv1.ListSessionsRequest) (*berthv1.ListSessionsResponse, error) {
	f.listReq = req
	return &berthv1.ListSessionsResponse{Sessions: f.list}, nil
}

func (f *fakeSessions) InspectSession(context.Context, *berthv1.InspectSessionRequest) (*berthv1.InspectSessionResponse, error) {
	at := timestamppb.New(time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	return &berthv1.InspectSessionResponse{
		Session: f.list[0],
		Processes: []*berthv1.SessionProcess{
			{Pid: 100, Executable: "/x/chrome-headless-shell", Role: berthv1.ProcessRole_PROCESS_ROLE_BROWSER},
			{Pid: 101, Executable: "/x/chrome-headless-shell", Role: berthv1.ProcessRole_PROCESS_ROLE_HELPER},
		},
		OwnerChain: []*berthv1.SessionProcess{{Pid: 80, Executable: "/opt/homebrew/bin/node", Role: berthv1.ProcessRole_PROCESS_ROLE_ANCESTOR}},
		History:    []*berthv1.Transition{{Version: 1, Event: "Busy", FromState: "Discovered", ToState: "Active", At: at}},
	}, nil
}

func (f *fakeSessions) StopSession(_ context.Context, req *berthv1.StopSessionRequest) (*berthv1.StopSessionResponse, error) {
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	f.stopped = append(f.stopped, req.GetId())
	return &berthv1.StopSessionResponse{State: berthv1.SessionState_SESSION_STATE_STOPPED}, nil
}

func invokeSessions(t *testing.T, fake *fakeSessions, args ...string) result {
	t.Helper()
	server := connect.NewServer()
	berthv1connect.RegisterSessionsServiceHandler(server, fake)
	cl := &client.Client{Sessions: berthv1connect.NewSessionsServiceClient(connect.NewClient(connectinprocess.New(server)))}
	var out, errOut bytes.Buffer
	code := Run(t.Context(), args, Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut,
		Connect: func(context.Context) (*client.Client, error) { return cl, nil },
	})
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func sampleSessions() []*berthv1.Session {
	at := timestamppb.New(time.Date(2026, 11, 17, 11, 59, 0, 0, time.UTC))
	return []*berthv1.Session{
		{
			Id: "100-1794945540000000", State: berthv1.SessionState_SESSION_STATE_ACTIVE, Kind: berthv1.BrowserKind_BROWSER_KIND_HEADLESS_SHELL,
			Driver: berthv1.Driver_DRIVER_PLAYWRIGHT, Headless: true, Cdp: berthv1.CdpMode_CDP_MODE_PORT, UserDataDir: "/tmp/pw-profile",
			Owner:        &berthv1.SessionOwner{Kind: berthv1.OwnerKind_OWNER_KIND_AGENT, Name: "claude", Process: &berthv1.SessionProcess{Pid: 20}},
			Browser:      &berthv1.SessionProcess{Pid: 100, StartedAt: at, Executable: "/x/chrome-headless-shell"},
			ProcessCount: 4, StateSince: at,
		},
		{
			Id: "200-1794945540000000", State: berthv1.SessionState_SESSION_STATE_IDLE, Kind: berthv1.BrowserKind_BROWSER_KIND_CHROME,
			Driver: berthv1.Driver_DRIVER_NONE, Cdp: berthv1.CdpMode_CDP_MODE_NONE,
			Owner: &berthv1.SessionOwner{Kind: berthv1.OwnerKind_OWNER_KIND_PERSON}, ProcessCount: 31,
		},
		{
			Id: "300-1794945540000000", State: berthv1.SessionState_SESSION_STATE_LEFTOVER, Kind: berthv1.BrowserKind_BROWSER_KIND_CHROME_FOR_TESTING,
			Driver: berthv1.Driver_DRIVER_PUPPETEER, Cdp: berthv1.CdpMode_CDP_MODE_PIPE,
			Owner: &berthv1.SessionOwner{Kind: berthv1.OwnerKind_OWNER_KIND_NONE}, ProcessCount: 5,
		},
		{
			Id: "400-1794945540000000", State: berthv1.SessionState_SESSION_STATE_IDLE, Kind: berthv1.BrowserKind_BROWSER_KIND_HEADLESS_SHELL,
			Headless: true, OtherUser: true, Uid: 502, Owner: &berthv1.SessionOwner{Kind: berthv1.OwnerKind_OWNER_KIND_PROCESS}, ProcessCount: 3,
		},
	}
}

func TestPsGolden(t *testing.T) {
	fake := &fakeSessions{list: sampleSessions()}
	r := invokeSessions(t, fake, "ps")
	want := `ID                    STATE     BROWSER             DRIVER      HEADLESS  CDP        OWNER                            PROCS
100-1794945540000000  active    headless shell      playwright  yes       port auto  agent claude (pid 20)            4
200-1794945540000000  idle      Chrome              none        no        none       you                              31
300-1794945540000000  leftover  Chrome for Testing  puppeteer   no        pipe       none, its owner exited           5
400-1794945540000000  idle      headless shell      ?           yes       ?          unmanaged, other user (uid 502)  3
`
	if r.code != ExitOK || r.stdout != want {
		t.Fatalf("code %d, stderr %q, stdout:\n%s\nwant:\n%s", r.code, r.stderr, r.stdout, want)
	}
	if fake.listReq.GetIncludeStopped() {
		t.Fatal("ps asked for stopped sessions")
	}
	if invokeSessions(t, fake, "ps", "--all"); !fake.listReq.GetIncludeStopped() {
		t.Fatal("ps --all did not ask for stopped sessions")
	}
}

func TestInspectGolden(t *testing.T) {
	r := invokeSessions(t, &fakeSessions{list: sampleSessions()}, "inspect", "100-1794945540000000")
	want := `session 100-1794945540000000: active since 2026-11-17 11:59:00

browser     headless shell, pid 100, started 2026-11-17 11:59:00
executable  /x/chrome-headless-shell
driver      playwright
headless    yes
cdp         port auto
profile     /tmp/pw-profile
arguments   read
owner       agent claude (pid 20)

PID  ROLE      EXECUTABLE
100  browser   /x/chrome-headless-shell
101  helper    /x/chrome-headless-shell
80   ancestor  /opt/homebrew/bin/node

V  WHEN                 EVENT  FROM        TO      DETAIL
1  2026-11-17 12:00:00  Busy   Discovered  Active
`
	if r.code != ExitOK || r.stdout != want {
		t.Fatalf("code %d, stderr %q, stdout:\n%s\nwant:\n%s", r.code, r.stderr, r.stdout, want)
	}
}

func TestStopAndLeftovers(t *testing.T) {
	fake := &fakeSessions{list: sampleSessions()[2:]}
	if r := invokeSessions(t, fake, "stop", "300-1794945540000000"); r.code != ExitOK || r.stdout != "stopped 300-1794945540000000\n" {
		t.Fatalf("stop: %+v", r)
	}
	fake.stopped = nil
	r := invokeSessions(t, fake, "leftovers", "--stop")
	if r.code != ExitOK || !strings.Contains(r.stdout, "stopped 300-1794945540000000\n") || !strings.Contains(r.stdout, "skipped 400-1794945540000000") {
		t.Fatalf("leftovers --stop: %+v", r)
	}
	if len(fake.stopped) != 1 || fake.listReq.GetStates()[0] != berthv1.SessionState_SESSION_STATE_LEFTOVER {
		t.Fatalf("stopped %v after listing %v", fake.stopped, fake.listReq)
	}
	fake.stopErr = reasonError(t, berthv1.ErrorReason_ERROR_REASON_SESSION_NOT_STOPPABLE)
	r = invokeSessions(t, fake, "stop", "200-1")
	if r.code != ExitError || !strings.Contains(r.stderr, "needs --force") {
		t.Fatalf("refused stop: %+v", r)
	}
	if r := invokeSessions(t, fake, "stop"); r.code != ExitUsage {
		t.Fatalf("stop without an id: %+v", r)
	}
}

// Text a process chose (its profile folder, its program name) must not drive the terminal.
func TestSessionTextIsEscaped(t *testing.T) {
	list := sampleSessions()[:1]
	list[0].UserDataDir = "/tmp/p\x1b[2J\nFAKE  line"
	list[0].Owner.Name = "cla\x1b]0;pwned\x07ude\r\n"
	list[0].Browser.Executable = "/x/chrome\u202e\x7f\u0085"
	for _, args := range [][]string{{"ps"}, {"inspect", "100-1794945540000000"}} {
		r := invokeSessions(t, &fakeSessions{list: list}, args...)
		if r.code != ExitOK {
			t.Fatalf("%v: %+v", args, r)
		}
		for _, bad := range []string{"\x1b", "\r", "\x07", "\x7f", "\u202e", "\u0085", "\nFAKE"} {
			if strings.Contains(r.stdout, bad) {
				t.Fatalf("%v printed %q raw:\n%s", args, bad, r.stdout)
			}
		}
		if !strings.Contains(r.stdout, `"cla\x1b]0;pwned\aude\r\n"`) {
			t.Fatalf("%v: owner name not shown escaped:\n%s", args, r.stdout)
		}
	}
	r := invokeSessions(t, &fakeSessions{list: list}, "inspect", "100-1794945540000000")
	if !strings.Contains(r.stdout, `"/tmp/p\x1b[2J\nFAKE  line"`) || !strings.Contains(r.stdout, `"/x/chrome\u202e\x7f\u0085"`) {
		t.Fatalf("profile or executable not shown escaped:\n%s", r.stdout)
	}
}

// --json escapes what a terminal would act on that protojson leaves raw: C1 controls, DEL and
// bidi overrides, outside the BMP too, and the JSON still reads back the same.
func TestJSONEscapesControlAndFormatRunes(t *testing.T) {
	odd := "/tmp/a\u0085b\u007fc\u202ed\U000e0001e"
	list := sampleSessions()
	list[0].UserDataDir = odd
	r := invokeSessions(t, &fakeSessions{list: list}, "ps", "--json")
	if r.code != ExitOK || strings.ContainsAny(r.stdout, "\u0085\u007f\u202e\U000e0001") {
		t.Fatalf("code %d, stdout %q", r.code, r.stdout)
	}
	var back berthv1.ListSessionsResponse
	if err := protojson.Unmarshal([]byte(r.stdout), &back); err != nil || back.GetSessions()[0].GetUserDataDir() != odd {
		t.Fatalf("read back %q: %v", back.GetSessions()[0].GetUserDataDir(), err)
	}
}

func TestInspectMarksAReplacedExecutable(t *testing.T) {
	list := sampleSessions()
	list[0].Browser.ExecutableChanged = true
	r := invokeSessions(t, &fakeSessions{list: list}, "inspect", list[0].GetId())
	if r.code != ExitOK || !strings.Contains(r.stdout, "/x/chrome-headless-shell (replaced or deleted since)") {
		t.Fatalf("code %d, stdout:\n%s", r.code, r.stdout)
	}
}
