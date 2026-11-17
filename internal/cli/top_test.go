package cli

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
)

var topNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func topSessions() []*berthv1.Session {
	started := func(ago time.Duration) *berthv1.SessionProcess {
		return &berthv1.SessionProcess{Pid: 1, StartedAt: timestamppb.New(topNow.Add(-ago)), Executable: "/x/chrome-headless-shell"}
	}
	list := sampleSessions()
	list[0].Browser = started(50 * time.Hour)
	list[1].Browser, list[1].UserDataDir = started(3*time.Hour), ""
	list[1].Browser.Executable = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	list[2].Browser = started(90 * time.Second)
	list[3].Browser = started(10 * time.Second)
	for _, s := range list {
		s.StateSince = timestamppb.New(topNow.Add(-time.Minute))
	}
	return list
}

type topFake struct {
	stops    []*berthv1.StopSessionRequest
	inspects []string
}

func newTopTest(t *testing.T, sessions []*berthv1.Session) (*topModel, *topFake) {
	t.Helper()
	fake := &topFake{}
	m := newTop(t.Context(), topDeps{
		inspect: func(ctx context.Context, req *berthv1.InspectSessionRequest) (*berthv1.InspectSessionResponse, error) {
			fake.inspects = append(fake.inspects, req.GetId())
			return (&fakeSessions{list: []*berthv1.Session{sessions[0]}}).InspectSession(ctx, req)
		},
		stop: func(_ context.Context, req *berthv1.StopSessionRequest) (*berthv1.StopSessionResponse, error) {
			fake.stops = append(fake.stops, req)
			return &berthv1.StopSessionResponse{State: berthv1.SessionState_SESSION_STATE_STOPPED}, nil
		},
		now: func() time.Time { return topNow },
	})
	m.Update(tea.WindowSizeMsg{Width: 120})
	m.Update(topUpdate{snapshot(7, sessions...)})
	return m, fake
}

func snapshot(id int64, sessions ...*berthv1.Session) *berthv1.WatchSessionsResponse {
	return &berthv1.WatchSessionsResponse{Update: &berthv1.WatchSessionsResponse_Snapshot{Snapshot: &berthv1.SessionsSnapshot{EventId: id, Sessions: sessions}}}
}

func change(id int64, event string, s *berthv1.Session) *berthv1.WatchSessionsResponse {
	return &berthv1.WatchSessionsResponse{Update: &berthv1.WatchSessionsResponse_Change{Change: &berthv1.SessionChange{EventId: id, Event: event, Session: s}}}
}

// press sends keys one by one and runs any command they return, feeding its message back, as
// Bubble Tea would.
func press(m *topModel, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "backspace":
			msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
		default:
			msg = tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
		}
		_, cmd := m.Update(msg)
		for cmd != nil {
			out := cmd()
			if _, quit := out.(tea.QuitMsg); quit || out == nil {
				break
			}
			_, cmd = m.Update(out)
		}
	}
}

func view(m *topModel) string { return m.View().Content }

func TestTopViewGolden(t *testing.T) {
	m, _ := newTopTest(t, topSessions())
	want := `berth top  4 live, 0 stopped  watching

  ID                    STATE     BROWSER             DRIVER      HEADLESS  OWNER                            AGE
> 100-1794945540000000  active    headless shell      playwright  yes       agent claude (pid 20)            2d
  200-1794945540000000  idle      Chrome              none        no        you                              3h
  300-1794945540000000  leftover  Chrome for Testing  puppeteer   no        none, its owner exited           1m
  400-1794945540000000  idle      headless shell      ?           yes       unmanaged, other user (uid 502)  10s

session 100-1794945540000000: active since 2026-11-17 11:59:00
executable  /x/chrome-headless-shell
cdp         port auto
profile     /tmp/pw-profile
arguments   read
owner       agent claude (pid 20)

keys: up/down move  i inspect  s stop  / filter  a stopped  q quit`
	if got := view(m); got != want {
		t.Fatalf("view:\n%s\nwant:\n%s", got, want)
	}

	press(m, "i")
	want = `session 100-1794945540000000: active since 2026-11-17 11:59:00
executable  /x/chrome-headless-shell
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

keys: up/down move  i inspect  s stop  / filter  a stopped  q quit`
	if got := view(m); !strings.HasSuffix(got, want) {
		t.Fatalf("inspected view:\n%s\nwant it to end with:\n%s", got, want)
	}
	press(m, "down")
	if got := view(m); strings.Contains(got, "PID  ROLE") || !strings.Contains(got, "> 200-1794945540000000") ||
		!strings.Contains(got, "profile     default") {
		t.Fatalf("moving down kept the old inspection or missed the row:\n%s", got)
	}
}

