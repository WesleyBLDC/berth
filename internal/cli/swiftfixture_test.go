package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"unicode"
	"unicode/utf8"

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
	"\u0301acute first", "\ufe0fselector first", "\U000E0100supplement selector first", "e\u0301 inner mark",
	`"x\n"`, `"`, `a"b`,
}

// quoted is the app's always quoting form: strconv.QuoteToGraphic, with a leading combining mark
// or variation selector escaped too, since it would join the opening quote. It is the reference
// the Swift quoted is checked against.
func quoted(text string) string {
	q := strconv.QuoteToGraphic(text)
	r, size := utf8.DecodeRuneInString(q[1:])
	if !unicode.Is(unicode.M, r) {
		return q
	}
	esc := fmt.Sprintf(`\u%04x`, r)
	if r > 0xffff {
		esc = fmt.Sprintf(`\U%08x`, r)
	}
	return `"` + esc + q[1+size:]
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
	// Quoted is the text always quoted (see quoted), as the app labels agent chosen text in
	// notifications.
	Quoted string `json:"quoted"`
}

func fixtureStatus() *berthv1.StatusResponse {
	return &berthv1.StatusResponse{
		State: "Serving", StateVersion: 9007199254740993, Pid: 4321, StartedAt: &timestamppb.Timestamp{Seconds: 1763380000},
		SocketPath: "/Users/u/Library/Application Support/Berth/berthd.sock", DaemonVersion: "dev",
		Executable: "/Applications/Berth.app/Contents/Helpers/berthd",
	}
}

func fixtureNeeds() []*berthv1.WatchNeedsResponse {
	full := &berthv1.Need{
		Id: "session/4242-1763380000123456", Kind: berthv1.NeedKind_NEED_KIND_LEFTOVER, SecretName: "gh-token",
		OwnerName: "node\u202e", Browser: berthv1.BrowserKind_BROWSER_KIND_HEADLESS_SHELL,
	}
	return []*berthv1.WatchNeedsResponse{
		{Update: &berthv1.WatchNeedsResponse_Snapshot{Snapshot: &berthv1.NeedsSnapshot{
			EventId: 9007199254740993, Needs: []*berthv1.Need{full}, Reason: berthv1.SnapshotReason_SNAPSHOT_REASON_BEHIND,
		}}},
		{Update: &berthv1.WatchNeedsResponse_Change{Change: &berthv1.NeedChange{
			EventId: 9007199254740995, Event: berthv1.NeedEvent_NEED_EVENT_OPENED, Need: full,
		}}},
		{Update: &berthv1.WatchNeedsResponse_Change{Change: &berthv1.NeedChange{
			EventId: 9007199254740996, Event: berthv1.NeedEvent_NEED_EVENT_RESOLVED,
			Need: &berthv1.Need{Id: "grant/7", Kind: berthv1.NeedKind_NEED_KIND_GRANT, SecretName: "gh-token"},
		}}},
		{Update: &berthv1.WatchNeedsResponse_Cursor{Cursor: &berthv1.NeedsCursor{EventId: 9007199254740999}}},
	}
}

func TestSwiftFixtures(t *testing.T) {
	want := fixtureSessions()
	requireEveryField(t, want[0].GetSnapshot().GetSessions()[0].ProtoReflect())
	messageFixture(t, "sessions.json", want, func() *berthv1.WatchSessionsResponse { return &berthv1.WatchSessionsResponse{} })
	needs := fixtureNeeds()
	requireEveryField(t, needs[0].GetSnapshot().ProtoReflect())
	requireEveryField(t, needs[0].GetSnapshot().GetNeeds()[0].ProtoReflect())
	requireEveryField(t, needs[1].GetChange().ProtoReflect())
	messageFixture(t, "needs.json", needs, func() *berthv1.WatchNeedsResponse { return &berthv1.WatchNeedsResponse{} })
	requireEveryField(t, fixtureStatus().ProtoReflect())
	messageFixture(t, "status.json", []*berthv1.StatusResponse{fixtureStatus()}, func() *berthv1.StatusResponse { return &berthv1.StatusResponse{} })

	cases := make([]fixtureCase, 0, len(shownCorpus))
	for _, in := range shownCorpus {
		cases = append(cases, fixtureCase{In: in, Out: shown(in), Quoted: quoted(in)})
	}
	if shown("rtl\u202eabc") == "rtl\u202eabc" || shown("claude") != "claude" || quoted("\u0301") != `"\u0301"` {
		t.Fatal("the corpus no longer exercises both of shown's branches")
	}
	shownPath := filepath.Join(swiftFixtures, "shown.json")
	if *updateSwift {
		writeJSON(t, shownPath, cases)
	}
	raw, err := os.ReadFile(shownPath)
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
			t.Fatalf("%s case %d is %+v, Go says %+v; rerun with -update-swift", shownPath, i, got[i], cases[i])
		}
	}
}

// messageFixture writes want with protojson under -update-swift, and otherwise checks that the
// committed file still holds exactly want. A file of one message holds it alone, else a list.
func messageFixture[M proto.Message](t *testing.T, name string, want []M, fresh func() M) {
	t.Helper()
	path := filepath.Join(swiftFixtures, name)
	if *updateSwift {
		list := make([]json.RawMessage, 0, len(want))
		for _, m := range want {
			b, err := protojson.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			list = append(list, b)
		}
		if len(list) == 1 {
			writeJSON(t, path, list[0])
		} else {
			writeJSON(t, path, list)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run go test ./internal/cli -run TestSwiftFixtures -update-swift", err)
	}
	var messages []json.RawMessage
	if len(want) == 1 {
		messages = []json.RawMessage{raw}
	} else if err := json.Unmarshal(raw, &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != len(want) {
		t.Fatalf("%d messages in %s, want %d; rerun with -update-swift", len(messages), path, len(want))
	}
	for i, m := range messages {
		got := fresh()
		if err := protojson.Unmarshal(m, got); err != nil {
			t.Fatalf("message %d of %s: %v", i, path, err)
		}
		if !proto.Equal(got, want[i]) {
			t.Fatalf("message %d of %s is stale; rerun with -update-swift", i, path)
		}
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
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
