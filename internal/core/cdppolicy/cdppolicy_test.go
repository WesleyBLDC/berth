package cdppolicy_test

import (
	"errors"
	"strings"
	"testing"

	"berth/internal/core/cdppolicy"
)

func ownOnly(id string) bool { return id == "BERTH-SESSION" }

func TestParse(t *testing.T) {
	m, err := cdppolicy.Parse([]byte(`{"id":7,"method":"Page.navigate","sessionId":"S","params":{"url":"data:,"}}`))
	if err != nil || m.ID != 7 || m.Method != "Page.navigate" || m.SessionID != "S" || string(m.Params) != `{"url":"data:,"}` {
		t.Fatalf("parse = %+v %v", m, err)
	}
	for _, raw := range []string{`[]`, `{"method":"Page.enable"}`, `{"id":1}`, `{"id":1.5,"method":"Page.enable"}`, `{"id":"1","method":"Page.enable"}`, `{"id":1,"method":7}`, `not json`} {
		if _, err := cdppolicy.Parse([]byte(raw)); !errors.Is(err, cdppolicy.ErrMalformed) {
			t.Fatalf("parse %s = %v, want ErrMalformed", raw, err)
		}
	}
}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		method, session, params string
		id                      int64
		allowed                 bool
	}{
		{method: "Page.navigate", allowed: true},
		{method: "Runtime.evaluate", session: "AGENT", allowed: true},
		{method: "Target.createTarget", allowed: true},
		{method: "Target.attachToTarget", params: `{"targetId":"T","flatten":true}`, allowed: true},
		{method: "Target.setAutoAttach", allowed: true},
		{method: "Browser.getVersion", allowed: true},
		{method: "Network.getCookies", allowed: true},
		{method: "Storage.getCookies", allowed: true},
		{method: "Input.dispatchMouseEvent", allowed: true},
		{method: "Browser.setDownloadBehavior", params: `{"behavior":"deny"}`, allowed: true},
		{method: "Page.setDownloadBehavior", params: `{"behavior":"default"}`, allowed: true},
		{method: "Target.detachFromTarget", params: `{"sessionId":"AGENT"}`, allowed: true},

		{method: "Browser.setDownloadBehavior", params: `{"behavior":"allow","downloadPath":"/Users/you"}`},
		{method: "Page.setDownloadBehavior", params: `{"behavior":"allowAndName","downloadPath":"/tmp"}`},
		{method: "Browser.setDownloadBehavior"},
		{method: "Browser.close"},
		{method: "Browser.crash"},
		{method: "Browser.crashGpuProcess"},
		{method: "Browser.getBrowserCommandLine"},
		{method: "Browser.executeBrowserCommand"},
		{method: "SystemInfo.getInfo"},
		{method: "SystemInfo.getProcessInfo"},
		{method: "Target.exposeDevToolsProtocol"},
		{method: "Target.setRemoteLocations"},
		{method: "Tethering.bind"},
		{method: "Extensions.loadUnpacked"},
		{method: "PWA.install"},
		{method: "Cast.startTabMirroring"},
		{method: "Autofill.trigger"},
		{method: "NewDomain.doSomething"},
		{method: "nodot"},
		{method: ".enable"},
		{method: "Page.enable", session: "BERTH-SESSION"},
		{method: "Target.detachFromTarget", params: `{"sessionId":"BERTH-SESSION"}`},
		{method: "Target.sendMessageToTarget", params: `{"sessionId":"BERTH-SESSION","message":"{}"}`},
		{method: "Page.enable", id: cdppolicy.FirstBerthID},
		{method: "Page.enable", id: -1},
	} {
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

// Every denied domain is outside the allowed list, so a denial never hides behind an allowance.
func TestDeniedDomainsAreNotAllowed(t *testing.T) {
	for name := range cdppolicy.Denied {
		domain, rest, _ := strings.Cut(name, ".")
		if rest != "*" {
			continue
		}
		for _, a := range cdppolicy.Allowed {
			if a == domain {
				t.Fatalf("%s is both denied and allowed", domain)
			}
		}
	}
}
