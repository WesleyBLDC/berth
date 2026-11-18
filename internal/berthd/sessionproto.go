package berthd

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/core/browsersig"
	"berth/internal/core/ownerchain"
	"berth/internal/core/ports"
	"berth/internal/core/session"
)

func sessionProto(t *tracked) *berthv1.Session {
	o, sig := t.obs, t.obs.Signature
	s := &berthv1.Session{
		Id: o.ID, State: stateProto(t.disc.State), Kind: kindProto[sig.Kind], Headless: sig.Headless,
		Browser:      processProto(o.Root, berthv1.ProcessRole_PROCESS_ROLE_BROWSER),
		ProcessCount: uint32(len(o.Members)), //nolint:gosec // a process count fits
		OtherUser:    o.OtherUser, Uid: o.Root.UID, StateSince: timestamppb.New(t.since),
		Owner: &berthv1.SessionOwner{Kind: ownerProto[o.Owner.Kind], Name: o.Owner.Name},
	}
	if o.Owner.Process.PID > 0 {
		s.Owner.Process = processProto(o.Owner.Process, berthv1.ProcessRole_PROCESS_ROLE_ANCESTOR)
	}
	if o.OtherUser || !sig.ArgsKnown {
		s.ArgsUnreadable = !o.OtherUser
		return s
	}
	s.Driver, s.Cdp, s.UserDataDir = driverProto[sig.Driver], cdpProto[sig.CDP], sig.UserDataDir
	if sig.CDP == browsersig.CDPPort {
		s.CdpPort = uint32(sig.CDPPort) //nolint:gosec // Classify keeps ports within 0 to 65535
	}
	return s
}

func processProto(p ports.ProcessInfo, role berthv1.ProcessRole) *berthv1.SessionProcess {
	return &berthv1.SessionProcess{
		Pid: int32(p.PID), StartedAt: timestamppb.New(p.Start), Executable: p.Executable, Role: role, //nolint:gosec // macOS pids fit in 32 bits
		ExecutableChanged: p.ExecutableChanged,
	}
}

func stateProto(s session.State) berthv1.SessionState {
	switch s {
	case session.Discovered:
		return berthv1.SessionState_SESSION_STATE_DISCOVERED
	case session.Active:
		return berthv1.SessionState_SESSION_STATE_ACTIVE
	case session.Idle:
		return berthv1.SessionState_SESSION_STATE_IDLE
	case session.Leftover:
		return berthv1.SessionState_SESSION_STATE_LEFTOVER
	case session.Stopping:
		return berthv1.SessionState_SESSION_STATE_STOPPING
	case session.Stopped:
		return berthv1.SessionState_SESSION_STATE_STOPPED
	}
	return berthv1.SessionState_SESSION_STATE_UNSPECIFIED
}

var (
	kindProto = map[browsersig.Kind]berthv1.BrowserKind{
		browsersig.Chrome: berthv1.BrowserKind_BROWSER_KIND_CHROME, browsersig.ChromeForTesting: berthv1.BrowserKind_BROWSER_KIND_CHROME_FOR_TESTING,
		browsersig.Chromium: berthv1.BrowserKind_BROWSER_KIND_CHROMIUM, browsersig.Edge: berthv1.BrowserKind_BROWSER_KIND_EDGE,
		browsersig.Brave: berthv1.BrowserKind_BROWSER_KIND_BRAVE, browsersig.HeadlessShell: berthv1.BrowserKind_BROWSER_KIND_HEADLESS_SHELL,
	}
	driverProto = map[browsersig.Driver]berthv1.Driver{
		browsersig.DriverNone: berthv1.Driver_DRIVER_NONE, browsersig.Playwright: berthv1.Driver_DRIVER_PLAYWRIGHT,
		browsersig.Puppeteer: berthv1.Driver_DRIVER_PUPPETEER, browsersig.Automation: berthv1.Driver_DRIVER_AUTOMATION,
	}
	cdpProto = map[browsersig.CDP]berthv1.CdpMode{
		browsersig.CDPNone: berthv1.CdpMode_CDP_MODE_NONE, browsersig.CDPPipe: berthv1.CdpMode_CDP_MODE_PIPE, browsersig.CDPPort: berthv1.CdpMode_CDP_MODE_PORT,
	}
	ownerProto = map[ownerchain.Kind]berthv1.OwnerKind{
		ownerchain.Agent: berthv1.OwnerKind_OWNER_KIND_AGENT, ownerchain.Terminal: berthv1.OwnerKind_OWNER_KIND_TERMINAL,
		ownerchain.App: berthv1.OwnerKind_OWNER_KIND_APP, ownerchain.Person: berthv1.OwnerKind_OWNER_KIND_PERSON,
		ownerchain.None: berthv1.OwnerKind_OWNER_KIND_NONE, ownerchain.Process: berthv1.OwnerKind_OWNER_KIND_PROCESS,
	}
)
