package e2e

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"berth/internal/cli"
)

// topDriver runs berth top's model the way Bubble Tea does, without a terminal: every command
// runs in its own goroutine and its message goes back through Update.
type topDriver struct {
	m    tea.Model
	msgs chan tea.Msg
}

func (d *topDriver) run(cmd tea.Cmd) {
	if cmd != nil {
		go func() { d.msgs <- cmd() }()
	}
}

func (d *topDriver) handle(msg tea.Msg) {
	switch msg := msg.(type) {
	case nil, tea.QuitMsg:
	case tea.BatchMsg:
		for _, c := range msg {
			d.run(c)
		}
	default:
		var cmd tea.Cmd
		d.m, cmd = d.m.Update(msg)
		d.run(cmd)
	}
}

func (d *topDriver) view() string { return d.m.View().Content }

func (d *topDriver) press(keys ...string) {
	for _, k := range keys {
		msg := tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
		if k == "enter" {
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		}
		d.handle(msg)
	}
}

// until feeds messages to the model until its view satisfies ok.
func (d *topDriver) until(t *testing.T, what string, ok func(view string) bool) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for !ok(d.view()) {
		select {
		case msg := <-d.msgs:
			d.handle(msg)
		case <-deadline:
			t.Fatalf("berth top never %s; view:\n%s", what, d.view())
		}
	}
}

func rowOf(view, id string) string {
	for line := range strings.Lines(view) {
		if f := strings.Fields(line); len(f) >= 2 && (f[0] == id || f[0] == ">" && f[1] == id) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// berth top's model, fed only by WatchSessions from the real berthd, shows a real headless
// browser appear, and stops it through StopSession after the confirmation.
func TestTopSeesABrowserAppearAndStopsIt(t *testing.T) {
	chrome := findChrome(t)
	h := home(t)
	start(t, h)
	c := waitServing(t, h)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := &topDriver{m: cli.NewTop(ctx, c), msgs: make(chan tea.Msg, 16)}
	d.run(d.m.Init())
	d.until(t, "connected", func(v string) bool { return strings.Contains(v, "  watching") })

	_, args := chromeArgs(t)
	browser := exec.CommandContext(t.Context(), chrome, args...)
	if err := browser.Start(); err != nil {
		t.Fatal(err)
	}
	pid := browser.Process.Pid
	killOnCleanup(t, pid)
	exited := make(chan struct{})
	go func() { _ = browser.Wait(); close(exited) }()

	prefix := strconv.Itoa(pid) + "-"
	var id string
	d.until(t, "listed the browser", func(v string) bool {
		for line := range strings.Lines(v) {
			for _, f := range strings.Fields(line) {
				if strings.HasPrefix(f, prefix) {
					id = f
					return true
				}
			}
		}
		return false
	})

	d.press(append(strings.Split("/"+prefix, ""), "enter", "a")...)
	if row := rowOf(d.view(), id); !strings.HasPrefix(row, "> "+id) || !strings.Contains(row, "headless shell") {
		t.Fatalf("the filtered list does not select the browser: %q\n%s", row, d.view())
	}
	d.press("s")
	if v := d.view(); !strings.Contains(v[strings.LastIndex(v, "\n")+1:], "stop "+id) {
		t.Fatalf("no confirmation:\n%s", v)
	}
	d.press("y")
	d.until(t, "showed the browser stopped", func(v string) bool {
		f := strings.Fields(rowOf(v, id))
		return len(f) > 2 && f[2] == "stopped" && strings.HasSuffix(v, "stopped "+id)
	})
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("berth top showed the browser stopped, but it is still running")
	}
}