// Text a process chose must not drive the terminal: every such string is quoted with escapes, and
// berth top writes no ESC byte of its own, so none may appear at all.
func TestTopEscapesProcessChosenText(t *testing.T) {
	list := topSessions()[:1]
	list[0].UserDataDir = "/tmp/p\x1b[2J\nFAKE  line"
	list[0].Owner.Name = "cla\x1b]0;pwned\x07ude\r\n"
	list[0].Browser.Executable = "/x/chrome\u202e\x7f\u0085"
	m, _ := newTopTest(t, list)
	m.Update(topDown{errors.New("dial \x1b[31mred\x1b[0m")})
	press(m, "i")
	got := view(m)
	for _, bad := range []string{"\x1b", "\r", "\x07", "\x7f", "\u202e", "\u0085", "\nFAKE"} {
		if strings.Contains(got, bad) {
			t.Fatalf("view holds %q raw:\n%q", bad, got)
		}
	}
	for _, want := range []string{
		`agent "cla\x1b]0;pwned\aude\r\n" (pid 20)`,
		`profile     "/tmp/p\x1b[2J\nFAKE  line"`,
		`executable  "/x/chrome\u202e\x7f\u0085"`,
		`reconnecting: "dial \x1b[31mred\x1b[0m"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("view does not show %s:\n%s", want, got)
		}
	}
	press(m, "/", "\x1b", "[")
	if got := lastLine(m); got != `filter: "\x1b["_  (enter keeps it, esc clears it)` {
		t.Fatalf("typed filter shown as %q", got)
	}
}

func TestTopStopFollowsTheStopRules(t *testing.T) {
	m, fake := newTopTest(t, topSessions())
	press(m, "s")
	if got := lastLine(m); got != "stop 100-1794945540000000 (headless shell, owner agent claude (pid 20))? y stops it, any other key cancels" {
		t.Fatalf("confirmation %q", got)
	}
	press(m, "n")
	if len(fake.stops) != 0 || lastLine(m) != "stop cancelled" {
		t.Fatalf("a cancelled stop sent %v, status %q", fake.stops, lastLine(m))
	}
	press(m, "s", "y")
	if len(fake.stops) != 1 || fake.stops[0].GetForce() || fake.stops[0].GetId() != "100-1794945540000000" || len(fake.stops[0].GetRequestId()) < 8 {
		t.Fatalf("plain stop sent %v", fake.stops)
	}
	if lastLine(m) != "stopped 100-1794945540000000" {
		t.Fatalf("status %q", lastLine(m))
	}

	press(m, "down", "s")
	if got := lastLine(m); got != "stop 200-1794945540000000? it belongs to you, so this is a forced stop. y stops it, any other key cancels" {
		t.Fatalf("forced confirmation %q", got)
	}
	press(m, "y")
	if len(fake.stops) != 2 || !fake.stops[1].GetForce() {
		t.Fatalf("the person's browser was not stopped with force: %v", fake.stops)
	}

	press(m, "down", "down", "s")
	if got := lastLine(m); got != "400-1794945540000000 belongs to another user; berth never stops it" || m.confirm != nil {
		t.Fatalf("another user's browser: %q", got)
	}
	press(m, "y")
	if len(fake.stops) != 2 {
		t.Fatalf("another user's browser got a stop: %v", fake.stops)
	}
}

func lastLine(m *topModel) string {
	v := view(m)
	return v[strings.LastIndex(v, "\n")+1:]
}

func TestTopFilterAndStopped(t *testing.T) {
	m, _ := newTopTest(t, topSessions())
	press(m, "/", "p", "u", "p", "x", "backspace")
	if got := lastLine(m); got != "filter: pup_  (enter keeps it, esc clears it)" {
		t.Fatalf("filter line %q", got)
	}
	press(m, "enter")
	got := view(m)
	if !strings.Contains(got, "filter pup  watching") || !strings.Contains(got, "> 300-1794945540000000") || strings.Contains(got, "100-1794945540000000") {
		t.Fatalf("filtered view:\n%s", got)
	}
	press(m, "/", "esc")
	if !strings.Contains(view(m), "100-1794945540000000") {
		t.Fatal("esc did not clear the filter")
	}

	gone := topSessions()[0]
	m.Update(topUpdate{change(9, "Exited", &berthv1.Session{Id: gone.GetId(), State: berthv1.SessionState_SESSION_STATE_STOPPED, StateSince: timestamppb.New(topNow)})})
	got = view(m)
	if !strings.HasPrefix(got, "berth top  3 live, 1 stopped (a shows them)  watching") || strings.Contains(got, "100-1794945540000000") {
		t.Fatalf("a stopped session is listed without a:\n%s", got)
	}
	press(m, "a")
	got = view(m)
	if !strings.Contains(got, "100-1794945540000000  stopped   headless shell      playwright  yes       agent claude (pid 20)") {
		t.Fatalf("a bare change lost the session's facts, or a did not show stopped sessions:\n%s", got)
	}
	m.Update(topUpdate{snapshot(20, topSessions()[3])})
	if got := view(m); !strings.HasPrefix(got, "berth top  1 live, 0 stopped  watching") {
		t.Fatalf("a snapshot did not replace the list:\n%s", got)
	}
}

func TestTopKeepsAtMostTheNewestStopped(t *testing.T) {
	m, _ := newTopTest(t, nil)
	for i := range topStoppedMax + 5 {
		m.Update(topUpdate{change(int64(i+1), "Exited", &berthv1.Session{
			Id: strings.Repeat("1", i+1) + "-1", State: berthv1.SessionState_SESSION_STATE_STOPPED, StateSince: timestamppb.New(topNow.Add(time.Duration(i) * time.Second)),
		})})
	}
	if len(m.sessions) != topStoppedMax {
		t.Fatalf("berth top keeps %d stopped sessions, want %d", len(m.sessions), topStoppedMax)
	}
	if _, ok := m.sessions["1-1"]; ok {
		t.Fatal("the oldest stopped session was kept")
	}
}

type fakeStream struct {
	msgs []*berthv1.WatchSessionsResponse
	end  error
}

func (f *fakeStream) Receive() (*berthv1.WatchSessionsResponse, error) {
	if len(f.msgs) == 0 {
		return nil, f.end
	}
	m := f.msgs[0]
	f.msgs = f.msgs[1:]
	return m, nil
}

func (f *fakeStream) Close() error { return nil }

// When a stream ends, berth top says so and resumes after the last event id it applied.
func TestTopResumesAfterTheLastEventID(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var afters []int64
	streams := []*fakeStream{
		{msgs: []*berthv1.WatchSessionsResponse{snapshot(5), change(9, "Seen", &berthv1.Session{Id: "1-1"})}, end: io.EOF},
		{msgs: []*berthv1.WatchSessionsResponse{change(12, "Busy", &berthv1.Session{Id: "1-1"})}, end: errors.New("connection reset")},
		{end: errors.New("refused")},
	}
	watch := func(_ context.Context, after int64) (sessionStream, error) {
		afters = append(afters, after)
		if len(afters) > len(streams) {
			cancel()
			return nil, context.Canceled
		}
		return streams[len(afters)-1], nil
	}
	out := make(chan tea.Msg)
	done := make(chan struct{})
	go func() { watchLoop(ctx, watch, out, time.Millisecond); close(done) }()
	var got []string
	for {
		select {
		case msg := <-out:
			switch msg := msg.(type) {
			case topUpdate:
				got = append(got, "update")
			case topDown:
				got = append(got, "down: "+msg.err.Error())
			}
			continue
		case <-done:
		}
		break
	}
	if want := []int64{0, 9, 12, 12}; !slices.Equal(afters, want) {
		t.Fatalf("WatchSessions after %v, want %v", afters, want)
	}
	want := []string{"update", "update", "down: berthd ended the stream", "update", "down: connection reset", "down: refused"}
	if !slices.Equal(got, want) {
		t.Fatalf("messages %v, want %v", got, want)
	}
}

func TestTopNeedsATerminal(t *testing.T) {
	r := invokeSessions(t, &fakeSessions{}, "top")
	if r.code != ExitUsage || !strings.Contains(r.stderr, "needs a terminal") {
		t.Fatalf("top without a terminal: %+v", r)
	}
}

// On a short terminal the list keeps the selection in view and the screen never grows past it.
func TestTopFitsTheTerminal(t *testing.T) {
	m, _ := newTopTest(t, topSessions())
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	press(m, "down", "down", "down")
	got := view(m)
	lines := strings.Split(got, "\n")
	if len(lines) > 12 || !strings.Contains(got, "> 400-") || strings.Contains(got, "100-1794945540000000") {
		t.Fatalf("%d lines:\n%s", len(lines), got)
	}
	for _, line := range lines {
		if len([]rune(line)) > 40 {
			t.Fatalf("line wider than 40: %q", line)
		}
	}
	if lines[len(lines)-1] != "keys: up/down move  i inspect  s stop  /" {
		t.Fatalf("key line not kept at the bottom: %q", lines[len(lines)-1])
	}
}

// A panic in the stream goroutine ends berth top through Bubble Tea, which restores the terminal,
// instead of killing the process.
func TestTopQuitsCleanlyWhenItsStreamPanics(t *testing.T) {
	m := newTop(t.Context(), topDeps{
		watch: func(context.Context, int64) (sessionStream, error) { panic("boom") },
		now:   func() time.Time { return topNow },
	})
	go m.watch()
	msg := m.next()()
	if _, ok := msg.(topFault); !ok {
		t.Fatalf("got %#v, want topFault", msg)
	}
	_, cmd := m.Update(msg)
	if cmd == nil {
		t.Fatal("no command after a fault")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit || m.fault == nil || !strings.Contains(m.fault.Error(), "boom") {
		t.Fatalf("fault %v did not quit", m.fault)
	}
}
