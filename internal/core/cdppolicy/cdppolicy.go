// Package cdppolicy decides which CDP messages an agent may send to a browser Berth manages
// (ADR 0009): everything inside its own session and its own browser passes; methods that reach
// Berth's own sessions, other users of the browser, the machine or files outside the profile are
// refused; and domains not reviewed yet are refused, so a new Chrome domain fails closed.
package cdppolicy

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// FirstBerthID is where Berth's own ids on the pipe start; agent ids must stay below it.
const FirstBerthID = 1_000_000_000

// Message is the part of an agent's CDP message the policy reads.
type Message struct {
	ID        int64
	Method    string
	SessionID string
	Params    json.RawMessage
}

// ErrMalformed means the message is not a JSON object with an integer id and a string method; the
// proxy ends a connection that sends one.
var ErrMalformed = errors.New("a CDP message must be a JSON object with an integer id and a string method")

// Parse reads an agent message.
func Parse(raw []byte) (Message, error) {
	var m struct {
		ID        json.RawMessage `json:"id"`
		Method    *string         `json:"method"`
		SessionID string          `json:"sessionId"`
		Params    json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Method == nil || len(m.ID) == 0 {
		return Message{}, ErrMalformed
	}
	id, err := strconv.ParseInt(string(m.ID), 10, 64)
	if err != nil {
		return Message{}, ErrMalformed
	}
	return Message{ID: id, Method: *m.Method, SessionID: m.SessionID, Params: m.Params}, nil
}

// Allowed lists the domains whose methods pass unless Denied names them.
var Allowed = []string{
	"Accessibility", "Animation", "Audits", "BackgroundService", "Browser", "CSS", "CacheStorage", "Console",
	"DOM", "DOMDebugger", "DOMSnapshot", "DOMStorage", "Database", "Debugger", "DeviceAccess", "DeviceOrientation",
	"Emulation", "EventBreakpoints", "FedCm", "Fetch", "FileSystem", "HeadlessExperimental", "HeapProfiler", "IO",
	"IndexedDB", "Input", "Inspector", "LayerTree", "Log", "Media", "Memory", "Network", "Overlay", "Page",
	"Performance", "PerformanceTimeline", "Preload", "Profiler", "Runtime", "Schema", "Security", "ServiceWorker",
	"Storage", "Target", "Tracing", "WebAudio", "WebAuthn",
}

// Denied names methods, and whole domains (as "Domain.*"), refused whatever their parameters.
var Denied = map[string]string{
	"Browser.close":                 "the browser's life belongs to Berth",
	"Browser.crash":                 "the browser's life belongs to Berth",
	"Browser.crashGpuProcess":       "the browser's life belongs to Berth",
	"Browser.getBrowserCommandLine": "it shows Berth's launch flags and profile path",
	"Browser.executeBrowserCommand": "it runs browser commands outside any page",
	"Target.exposeDevToolsProtocol": "it hands CDP to page JavaScript",
	"Target.setRemoteLocations":     "it makes the browser connect out to other targets",
	"SystemInfo.*":                  "it reads facts about the machine",
	"Tethering.*":                   "it binds ports on the Mac",
	"Extensions.*":                  "it loads code from disk into the browser",
	"PWA.*":                         "it installs and launches apps outside the browser",
	"Cast.*":                        "it talks to devices on the network",
	"Autofill.*":                    "autofill belongs to Berth",
}

// Check decides one message. own reports whether a CDP session id is one Berth attached to
// itself. It returns "" when the message may pass, otherwise why it is refused.
func Check(m Message, own func(sessionID string) bool) string {
	if m.ID < 0 || m.ID >= FirstBerthID {
		return "ids from 1000000000 are Berth's"
	}
	if own(m.SessionID) {
		return "the session is Berth's"
	}
	domain, _, ok := strings.Cut(m.Method, ".")
	if !ok || domain == "" {
		return "not a CDP method"
	}
	if why, denied := Denied[m.Method]; denied {
		return why
	}
	if why, denied := Denied[domain+".*"]; denied {
		return why
	}
	if !allowedDomain(domain) {
		return "the domain is not reviewed for agents"
	}
	switch m.Method {
	case "Browser.setDownloadBehavior", "Page.setDownloadBehavior":
		var p struct {
			Behavior string `json:"behavior"`
		}
		if json.Unmarshal(m.Params, &p) != nil || (p.Behavior != "deny" && p.Behavior != "default") {
			return "downloads may only be denied or left at the default"
		}
	case "Target.detachFromTarget", "Target.sendMessageToTarget":
		var p struct {
			SessionID string `json:"sessionId"`
		}
		if len(m.Params) > 0 && json.Unmarshal(m.Params, &p) != nil {
			return "unreadable parameters"
		}
		if own(p.SessionID) {
			return "the session is Berth's"
		}
	}
	return ""
}

func allowedDomain(domain string) bool {
	for _, d := range Allowed {
		if d == domain {
			return true
		}
	}
	return false
}
