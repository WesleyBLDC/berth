// Package discovery turns the process table into browser sessions, and each session's
// observation into the events its machine should see. A Scanner reads through the ProcessSource
// port; Events is pure. Arguments are read only for browser processes and the ancestors whose
// arguments name an agent (node, bun, deno), once per process instance, and only for the owner's
// own processes, since the kernel refuses everyone else's.
package discovery

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"berth/internal/core/browsersig"
	"berth/internal/core/ownerchain"
	"berth/internal/core/ports"
	"berth/internal/core/session"
)

const (
	// BusyCPU is the CPU growth between two scans that makes a session Busy.
	BusyCPU = 20 * time.Millisecond
	// QuietAfter is how long a session's CPU must stay flat before it is Quiet.
	QuietAfter = 60 * time.Second
	launchdPID = 1
	maxDepth   = 64
)

// Member is one process of a session.
type Member struct {
	ports.ProcessInfo
	Helper bool
}

// Observed is one session as one scan saw it.
type Observed struct {
	ID        string
	Root      ports.ProcessInfo
	Signature browsersig.Signature
	Owner     ownerchain.Owner
	// Chain runs from the browser's parent upward, as far as the kernel describes it.
	Chain   []ports.ProcessInfo
	Members []Member
	// CPU is the summed CPU time of the members the kernel would describe.
	CPU time.Duration
	// OtherUser marks a browser run by another user: unmanaged and opaque.
	OtherUser bool
}

// ID names a session by its browser's pid and start time, so a reused pid is a new session.
func ID(root ports.ProcessRef) string {
	return fmt.Sprintf("%d-%d", root.PID, root.Start.UnixMicro())
}

type candidate struct {
	ports.ProcessInfo
	sig browsersig.Signature
}

// Scanner lists browser sessions scan after scan. It remembers what it read about each process
// instance, so a scan reads arguments only for processes it has not seen; a long lived Chrome
// would otherwise cost a kern.procargs2 read per helper on every scan. It is not safe for
// concurrent use.
type Scanner struct {
	src ports.ProcessSource
	// uid is berthd's own user: its processes are read in full, everyone else's by path only.
	uid uint32
	// sigs and args hold the last scan's classifications and ancestor arguments; each scan
	// keeps only the processes it listed, so a process that is gone is forgotten.
	sigs map[instance]classified
	args map[instance][]string
}

// instance keys a process by its executable too, since exec keeps the pid and start time.
type instance struct {
	pid   int
	start int64
	exe   string
}

type classified struct {
	sig browsersig.Signature
	ok  bool
}

func NewScanner(src ports.ProcessSource, uid uint32) *Scanner {
	return &Scanner{src: src, uid: uid}
}

