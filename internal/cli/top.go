package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/client"
)

const (
	// topRetry is how long berth top waits before it resumes a stream that ended.
	topRetry = time.Second
	// topStoppedMax bounds the stopped sessions berth top remembers, as berthd bounds its own.
	topStoppedMax = 200
	topHistory    = 10
	topKeys       = "keys: up/down move  i inspect  s stop  / filter  a stopped  q quit"
)

// sessionStream is the client side of WatchSessions.
type sessionStream interface {
	Receive() (*berthv1.WatchSessionsResponse, error)
	Close() error
}

type watchFunc func(ctx context.Context, after int64) (sessionStream, error)

type inspectFunc func(context.Context, *berthv1.InspectSessionRequest) (*berthv1.InspectSessionResponse, error)

// topDeps are the daemon calls and the clock berth top uses, so tests can replace them.
type topDeps struct {
	watch   watchFunc
	inspect inspectFunc
	stop    stopFunc
	now     func() time.Time
}

// Messages the model receives besides keys and window sizes.
type (
	topUpdate struct {
		resp *berthv1.WatchSessionsResponse
	}
	topDown      struct{ err error }
	topInspected struct {
		resp *berthv1.InspectSessionResponse
		err  error
	}
	topStopped struct {
		id   string
		resp *berthv1.StopSessionResponse
		err  error
	}
	topTick struct{}
	// topFault ends berth top when its stream goroutine panicked, so Bubble Tea still restores
	// the terminal.
	topFault struct{ err error }
)

type topConfirm struct {
	id    string
	force bool
}

// topModel is berth top's whole state. Update and View take no terminal, so tests drive them
// with messages and compare the text.
type topModel struct {
	ctx     context.Context
	deps    topDeps
	updates chan tea.Msg

	sessions    map[string]*berthv1.Session
	selected    string
	showStopped bool
	filter      string
	filtering   bool
	confirm     *topConfirm
	inspected   *berthv1.InspectSessionResponse
	link        string
	status      string
	fault       error
	width       int
	height      int
}

// NewTop is berth top's model, fed by WatchSessions through c; Init starts the stream.
func NewTop(ctx context.Context, c *client.Client) tea.Model {
	return newTop(ctx, topDeps{
		watch: func(ctx context.Context, after int64) (sessionStream, error) {
			return c.Sessions.WatchSessions(ctx, &berthv1.WatchSessionsRequest{AfterEventId: after})
		},
		inspect: c.Sessions.InspectSession,
		stop:    c.Sessions.StopSession,
		now:     time.Now,
	})
}

func newTop(ctx context.Context, deps topDeps) *topModel {
	return &topModel{ctx: ctx, deps: deps, updates: make(chan tea.Msg), sessions: map[string]*berthv1.Session{}, link: "connecting"}
}

func top(ctx context.Context, env Env) int {
	out, ok := env.Stdout.(*os.File)
	if !ok || !isTerminal(out) {
		fmt.Fprintln(env.Stderr, "berth top needs a terminal; berth ps prints the same list once")
		return ExitUsage
	}
	c, ok := connectClient(ctx, env)
	if !ok {
		return ExitError
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	final, err := tea.NewProgram(NewTop(ctx, c), tea.WithContext(ctx), tea.WithInput(env.Stdin), tea.WithOutput(out)).Run()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		fmt.Fprintln(env.Stderr, err)
		return ExitError
	}
	if m, ok := final.(*topModel); ok && m.fault != nil {
		fmt.Fprintln(env.Stderr, shown(m.fault.Error()))
		return ExitError
	}
	return ExitOK
}

func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA) //nolint:gosec // descriptors fit in an int
	return err == nil
}

func (m *topModel) Init() tea.Cmd {
	return tea.Batch(func() tea.Msg {
		go m.watch()
		return nil
	}, m.next(), topTickCmd())
}

// watch runs watchLoop and turns a panic in it into topFault: a panic on this goroutine would
// otherwise kill the process without Bubble Tea restoring the terminal.
func (m *topModel) watch() {
	defer func() {
		if r := recover(); r != nil {
			select {
			case m.updates <- topFault{fmt.Errorf("berth top's stream failed: %v", r)}:
			case <-m.ctx.Done():
			}
		}
	}()
	watchLoop(m.ctx, m.deps.watch, m.updates, topRetry)
}

