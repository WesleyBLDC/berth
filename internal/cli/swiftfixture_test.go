package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/codetrust"
)

// Berth.app decodes WatchSessions with hand written Swift models and escapes text with a Swift
// twin of shown (ADR 0019). These fixtures are the contract between the two: this test writes
// them from the real schema and the real shown with -update-swift, and otherwise checks that the
// committed files still say exactly that; the Swift tests read the same files.
var updateSwift = flag.Bool("update-swift", false, "rewrite the fixtures the Swift app tests read")

const swiftFixtures = "../../macos/Tests/BerthAppCoreTests/Fixtures"

// shownCorpus holds text a process could choose: plain, quoted and every kind of character
// shown must escape, with code points stable across Unicode versions.
var shownCorpus = []string{
	"", "claude", "Ghostty", "a b", "café", "日本語", "emoji 🙂",
	"quote\"back\\slash", "tab\there", "line\nbreak", "cr\rlf", "\x1b[31mred", "bell\a", "bs\b", "ff\f", "vt\v",
	"del\x7f", "nul\x00x", "rtl\u202eabc", "nel\u0085", "zwsp\u200b", "bom\ufeff", "ls\u2028", "ps\u2029",
	"quote\" and \x1b", "private\ue000", "tag\U000E0001", "supp private\U000F0000",
}

func fixtureSessions() []*berthv1.WatchSessionsResponse {
	at := func(sec int64, nanos int32) *timestamppb.Timestamp {
		return &timestamppb.Timestamp{Seconds: sec, Nanos: nanos}
	}
	full := &berthv1.Session{
		Id: "4242-1763380000123456", State: berthv1.SessionState_SESSION_STATE_ACTIVE,
		Kind: berthv1.BrowserKind_BROWSER_KIND_CHROME_FOR_TESTING, Driver: berthv1.Driver_DRIVER_PLAYWRIGHT,
		Headless: true, Cdp: berthv1.CdpMode_CDP_MODE_PORT, CdpPort: 9222, UserDataDir: "/tmp/berth profile",
		Owner: &berthv1.SessionOwner{
			Kind: berthv1.OwnerKind_OWNER_KIND_AGENT, Name: "claude\x1b[2J",
			Process: &berthv1.SessionProcess{
				Pid: 4000, StartedAt: at(1763379990, 0), Executable: "/usr/local/bin/claude",
				Role: berthv1.ProcessRole_PROCESS_ROLE_ANCESTOR, ExecutableChanged: true,
			},
		},
		Browser: &berthv1.SessionProcess{
			Pid: 4242, StartedAt: at(1763380000, 123456000), Executable: "/opt/chrome/Chrome for Testing",
			Role: berthv1.ProcessRole_PROCESS_ROLE_BROWSER, ExecutableChanged: true,
		},
		ProcessCount: 7, OtherUser: true, Uid: 501, StateSince: at(1763380005, 123456789), ArgsUnreadable: true,
	}
	return []*berthv1.WatchSessionsResponse{
		{Update: &berthv1.WatchSessionsResponse_Snapshot{Snapshot: &berthv1.SessionsSnapshot{
			// Above 2^53, so a client reading ids as doubles would get it wrong.
			EventId: 9007199254740993, Sessions: []*berthv1.Session{full},
			Reason: berthv1.SnapshotReason_SNAPSHOT_REASON_START,
		}}},
		{Update: &berthv1.WatchSessionsResponse_Change{Change: &berthv1.SessionChange{
			EventId: 9007199254740995, Event: "Exited",
			Session: &berthv1.Session{
				Id: full.GetId(), State: berthv1.SessionState_SESSION_STATE_STOPPED, StateSince: at(1763380010, 500000000),
			},
		}}},
	}
}

// requireEveryField fails when a message or a message it holds leaves a field unset, so a field
// added to Session must be added to the fixture, where the Swift tests meet it.
func requireEveryField(t *testing.T, m protoreflect.Message) {
	t.Helper()
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if !m.Has(fd) {
			t.Errorf("the Swift fixture leaves %s unset", fd.FullName())
			continue
		}
		if fd.Kind() == protoreflect.MessageKind && fd.Message().FullName() != "google.protobuf.Timestamp" && !fd.IsList() {
			requireEveryField(t, m.Get(fd).Message())
		}
	}
}

type fixtureCase struct {
	In  string `json:"in"`
	Out string `json:"out"`
}

func TestSwiftFixtures(t *testing.T) {
	want := fixtureSessions()
	requireEveryField(t, want[0].GetSnapshot().GetSessions()[0].ProtoReflect())
	cases := make([]fixtureCase, 0, len(shownCorpus))
	for _, in := range shownCorpus {
		cases = append(cases, fixtureCase{In: in, Out: shown(in)})
	}
	if shown("rtl\u202eabc") == "rtl\u202eabc" || shown("claude") != "claude" {
		t.Fatal("the corpus no longer exercises both of shown's branches")
	}
	sessionsPath := filepath.Join(swiftFixtures, "sessions.json")
	shownPath := filepath.Join(swiftFixtures, "shown.json")
	if *updateSwift {
		writeFixtures(t, sessionsPath, shownPath, want, cases)
	}

	raw, err := os.ReadFile(sessionsPath)
	if err != nil {
		t.Fatalf("%v; run go test ./internal/cli -run TestSwiftFixtures -update-swift", err)
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != len(want) {
		t.Fatalf("%d messages in %s, want %d; rerun with -update-swift", len(messages), sessionsPath, len(want))
	}
	for i, m := range messages {
		got := &berthv1.WatchSessionsResponse{}
		if err := protojson.Unmarshal(m, got); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if !proto.Equal(got, want[i]) {
			t.Fatalf("message %d of %s is stale; rerun with -update-swift", i, sessionsPath)
		}
	}

	raw, err = os.ReadFile(shownPath)
	if err != nil {
		t.Fatal(err)
	}
	var got []fixtureCase
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(cases) {
		t.Fatalf("%s is stale; rerun with -update-swift", shownPath)
	}
	for i := range cases {
		if got[i] != cases[i] {
			t.Fatalf("%s case %d is %q, shown says %q; rerun with -update-swift", shownPath, i, got[i].Out, cases[i].Out)
		}
	}
}

func writeFixtures(t *testing.T, sessionsPath, shownPath string, messages []*berthv1.WatchSessionsResponse, cases []fixtureCase) {
	t.Helper()
	var list []json.RawMessage
	for _, m := range messages {
		b, err := protojson.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, b)
	}
	for path, v := range map[string]any{sessionsPath: list, shownPath: cases} {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The app checks that the signed berthd answers with a requirement written in Swift; it must be
// the one Go pins berthd by in release builds, byte for byte.
func TestSwiftDaemonRequirementFixture(t *testing.T) {
	path := filepath.Join(swiftFixtures, "requirement.json")
	want := map[string]string{"daemon": codetrust.Requirement(codetrust.DaemonSigningID)}
	if *updateSwift {
		b, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["daemon"] != want["daemon"] {
		t.Fatalf("%s says %q, Go pins berthd by %q; rerun with -update-swift", path, got["daemon"], want["daemon"])
	}
}