// Scan lists every browser session running now.
func (s *Scanner) Scan(ctx context.Context) ([]Observed, error) {
	procs, err := s.src.List(ctx)
	if err != nil {
		return nil, err
	}
	byPID := make(map[int]ports.ProcessInfo, len(procs))
	children := make(map[int][]int)
	for _, p := range procs {
		byPID[p.PID] = p
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	read := func(p ports.ProcessInfo) ([]string, bool) {
		if p.UID != s.uid {
			return nil, false
		}
		a, err := s.src.Args(ctx, p.ProcessRef)
		return a, err == nil
	}
	sigs, args := make(map[instance]classified, len(s.sigs)), make(map[instance][]string, len(s.args))
	browsers := make(map[int]candidate)
	for _, p := range procs {
		if _, ok := browsersig.Classify(p.Executable, nil, false); !ok {
			continue
		}
		k := key(p)
		c, hit := s.sigs[k]
		if !hit {
			a, known := read(p)
			c.sig, c.ok = browsersig.Classify(p.Executable, a, known)
		}
		sigs[k] = c
		if c.ok {
			browsers[p.PID] = candidate{p, c.sig}
		}
	}
	ancestorArgs := func(p ports.ProcessInfo) []string {
		if !ownerchain.ReadsArgs(p.Executable) {
			return nil
		}
		k := key(p)
		a, hit := s.args[k]
		if !hit {
			a, _ = read(p)
		}
		args[k] = a
		return a
	}
	var out []Observed
	for _, p := range procs {
		c, ok := browsers[p.PID]
		if !ok || c.sig.Role != browsersig.RoleBrowser {
			continue
		}
		if parent, ok := browsers[p.PPID]; ok && parent.sig.Kind == c.sig.Kind {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, observe(ctx, s.src, c, byPID, children, ancestorArgs, s.uid))
	}
	s.sigs, s.args = sigs, args
	return out, nil
}

func key(p ports.ProcessInfo) instance {
	return instance{pid: p.PID, start: p.Start.UnixNano(), exe: p.Executable}
}

func observe(ctx context.Context, src ports.ProcessSource, root candidate, byPID map[int]ports.ProcessInfo,
	children map[int][]int, args func(ports.ProcessInfo) []string, uid uint32,
) Observed {
	o := Observed{ID: ID(root.ProcessRef), Root: root.ProcessInfo, Signature: root.sig, OtherUser: root.UID != uid}
	o.Members = []Member{{ProcessInfo: root.ProcessInfo}}
	for queue := children[root.PID]; len(queue) > 0; queue = queue[1:] {
		p := byPID[queue[0]]
		o.Members = append(o.Members, Member{ProcessInfo: p, Helper: true})
		queue = append(queue, children[p.PID]...)
	}
	for _, m := range o.Members {
		if m.UID != uid {
			continue
		}
		if cpu, err := src.CPUTime(ctx, m.ProcessRef); err == nil {
			o.CPU += cpu
		}
	}
	var chain []ownerchain.Ancestor
	seen := map[int]bool{root.PID: true}
	for pid := root.PPID; len(chain) < maxDepth && !seen[pid]; {
		p, ok := byPID[pid]
		if !ok {
			break
		}
		seen[pid] = true
		chain = append(chain, ownerchain.Ancestor{ProcessInfo: p, Args: args(p)})
		o.Chain = append(o.Chain, p)
		if pid == launchdPID {
			break
		}
		pid = p.PPID
	}
	o.Owner = ownerchain.Find(chain, automated(root.sig))
	return clipped(o)
}

// automated is real evidence of a driver: a Playwright or Puppeteer install or profile, headless,
// or a CDP pipe. A CDP port alone is not: a person can open their own browser with one.
func automated(s browsersig.Signature) bool {
	return s.Driver == browsersig.Playwright || s.Driver == browsersig.Puppeteer || s.Headless || s.CDP == browsersig.CDPPipe
}

// MaxText bounds every string berthd keeps about a session. Paths, profile folders and names are
// chosen by whoever started the process, so one process must not make berthd hold megabytes.
const MaxText = 1024

// Truncated marks a string MaxText cut short.
const Truncated = "...(truncated)"

// Clip cuts s to MaxText bytes, the mark included, on a UTF-8 boundary.
func Clip(s string) string {
	if len(s) <= MaxText {
		return s
	}
	cut := MaxText - len(Truncated)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.Clone(s[:cut]) + Truncated
}

func clipped(o Observed) Observed {
	info := func(p ports.ProcessInfo) ports.ProcessInfo {
		p.Executable = Clip(p.Executable)
		return p
	}
	o.Root = info(o.Root)
	o.Signature.UserDataDir = Clip(o.Signature.UserDataDir)
	o.Owner.Name = Clip(o.Owner.Name)
	o.Owner.Process = info(o.Owner.Process)
	for i := range o.Members {
		o.Members[i].ProcessInfo = info(o.Members[i].ProcessInfo)
	}
	for i := range o.Chain {
		o.Chain[i] = info(o.Chain[i])
	}
	return o
}

// Tracked is what the shell remembers about a live session between scans: facts, never a
// second state beside the machine's.
type Tracked struct {
	State session.State
	// Seen is false until the first observation fills the rest.
	Seen bool
	// Unsaved means the machine has no row for this session yet, so its first observation sends
	// session.Seen before anything else.
	Unsaved bool
	// Parent and Owner are the processes above the browser when it was first seen; either
	// changing means the process that started it exited.
	Parent ports.ProcessRef
	Owner  ports.ProcessRef
	CPU    time.Duration
	// CPUGrewAt is when CPU last grew by BusyCPU, or the first observation.
	CPUGrewAt time.Time
}

// Events is the pure step for one session: given what the shell tracked, this scan's observation
// (nil when the browser is gone) and the time, it returns the updated facts and the events the
// machine accepts in its current state, so a steady browser produces none.
func Events(t Tracked, o *Observed, now time.Time) (Tracked, []session.Event) {
	live := t.State != session.Stopped
	if o == nil {
		if live {
			return t, []session.Event{{Kind: session.Exited, Note: "gone from the process table"}}
		}
		return t, nil
	}
	var first []session.Event
	if !t.Seen {
		t.Seen, t.CPU, t.CPUGrewAt = true, o.CPU, now
		t.Parent = parentRef(o)
		t.Owner = o.Owner.Process.ProcessRef
		if t.Unsaved && t.State == session.Discovered {
			first = []session.Event{{Kind: session.Seen}}
		}
		t.Unsaved = false
	}
	next, events := step(t, o, now)
	return next, append(first, events...)
}

// step returns the one event, if any, that this observation of a live browser calls for.
func step(t Tracked, o *Observed, now time.Time) (Tracked, []session.Event) {
	busy := o.CPU-t.CPU >= BusyCPU
	if busy {
		t.CPUGrewAt = now
	}
	t.CPU = o.CPU
	observing := t.State == session.Discovered || t.State == session.Active || t.State == session.Idle
	switch {
	case observing && o.Owner.Kind == ownerchain.None:
		return t, []session.Event{{Kind: session.OwnerExited, Note: "no owner above the browser"}}
	case observing && !parentRef(o).Same(t.Parent):
		return t, []session.Event{{Kind: session.OwnerExited, Note: "the browser's parent exited"}}
	case observing && !o.Owner.Process.Same(t.Owner):
		return t, []session.Event{{Kind: session.OwnerExited, Note: "the owner exited"}}
	case busy && (t.State == session.Discovered || t.State == session.Idle):
		return t, []session.Event{{Kind: session.Busy}}
	case !busy && now.Sub(t.CPUGrewAt) >= QuietAfter && (t.State == session.Discovered || t.State == session.Active):
		return t, []session.Event{{Kind: session.Quiet}}
	}
	return t, nil
}

func parentRef(o *Observed) ports.ProcessRef {
	if len(o.Chain) == 0 {
		return ports.ProcessRef{}
	}
	return o.Chain[0].ProcessRef
}