// watchLoop feeds out from WatchSessions, and when a stream ends, reports it and resumes after
// the last event id it delivered, so nothing is missed or applied twice out of order.
func watchLoop(ctx context.Context, watch watchFunc, out chan<- tea.Msg, retry time.Duration) {
	var after int64
	for {
		err := func() error {
			stream, err := watch(ctx, after)
			if err != nil {
				return err
			}
			defer func() { _ = stream.Close() }()
			for {
				resp, err := stream.Receive()
				if err != nil {
					return err
				}
				if snap := resp.GetSnapshot(); snap != nil {
					after = snap.GetEventId()
				} else {
					after = resp.GetChange().GetEventId()
				}
				select {
				case out <- topUpdate{resp}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}()
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, io.EOF) {
			err = errors.New("berthd ended the stream")
		}
		select {
		case out <- topDown{err}:
		case <-ctx.Done():
			return
		}
		select {
		case <-time.After(retry):
		case <-ctx.Done():
			return
		}
	}
}

func (m *topModel) next() tea.Cmd {
	return func() tea.Msg {
		select {
		case msg := <-m.updates:
			return msg
		case <-m.ctx.Done():
			return nil
		}
	}
}

func topTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return topTick{} })
}

func (m *topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case topUpdate:
		m.link = "watching"
		m.apply(msg.resp)
		return m, m.next()
	case topDown:
		m.link = "reconnecting: " + shown(msg.err.Error())
		return m, m.next()
	case topTick:
		return m, topTickCmd()
	case topFault:
		m.fault = msg.err
		return m, tea.Quit
	case topInspected:
		switch {
		case msg.err != nil:
			m.status = errorLine(msg.err, m.selected)
		case msg.resp.GetSession().GetId() == m.selected:
			m.inspected = msg.resp
		}
	case topStopped:
		switch {
		case msg.err != nil:
			m.status = errorLine(msg.err, msg.id)
		case msg.resp.GetState() != berthv1.SessionState_SESSION_STATE_STOPPED:
			m.status = fmt.Sprintf("session %s is still %s after 10 seconds", shown(msg.id), stateName(msg.resp.GetState()))
		default:
			m.status = "stopped " + shown(msg.id)
		}
	case tea.KeyPressMsg:
		return m, m.key(msg)
	}
	return m, nil
}

func errorLine(err error, id string) string {
	var b strings.Builder
	reportError(&b, err, "", shown(id))
	return shown(strings.TrimSpace(b.String()))
}

// apply folds one stream message in: a snapshot replaces everything, a change replaces one
// session, keeping the facts it already had when the change carries only the state.
func (m *topModel) apply(resp *berthv1.WatchSessionsResponse) {
	if snap := resp.GetSnapshot(); snap != nil {
		m.sessions = make(map[string]*berthv1.Session, len(snap.GetSessions()))
		for _, s := range snap.GetSessions() {
			m.sessions[s.GetId()] = s
		}
		return
	}
	s := resp.GetChange().GetSession()
	if old, ok := m.sessions[s.GetId()]; ok && s.GetBrowser() == nil {
		merged := proto.CloneOf(old)
		merged.State, merged.StateSince = s.GetState(), s.GetStateSince()
		s = merged
	}
	m.sessions[s.GetId()] = s
	if s.GetState() == berthv1.SessionState_SESSION_STATE_STOPPED {
		m.trimStopped()
	}
}

func (m *topModel) trimStopped() {
	var stopped []*berthv1.Session
	for _, s := range m.sessions {
		if s.GetState() == berthv1.SessionState_SESSION_STATE_STOPPED {
			stopped = append(stopped, s)
		}
	}
	if len(stopped) <= topStoppedMax {
		return
	}
	slices.SortFunc(stopped, func(x, y *berthv1.Session) int {
		return cmp.Or(x.GetStateSince().AsTime().Compare(y.GetStateSince().AsTime()), strings.Compare(x.GetId(), y.GetId()))
	})
	for _, s := range stopped[:len(stopped)-topStoppedMax] {
		delete(m.sessions, s.GetId())
	}
}

