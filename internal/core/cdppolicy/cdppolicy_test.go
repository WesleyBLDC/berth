package cdppolicy_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"berth/internal/core/cdppolicy"
)

func ownOnly(id string) bool { return id == "BERTH-SESSION" }

func TestParse(t *testing.T) {
	m, err := cdppolicy.Parse([]byte(`{"id":7,"method":"Page.navigate","sessionId":"S","params":{"url":"data:,"}}`))
	if err != nil || m.ID != 7 || m.Method != "Page.navigate" || m.SessionID != "S" || string(m.Params) != `{"url":"data:,"}` {
		t.Fatalf("parse = %+v %v", m, err)
	}
	for _, raw := range []string{
		`[]`, `{"method":"Page.enable"}`, `{"id":1}`, `{"id":1.5,"method":"Page.enable"}`, `{"id":"1","method":"Page.enable"}`,
		`{"id":1,"method":7}`, `not json`, `{"id":1,"method":"Page.enable"} {}`, `{"id":1,"method":"Page.enable","params":[1]}`,
	} {
		if _, err := cdppolicy.Parse([]byte(raw)); !errors.Is(err, cdppolicy.ErrMalformed) {
			t.Fatalf("parse %s = %v, want ErrMalformed", raw, err)
		}
	}
}

// Go matches keys without regard to case and lets the last duplicate win; Chrome matches exactly.
// Any message the two could read differently is refused, never re-encoded.
func TestParseRefusesWhatGoAndChromeReadDifferently(t *testing.T) {
	for _, raw := range []string{
		// Go would see behavior "deny", Chrome "allow".
		`{"id":1,"method":"Browser.setDownloadBehavior","params":{"behavior":"allow","Behavior":"deny"}}`,
		// Exact duplicates, at the top, in params and deeper.
		`{"id":1,"method":"Page.enable","method":"Browser.close"}`,
		`{"id":1,"id":2,"method":"Page.enable"}`,
		`{"id":1,"method":"Page.navigate","params":{"url":"https://a.example","url":"file:///etc/passwd"}}`,
		`{"id":1,"method":"Runtime.evaluate","params":{"expression":"1","x":{"a":1,"a":2}}}`,
		// Go would read SessionId as the session; Chrome would send the message to the browser.
		`{"id":1,"method":"Page.enable","SessionId":"AGENT"}`,
		`{"id":1,"method":"Page.enable","sessionId":"AGENT","SESSIONID":"BERTH-SESSION"}`,
		`{"id":1,"Method":"Page.enable","method":"Browser.close"}`,
		`{"id":1,"method":"Page.navigate","params":{"URL":"file:///etc/passwd"}}`,
		`{"id":1,"method":"Target.attachToTarget","params":{"targetId":"T","Flatten":true}}`,
		`{"id":1,"method":"Input.dispatchDragEvent","params":{"type":"drop","data":{"Files":["/etc/passwd"]}}}`,
		// Unicode case folding: the long s folds to s.
		"{\"id\":1,\"method\":\"Page.enable\",\"ſessionId\":\"AGENT\"}",
	} {
		if _, err := cdppolicy.Parse([]byte(raw)); !errors.Is(err, cdppolicy.ErrMalformed) {
			t.Errorf("parse %s = %v, want ErrMalformed", raw, err)
		}
	}
}

// manyKeys is a message whose params hold n distinct keys, at a level the policy reads.
func manyKeys(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"id":1,"method":"Runtime.evaluate","params":{`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%d":%d`, i, i)
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}

