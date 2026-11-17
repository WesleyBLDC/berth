package agentsession_test

import (
	"errors"
	"testing"
	"time"

	"berth/internal/core/agentsession"
	"berth/internal/core/machine"
	"berth/internal/core/machine/machinetest"
	"berth/internal/core/ports"
)

func TestEveryPairIsLegalOrRejected(t *testing.T) {
	// Bind + 2 Expire + 2 Revoke + RootExited.
	if got := machinetest.Walk(t, agentsession.States, agentsession.AllEvents, agentsession.Transition); got != 6 {
		t.Fatalf("legal pairs = %d, want 6; update this number only with a deliberate table change", got)
	}
}

func TestLifecycle(t *testing.T) {
	for _, c := range []struct {
		from agentsession.State
		kind agentsession.EventKind
		want agentsession.State
	}{
		{agentsession.Issued, agentsession.Bind, agentsession.Bound},
		{agentsession.Issued, agentsession.Expire, agentsession.Expired},
		{agentsession.Bound, agentsession.Expire, agentsession.Expired},
		{agentsession.Issued, agentsession.Revoke, agentsession.Revoked},
		{agentsession.Bound, agentsession.Revoke, agentsession.Revoked},
		{agentsession.Bound, agentsession.RootExited, agentsession.Ended},
	} {
		next, effects, err := agentsession.Transition(c.from, agentsession.Event{Kind: c.kind})
		if err != nil || next != c.want || len(effects) != 0 {
			t.Errorf("%s in %s: %s %v %v, want %s", c.kind, c.from, next, effects, err, c.want)
		}
	}
}

func TestEndedSessionsNeverComeBack(t *testing.T) {
	for _, from := range []agentsession.State{agentsession.Expired, agentsession.Revoked, agentsession.Ended} {
		if agentsession.Live(from) {
			t.Errorf("%s is live", from)
		}
		for _, e := range agentsession.AllEvents {
			next, _, err := agentsession.Transition(from, e)
			if !errors.Is(err, machine.ErrIllegal) || next != from {
				t.Fatalf("%s in %s: want ErrIllegal, got %s %v", e.Name(), from, next, err)
			}
		}
	}
	// A bound session is never bound again to another tree.
	if _, _, err := agentsession.Transition(agentsession.Bound, agentsession.Event{Kind: agentsession.Bind}); !errors.Is(err, machine.ErrIllegal) {
		t.Fatalf("Bind in Bound: %v", err)
	}
}

func proc(pid, ppid int, uid uint32, start int64) ports.ProcessInfo {
	return ports.ProcessInfo{ProcessRef: ports.ProcessRef{PID: pid, Start: time.Unix(start, 0)}, PPID: ppid, UID: uid}
}

func TestRootIsTheHighestAgentAncestor(t *testing.T) {
	const agent, root = 600, 0
	cases := []struct {
		name    string
		lineage []ports.ProcessInfo
		want    int
		ok      bool
	}{
		{"below a launch helper", []ports.ProcessInfo{proc(40, 30, agent, 4), proc(30, 20, agent, 3), proc(20, 10, root, 2), proc(10, 1, root, 1), proc(1, 0, root, 0)}, 30, true},
		{"adopted by launchd", []ports.ProcessInfo{proc(40, 1, agent, 4), proc(1, 0, root, 0)}, 40, true},
		{"top of the agent's own tree", []ports.ProcessInfo{proc(40, 30, agent, 4), proc(30, 1, agent, 3), proc(1, 0, root, 0)}, 30, true},
		{"parent exited mid walk", []ports.ProcessInfo{proc(40, 30, agent, 4)}, 40, true},
		{"caller is another user", []ports.ProcessInfo{proc(40, 30, 501, 4), proc(30, 1, agent, 3)}, 0, false},
		{"no lineage", nil, 0, false},
	}
	for _, c := range cases {
		got, ok := agentsession.Root(c.lineage, agent)
		if ok != c.ok || got.PID != c.want {
			t.Errorf("%s: got %d %v, want %d %v", c.name, got.PID, ok, c.want, c.ok)
		}
	}
}

func TestDescendsNeedsTheSameInstance(t *testing.T) {
	root := ports.ProcessRef{PID: 30, Start: time.Unix(3, 0)}
	child := []ports.ProcessInfo{proc(41, 30, 600, 5), proc(30, 20, 600, 3), proc(20, 1, 0, 2)}
	if !agentsession.Descends(child, root) {
		t.Fatal("a child of the root does not descend from it")
	}
	if !agentsession.Descends(child[1:], root) {
		t.Fatal("the root itself does not count")
	}
	reused := []ports.ProcessInfo{proc(41, 30, 600, 5), proc(30, 1, 600, 9)}
	if agentsession.Descends(reused, root) {
		t.Fatal("a later process on the root's pid counted as the root")
	}
	orphan := []ports.ProcessInfo{proc(41, 1, 600, 5), proc(1, 0, 0, 0)}
	if agentsession.Descends(orphan, root) {
		t.Fatal("a process launchd adopted counted as a descendant")
	}
}

func TestAdmitStore(t *testing.T) {
	now := time.Unix(10_000, 0)
	if ok, _ := agentsession.AdmitStore(agentsession.MaxOpenStores, nil, now); ok {
		t.Fatal("admitted beyond the open dialog limit")
	}
	var started []time.Time
	for i := range agentsession.MaxStoresPerHour {
		ok, kept := agentsession.AdmitStore(0, started, now.Add(time.Duration(i)*time.Minute))
		if !ok {
			t.Fatalf("store %d refused", i+1)
		}
		started = kept
	}
	at := now.Add(30 * time.Minute)
	if ok, _ := agentsession.AdmitStore(0, started, at); ok {
		t.Fatal("admitted an eleventh store inside the hour")
	}
	// An hour after the first, one slot frees.
	ok, kept := agentsession.AdmitStore(0, started, now.Add(time.Hour))
	if !ok || len(kept) != agentsession.MaxStoresPerHour {
		t.Fatalf("after an hour: %v with %d kept", ok, len(kept))
	}
}