func (m *topModel) key(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if key == "ctrl+c" {
		return tea.Quit
	}
	if c := m.confirm; c != nil {
		m.confirm = nil
		if key != "y" {
			m.status = "stop cancelled"
			return nil
		}
		m.status = "stopping " + shown(c.id)
		return m.stopCmd(c)
	}
	if m.filtering {
		switch key {
		case "enter":
			m.filtering = false
		case "esc":
			m.filter, m.filtering = "", false
		case "backspace":
			if r := []rune(m.filter); len(r) > 0 {
				m.filter = string(r[:len(r)-1])
			}
		default:
			m.filter += k.Text
		}
		return nil
	}
	m.status = ""
	switch key {
	case "q":
		return tea.Quit
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "i", "enter":
		return m.inspectCmd()
	case "esc":
		m.inspected = nil
	case "s":
		m.askStop()
	case "/":
		m.filtering = true
	case "a":
		m.showStopped = !m.showStopped
	}
	return nil
}

// visible is the sessions the list shows, oldest browser first.
func (m *topModel) visible() []*berthv1.Session {
	var out []*berthv1.Session
	needle := strings.ToLower(m.filter)
	for _, s := range m.sessions {
		if s.GetState() == berthv1.SessionState_SESSION_STATE_STOPPED && !m.showStopped {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(strings.Join(topRow(s, time.Time{})[:6], " ")), needle) {
			continue
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(x, y *berthv1.Session) int {
		return cmp.Or(x.GetBrowser().GetStartedAt().AsTime().Compare(y.GetBrowser().GetStartedAt().AsTime()), strings.Compare(x.GetId(), y.GetId()))
	})
	return out
}

// current is the selected session, or the first visible one when the selection is hidden.
func (m *topModel) current(list []*berthv1.Session) (int, *berthv1.Session) {
	for i, s := range list {
		if s.GetId() == m.selected {
			return i, s
		}
	}
	if len(list) == 0 {
		return -1, nil
	}
	return 0, list[0]
}

func (m *topModel) move(by int) {
	list := m.visible()
	i, _ := m.current(list)
	if i < 0 {
		return
	}
	i = min(max(i+by, 0), len(list)-1)
	if list[i].GetId() != m.selected {
		m.selected, m.inspected = list[i].GetId(), nil
	}
}

func (m *topModel) selection() *berthv1.Session {
	_, s := m.current(m.visible())
	return s
}

func (m *topModel) inspectCmd() tea.Cmd {
	s := m.selection()
	if s == nil {
		return nil
	}
	m.selected = s.GetId()
	req := &berthv1.InspectSessionRequest{Id: s.GetId(), HistoryLimit: topHistory}
	return func() tea.Msg {
		resp, err := m.deps.inspect(m.ctx, req)
		return topInspected{resp: resp, err: err}
	}
}

// askStop follows berth stop's rules: another user's browser is never offered a stop, and the
// person's or an app's is offered a forced one, named as such.
func (m *topModel) askStop() {
	s := m.selection()
	switch {
	case s == nil:
		m.status = "no session to stop"
	case s.GetOtherUser():
		m.status = fmt.Sprintf("%s belongs to another user; berth never stops it", shown(s.GetId()))
	case s.GetState() == berthv1.SessionState_SESSION_STATE_STOPPED:
		m.status = shown(s.GetId()) + " has already stopped"
	default:
		kind := s.GetOwner().GetKind()
		m.selected = s.GetId()
		m.confirm = &topConfirm{id: s.GetId(), force: kind == berthv1.OwnerKind_OWNER_KIND_PERSON || kind == berthv1.OwnerKind_OWNER_KIND_APP}
	}
}

func (m *topModel) stopCmd(c *topConfirm) tea.Cmd {
	req := &berthv1.StopSessionRequest{RequestId: requestID(), Id: c.id, Force: c.force}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, stopTimeout)
		defer cancel()
		resp, err := m.deps.stop(ctx, req)
		return topStopped{id: c.id, resp: resp, err: err}
	}
}

func (m *topModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "berth top"
	return v
}