// One message of 80,000 keys once took 22.6 seconds, since every key was compared with every
// key before it. Keys are now checked in constant time each, and an object past the limit is
// refused.
func TestParseIsLinearInTheKeyCount(t *testing.T) {
	start := time.Now()
	if _, err := cdppolicy.Parse(manyKeys(cdppolicy.MaxKeys)); err != nil {
		t.Fatalf("an object at the key limit must parse: %v", err)
	}
	if _, err := cdppolicy.Parse(manyKeys(80_000)); !errors.Is(err, cdppolicy.ErrMalformed) {
		t.Fatalf("an object past the key limit = %v, want ErrMalformed", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("parsing took %v; it must be linear in the key count", took)
	}
	// Folding still catches a late key that differs only by case from an early one.
	late := strings.Replace(string(manyKeys(cdppolicy.MaxKeys)), `}}`, `,"K0":0}}`, 1)
	if _, err := cdppolicy.Parse([]byte(strings.Replace(late, `"k9999":9999,`, ``, 1))); !errors.Is(err, cdppolicy.ErrMalformed) {
		t.Fatalf("a key folding onto an earlier one = %v, want ErrMalformed", err)
	}
}

func TestParseRefusesDeepNesting(t *testing.T) {
	nest := func(depth int, open, closing string) []byte {
		// The message object and params are two levels; the rest nest inside params.
		inner := strings.Repeat(open, depth-2) + "1" + strings.Repeat(closing, depth-2)
		if open == "{" {
			inner = strings.Repeat(`{"a":`, depth-2) + "1" + strings.Repeat("}", depth-2)
		}
		return []byte(`{"id":1,"method":"Runtime.evaluate","params":{"x":` + inner + `}}`)
	}
	for _, open := range []string{"[", "{"} {
		closing := map[string]string{"[": "]", "{": "}"}[open]
		if _, err := cdppolicy.Parse(nest(cdppolicy.MaxDepth, open, closing)); err != nil {
			t.Fatalf("%s nesting at the limit must parse: %v", open, err)
		}
		if _, err := cdppolicy.Parse(nest(cdppolicy.MaxDepth+1, open, closing)); !errors.Is(err, cdppolicy.ErrMalformed) {
			t.Fatalf("%s nesting past the limit = %v, want ErrMalformed", open, err)
		}
	}
}

func BenchmarkParseManyKeys(b *testing.B) {
	raw := manyKeys(cdppolicy.MaxKeys)
	for b.Loop() {
		if _, err := cdppolicy.Parse(raw); err != nil {
			b.Fatal(err)
		}
	}
}

type row struct {
	method, session, params string
	id                      int64
	allowed                 bool
}

func check(t *testing.T, rows []row) {
	t.Helper()
	for _, tc := range rows {
		id := tc.id
		if id == 0 {
			id = 1
		}
		m := cdppolicy.Message{ID: id, Method: tc.method, SessionID: tc.session}
		if tc.params != "" {
			m.Params = []byte(tc.params)
		}
		why := cdppolicy.Check(m, ownOnly)
		if (why == "") != tc.allowed {
			t.Errorf("%s %s %s id %d: refusal %q, want allowed %v", tc.method, tc.session, tc.params, id, why, tc.allowed)
		}
	}
}

func TestCheck(t *testing.T) {
	check(t, []row{
		{method: "Page.navigate", params: `{"url":"https://example.com"}`, allowed: true},
		{method: "Runtime.evaluate", session: "AGENT", allowed: true},
		{method: "Target.createTarget", params: `{"url":"about:blank"}`, allowed: true},
		{method: "Target.createTarget", params: `{"url":""}`, allowed: true},
		{method: "Target.attachToTarget", params: `{"targetId":"T","flatten":true}`, allowed: true},
		{method: "Target.setAutoAttach", params: `{"autoAttach":true,"waitForDebuggerOnStart":false,"flatten":true}`, allowed: true},
		{method: "Browser.getVersion", allowed: true},
		{method: "Network.getCookies", allowed: true},
		{method: "Storage.getCookies", allowed: true},
		{method: "Input.dispatchMouseEvent", allowed: true},
		{method: "Input.dispatchKeyEvent", params: `{"type":"keyDown","commands":["selectAll"]}`, allowed: true},
		{method: "Input.dispatchDragEvent", params: `{"type":"dragEnter","x":1,"y":1,"data":{"items":[],"dragOperationsMask":1}}`, allowed: true},
		{method: "Target.detachFromTarget", params: `{"sessionId":"AGENT"}`, allowed: true},
		{method: "Target.createBrowserContext", params: `{"disposeOnDetach":true}`, allowed: true},
		{method: "Fetch.continueRequest", params: `{"requestId":"r"}`, allowed: true},
		{method: "IO.read", params: `{"handle":"h"}`, allowed: true},

		{method: "Browser.close"},
		{method: "Browser.crash"},
		{method: "Browser.crashGpuProcess"},
		{method: "Browser.getBrowserCommandLine"},
		{method: "Browser.executeBrowserCommand"},
		{method: "Browser.setDownloadBehavior", params: `{"behavior":"deny"}`},
		{method: "Page.setDownloadBehavior", params: `{"behavior":"default"}`},
		{method: "Browser.grantPermissions", params: `{"permissions":["clipboardReadWrite"]}`},
		{method: "Browser.setPermission"},
		{method: "Browser.resetPermissions"},
		{method: "SystemInfo.getInfo"},
		{method: "SystemInfo.getProcessInfo"},
		{method: "Target.exposeDevToolsProtocol"},
		{method: "Target.setRemoteLocations"},
		{method: "Target.attachToBrowserTarget"},
		{method: "Tethering.bind"},
		{method: "Extensions.loadUnpacked"},
		{method: "PWA.install"},
		{method: "Cast.startTabMirroring"},
		{method: "Autofill.trigger"},
		{method: "NewDomain.doSomething"},
		{method: "nodot"},
		{method: ".enable"},
		{method: "Page."},
		{method: "Page.enable", session: "BERTH-SESSION"},
		{method: "Target.detachFromTarget", params: `{"sessionId":"BERTH-SESSION"}`},
		{method: "Page.enable", id: cdppolicy.FirstBerthID},
		{method: "Page.enable", id: -1},
	})
}

// A nested message inside sendMessageToTarget, or a session that is not flattened, would carry
// CDP the filter never sees.
func TestNoUncheckedSessions(t *testing.T) {
	check(t, []row{
		{method: "Target.sendMessageToTarget", params: `{"sessionId":"AGENT","message":"{\"id\":1,\"method\":\"Browser.close\"}"}`},
		{method: "Target.sendMessageToTarget", params: `{"sessionId":"BERTH-SESSION","message":"{}"}`},
		{method: "Target.attachToTarget", params: `{"targetId":"T"}`},
		{method: "Target.attachToTarget", params: `{"targetId":"T","flatten":false}`},
		{method: "Target.attachToTarget", params: `{"targetId":"T","flatten":"true"}`},
		{method: "Target.attachToTarget", params: `{"targetId":"T","flatten":1}`},
		{method: "Target.setAutoAttach", params: `{"autoAttach":true,"waitForDebuggerOnStart":false}`},
		// autoAttachRelated has no flatten parameter, so the filter cannot tell whether the sessions
		// it creates are flat; no driver tested needs it, so it is not listed at all.
		{method: "Target.autoAttachRelated", params: `{"targetId":"T","waitForDebuggerOnStart":false}`},
	})
}

// Chrome runs as the person, so nothing may load, pick or read a local file.
func TestNoLocalFiles(t *testing.T) {
	var rows []row
	for _, u := range []string{
		"file:///Users/you/.ssh/id_ed25519", "FILE:///etc/passwd", " file:///etc/passwd", "\tfile:///etc/passwd",
		"fi\nle:///etc/passwd", "\x00file:///etc/passwd", "filesystem:http://a/temporary/x", "chrome://settings",
		"chrome-extension://abc/x.html", "devtools://devtools/bundled/inspector.html", "view-source:file:///etc/passwd",
		"javascript:alert(1)", "/etc/passwd", "etc/passwd", "about:srcdoc", "1http://a",
	} {
		rows = append(rows,
			row{method: "Page.navigate", params: `{"url":` + quote(u) + `}`},
			row{method: "Target.createTarget", params: `{"url":` + quote(u) + `}`},
			row{method: "Fetch.continueRequest", params: `{"requestId":"r","url":` + quote(u) + `}`},
		)
	}
	for _, u := range []string{"http://a.example", "HTTPS://a.example/x", " https://a.example ", "data:text/html,<b>x</b>", "blob:https://a.example/uuid", "about:blank", "ABOUT:BLANK"} {
		rows = append(rows, row{method: "Page.navigate", params: `{"url":` + quote(u) + `}`, allowed: true})
	}
	rows = append(rows,
		row{method: "Page.navigate"},
		row{method: "Page.navigate", params: `{"url":7}`},
		row{method: "DOM.setFileInputFiles", params: `{"files":["/Users/you/.ssh/id_ed25519"],"nodeId":1}`},
		row{method: "DOM.getFileInfo"},
		row{method: "Input.dispatchDragEvent", params: `{"type":"drop","x":1,"y":1,"data":{"items":[],"files":["/etc/passwd"],"dragOperationsMask":1}}`},
		row{method: "Network.loadNetworkResource", params: `{"url":"file:///etc/passwd","options":{}}`},
		row{method: "Target.createBrowserContext", params: `{"proxyServer":"socks5://127.0.0.1:1"}`},
		row{method: "Input.dispatchKeyEvent", params: `{"type":"keyDown","commands":["paste"]}`},
		row{method: "Input.dispatchKeyEvent", params: `{"type":"keyDown","commands":["selectAll","Copy"]}`},
		// A method of a domain listed method by method that is not on its list is refused.
		row{method: "Page.newMethodChromeAddsLater"},
		row{method: "IO.write"},
		row{method: "Tracing.writeTraceToFile"},
		// A trace's metadata holds the command line, the profile path and machine facts.
		row{method: "Tracing.start", params: `{"traceConfig":{}}`},
		row{method: "Tracing.end"},
		row{method: "Browser.setDockTile", params: `{"image":"AA=="}`},
		// It picks nearby Bluetooth and USB devices with no person in the loop.
		row{method: "DeviceAccess.enable"},
		row{method: "DeviceAccess.selectPrompt", params: `{"id":"p","deviceId":"d"}`},
	)
	check(t, rows)
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteString(`\` + string(r))
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte("0123456789abcdef"[r>>4])
			b.WriteByte("0123456789abcdef"[r&15])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// No domain is both wholesale allowed and listed method by method or denied.
func TestListsDoNotOverlap(t *testing.T) {
	for _, d := range cdppolicy.Allowed {
		if _, ok := cdppolicy.Methods[d]; ok {
			t.Errorf("%s is both allowed wholesale and listed by method", d)
		}
		if _, ok := cdppolicy.Denied[d+".*"]; ok {
			t.Errorf("%s is both allowed and denied", d)
		}
	}
	for name := range cdppolicy.Denied {
		domain, method, _ := strings.Cut(name, ".")
		if slices.Contains(cdppolicy.Methods[domain], method) {
			t.Errorf("%s is both listed and denied", name)
		}
	}
}

// The proxy answers the download behaviour itself; Check still refuses it, so it never reaches
// Chrome, and a Berth session or id is not answered.
func TestDownloadBehaviourIsAnsweredNotForwarded(t *testing.T) {
	for _, method := range []string{"Browser.setDownloadBehavior", "Page.setDownloadBehavior"} {
		m := cdppolicy.Message{ID: 1, Method: method, Params: []byte(`{"behavior":"allowAndName","downloadPath":"/Users/you"}`)}
		if !cdppolicy.Answers(m, ownOnly) || cdppolicy.Check(m, ownOnly) == "" {
			t.Fatalf("%s must be answered by the proxy and refused by Check", method)
		}
		if cdppolicy.Answers(cdppolicy.Message{ID: 1, Method: method, SessionID: "BERTH-SESSION"}, ownOnly) ||
			cdppolicy.Answers(cdppolicy.Message{ID: cdppolicy.FirstBerthID, Method: method}, ownOnly) {
			t.Fatalf("%s on a Berth session or id must not be answered", method)
		}
	}
	if cdppolicy.Answers(cdppolicy.Message{ID: 1, Method: "Browser.close"}, ownOnly) {
		t.Fatal("only the download behaviour is answered")
	}
}