// render lays the screen out as plain text: every process chosen string has been through shown,
// and lines are cut to the width after that, so no cut splits an escape.
func (m *topModel) render() string {
	now := m.deps.now()
	list := m.visible()
	at, sel := m.current(list)
	var lines []string
	add := func(text string) {
		for line := range strings.Lines(text) {
			lines = append(lines, strings.TrimRight(line, " \n"))
		}
	}

	add(m.header() + "\n\n")
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  ID\tSTATE\tBROWSER\tDRIVER\tHEADLESS\tOWNER\tAGE")
	first, last := m.window(len(list), at)
	for i, s := range list[first:last] {
		mark := "  "
		if first+i == at {
			mark = "> "
		}
		fmt.Fprintln(w, mark+strings.Join(topRow(s, now), "\t"))
	}
	_ = w.Flush()
	add(b.String())
	if len(list) == 0 {
		add("  no browser sessions\n")
	}
	if sel != nil {
		add("\n" + m.detail(sel))
	}
	foot := []string{"", topKeys}
	switch {
	case m.confirm != nil:
		foot = append(foot, m.confirmLine())
	case m.filtering:
		foot = append(foot, "filter: "+shown(m.filter)+"_  (enter keeps it, esc clears it)")
	case m.status != "":
		foot = append(foot, m.status)
	}
	if m.height > 0 && len(lines)+len(foot) > m.height {
		lines = lines[:max(m.height-len(foot), 0)]
	}
	lines = append(lines, foot...)
	for i, line := range lines {
		if r := []rune(line); m.width > 0 && len(r) > m.width {
			lines[i] = string(r[:m.width])
		}
	}
	return strings.Join(lines, "\n")
}

func (m *topModel) header() string {
	live, stopped := 0, 0
	for _, s := range m.sessions {
		if s.GetState() == berthv1.SessionState_SESSION_STATE_STOPPED {
			stopped++
		} else {
			live++
		}
	}
	h := fmt.Sprintf("berth top  %d live, %d stopped", live, stopped)
	if stopped > 0 && !m.showStopped {
		h += " (a shows them)"
	}
	if m.filter != "" && !m.filtering {
		h += "  filter " + shown(m.filter)
	}
	return h + "  " + m.link
}

// window picks which rows of the list fit: up to half the screen, keeping the selection in view.
func (m *topModel) window(n, at int) (int, int) {
	rows := n
	if m.height > 0 {
		rows = min(n, max(m.height/2-3, 3))
	}
	first := min(max(at-rows/2, 0), n-rows)
	return max(first, 0), max(first, 0) + rows
}

func (m *topModel) confirmLine() string {
	c := m.confirm
	s := m.sessions[c.id]
	if c.force {
		return fmt.Sprintf("stop %s? it belongs to %s, so this is a forced stop. y stops it, any other key cancels", shown(c.id), ownerName(s))
	}
	return fmt.Sprintf("stop %s (%s, owner %s)? y stops it, any other key cancels", shown(c.id), kindName(s.GetKind()), ownerName(s))
}

func (m *topModel) detail(s *berthv1.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session %s: %s since %s\n", shown(s.GetId()), stateName(s.GetState()), when(s.GetStateSince().AsTime()))
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "executable\t%s\n", executableName(s.GetBrowser()))
	fmt.Fprintf(w, "cdp\t%s\n", cdpName(s))
	fmt.Fprintf(w, "profile\t%s\n", profileName(s))
	fmt.Fprintf(w, "arguments\t%s\n", argsName(s))
	fmt.Fprintf(w, "owner\t%s\n", ownerName(s))
	_ = w.Flush()
	r := m.inspected
	if r == nil || r.GetSession().GetId() != s.GetId() {
		return b.String()
	}
	b.WriteString("\n")
	printProcesses(&b, slices.Concat(r.GetProcesses(), r.GetOwnerChain()))
	b.WriteString("\n")
	printHistory(&b, r.GetHistory())
	return b.String()
}

// topRow is one list row: id, state, browser, driver, headless, owner and age; the first six
// are also what the filter matches.
func topRow(s *berthv1.Session, now time.Time) []string {
	return []string{shown(s.GetId()), stateName(s.GetState()), kindName(s.GetKind()), driverName(s.GetDriver()), headlessName(s), ownerName(s), age(s, now)}
}

func age(s *berthv1.Session, now time.Time) string {
	start := s.GetBrowser().GetStartedAt()
	if start == nil || now.IsZero() {
		return "?"
	}
	d := max(now.Sub(start.AsTime()), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
