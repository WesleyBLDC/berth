// Package cdppolicy decides which CDP messages an agent may send to a browser Berth manages
// (ADR 0009). Chrome runs as the person, so the policy tries to confine whoever holds the token
// to web content in its own browser: the methods of the riskiest domains pass only when listed
// one by one, URLs to load must be web URLs, nothing may read or pick local files, and methods
// that reach Berth's own sessions, the machine or the browser's life are refused. A domain or
// method not reviewed yet is refused, so whatever Chrome adds later fails closed.
package cdppolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
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

// ErrMalformed means the message is not one JSON object with an integer id and a string method,
// or could be read differently by Go and by Chrome; the proxy ends a connection that sends one.
var ErrMalformed = errors.New("a CDP message must be one JSON object with an integer id and a string method")

// readFields are the keys the policy reads, by where they sit. Go's decoder matches keys without
// regard to case and lets the last duplicate win, while Chrome matches exactly, so at these
// levels a key that differs from a known one only by case, or two keys equal but for case, are
// refused rather than guessed at.
var readFields = map[string][]string{
	"":            {"id", "method", "sessionId", "params"},
	"params":      {"url", "flatten", "sessionId", "targetId", "commands", "data", "proxyServer", "proxyBypassList"},
	"params.data": {"files"},
}

// Parse reads an agent message strictly: no key repeats anywhere, and the levels the policy
// reads hold no key that Go and Chrome would match differently.
func Parse(raw []byte) (Message, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		return Message{}, ErrMalformed
	}
	if err := walkObject(dec, ""); err != nil {
		return Message{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Message{}, ErrMalformed
	}
	var m struct {
		ID        json.RawMessage `json:"id"`
		Method    *string         `json:"method"`
		SessionID *string         `json:"sessionId"`
		Params    json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Method == nil || len(m.ID) == 0 {
		return Message{}, ErrMalformed
	}
	id, err := strconv.ParseInt(string(m.ID), 10, 64)
	if err != nil {
		return Message{}, ErrMalformed
	}
	msg := Message{ID: id, Method: *m.Method, Params: m.Params}
	if m.SessionID != nil {
		msg.SessionID = *m.SessionID
	}
	if len(msg.Params) > 0 && msg.Params[0] != '{' && string(msg.Params) != "null" {
		return Message{}, ErrMalformed
	}
	return msg, nil
}

// walkObject reads the rest of an object whose '{' was just read.
func walkObject(dec *json.Decoder, path string) error {
	known := readFields[path]
	var seen []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		if slices.Contains(seen, key) {
			return fmt.Errorf("duplicate key %q", key)
		}
		if known != nil {
			for _, other := range append(slices.Clone(known), seen...) {
				if other != key && strings.EqualFold(other, key) {
					return fmt.Errorf("key %q differs from %q only by case", key, other)
				}
			}
		}
		seen = append(seen, key)
		child := key
		if path != "" {
			child = path + "." + key
		}
		if err := walkValue(dec, child); err != nil {
			return err
		}
	}
	_, err := dec.Token()
	return err
}

func walkValue(dec *json.Decoder, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		return walkObject(dec, path)
	case json.Delim('['):
		for dec.More() {
			if err := walkValue(dec, path+"[]"); err != nil {
				return err
			}
		}
		_, err := dec.Token()
		return err
	}
	return nil
}

// Allowed lists the domains whose every method passes; the riskier domains are in Methods.
var Allowed = []string{
	"Accessibility", "Animation", "Audits", "BackgroundService", "CSS", "CacheStorage", "Console",
	"DOMDebugger", "DOMSnapshot", "DOMStorage", "Database", "Debugger", "DeviceAccess", "DeviceOrientation",
	"Emulation", "EventBreakpoints", "FedCm", "Fetch", "FileSystem", "HeadlessExperimental", "HeapProfiler",
	"IndexedDB", "Inspector", "LayerTree", "Log", "Media", "Memory", "Overlay", "Performance",
	"PerformanceTimeline", "Preload", "Profiler", "Runtime", "Schema", "Security", "ServiceWorker",
	"WebAudio", "WebAuthn",
}

// Methods lists, for the domains that can reach files, other targets, permissions or the
// browser's life, the only methods that pass: what Playwright and Puppeteer use for normal
// automation. Any other method of these domains is refused.
var Methods = map[string][]string{
	"Browser": {"getVersion", "getWindowForTarget", "getWindowBounds", "setWindowBounds", "cancelDownload", "setDockTile"},
	"Target": {
		"activateTarget", "attachToTarget", "autoAttachRelated", "closeTarget", "createBrowserContext", "createTarget",
		"detachFromTarget", "disposeBrowserContext", "getBrowserContexts", "getTargetInfo", "getTargets",
		"setAutoAttach", "setDiscoverTargets",
	},
	"Page": {
		"addScriptToEvaluateOnNewDocument", "bringToFront", "captureScreenshot", "captureSnapshot", "close",
		"createIsolatedWorld", "crash", "disable", "enable", "getAppManifest", "getFrameTree", "getLayoutMetrics",
		"getNavigationHistory", "getResourceContent", "getResourceTree", "handleJavaScriptDialog", "navigate",
		"navigateToHistoryEntry", "printToPDF", "reload", "removeScriptToEvaluateOnNewDocument",
		"resetNavigationHistory", "screencastFrameAck", "setBypassCSP", "setDocumentContent",
		"setFontFamilies", "setFontSizes", "setInterceptFileChooserDialog", "setLifecycleEventsEnabled",
		"setWebLifecycleState", "startScreencast", "stopLoading", "stopScreencast",
	},
	"DOM": {
		"collectClassNamesFromSubtree", "copyTo", "describeNode", "disable", "discardSearchResults", "enable", "focus",
		"getAttributes", "getBoxModel", "getContentQuads", "getDocument", "getFrameOwner", "getNodeForLocation",
		"getOuterHTML", "getSearchResults", "hideHighlight", "highlightNode", "markUndoableState", "moveTo",
		"performSearch", "pushNodesByBackendIdsToFrontend", "querySelector", "querySelectorAll", "redo",
		"removeAttribute", "removeNode", "requestChildNodes", "requestNode", "resolveNode",
		"scrollIntoViewIfNeeded", "setAttributeValue", "setAttributesAsText", "setInspectedNode", "setNodeName",
		"setNodeValue", "setOuterHTML", "undo",
	},
	"Input": {
		"cancelDragging", "dispatchDragEvent", "dispatchKeyEvent", "dispatchMouseEvent", "dispatchTouchEvent",
		"emulateTouchFromMouseEvent", "imeSetComposition", "insertText", "setIgnoreInputEvents", "setInterceptDrags",
		"synthesizePinchGesture", "synthesizeScrollGesture", "synthesizeTapGesture",
	},
	"Network": {
		"clearAcceptedEncodingsOverride", "clearBrowserCache", "clearBrowserCookies", "deleteCookies", "disable",
		"emulateNetworkConditions", "enable", "getAllCookies", "getCookies", "getRequestPostData", "getResponseBody",
		"getSecurityIsolationStatus", "replayXHR", "searchInResponseBody", "setAcceptedEncodings",
		"setAttachDebugStack", "setBlockedURLs", "setBypassServiceWorker", "setCacheDisabled", "setCookie",
		"setCookies", "setExtraHTTPHeaders", "setUserAgentOverride",
	},
	"Storage": {
		"clearCookies", "clearDataForOrigin", "clearDataForStorageKey", "getCookies", "getStorageKeyForFrame",
		"getUsageAndQuota", "setCookies", "trackCacheStorageForOrigin", "trackIndexedDBForOrigin",
		"untrackCacheStorageForOrigin", "untrackIndexedDBForOrigin",
	},
	// IO only reads the streams Chrome itself made (a PDF or a trace), never a path.
	"IO":      {"close", "read", "resolveBlob"},
	"Tracing": {"end", "getCategories", "recordClockSyncMarker", "requestMemoryDump", "start"},
}

// Denied names methods, and whole domains (as "Domain.*"), refused whatever their parameters,
// with the reason the agent is told. Methods of the domains in Methods that are not listed there
// are refused too; the ones named here are the reviewed refusals.
var Denied = map[string]string{
	"Browser.close":                 "the browser's life belongs to Berth",
	"Browser.crash":                 "the browser's life belongs to Berth",
	"Browser.crashGpuProcess":       "the browser's life belongs to Berth",
	"Browser.getBrowserCommandLine": "it shows Berth's launch flags and profile path",
	"Browser.executeBrowserCommand": "it runs browser commands outside any page",
	"Browser.setDownloadBehavior":   "downloads go only to the profile's own folder",
	"Page.setDownloadBehavior":      "downloads go only to the profile's own folder",
	"Browser.grantPermissions":      "permissions such as the clipboard stay at their defaults",
	"Browser.setPermission":         "permissions such as the clipboard stay at their defaults",
	"Browser.resetPermissions":      "permissions such as the clipboard stay at their defaults",
	"Target.exposeDevToolsProtocol": "it hands CDP to page JavaScript",
	"Target.setRemoteLocations":     "it makes the browser connect out to other targets",
	"Target.sendMessageToTarget":    "nested messages would pass unchecked; use flattened sessions",
	"Target.attachToBrowserTarget":  "a browser session is Berth's",
	"DOM.setFileInputFiles":         "it hands local files to a page",
	"DOM.getFileInfo":               "it shows local file paths",
	"Network.loadNetworkResource":   "it loads any URL, local files included",
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
	domain, name, ok := strings.Cut(m.Method, ".")
	if !ok || domain == "" || name == "" {
		return "not a CDP method"
	}
	if why, denied := Denied[m.Method]; denied {
		return why
	}
	if why, denied := Denied[domain+".*"]; denied {
		return why
	}
	if listed, ok := Methods[domain]; ok {
		if !slices.Contains(listed, name) {
			return "the method is not reviewed for agents"
		}
	} else if !slices.Contains(Allowed, domain) {
		return "the domain is not reviewed for agents"
	}
	return checkParams(m, own)
}

type params struct {
	URL             *string         `json:"url"`
	Flatten         json.RawMessage `json:"flatten"`
	SessionID       string          `json:"sessionId"`
	Commands        []string        `json:"commands"`
	ProxyServer     json.RawMessage `json:"proxyServer"`
	ProxyBypassList json.RawMessage `json:"proxyBypassList"`
	Data            *struct {
		Files []json.RawMessage `json:"files"`
	} `json:"data"`
}

const webOnly = "only http, https, data, blob and about:blank URLs load"

func checkParams(m Message, own func(string) bool) string {
	var p params
	if len(m.Params) > 0 && json.Unmarshal(m.Params, &p) != nil {
		return "unreadable parameters"
	}
	url := ""
	if p.URL != nil {
		url = *p.URL
	}
	switch m.Method {
	case "Page.navigate":
		if !Loadable(url) {
			return webOnly
		}
	case "Target.createTarget":
		if url != "" && !Loadable(url) {
			return webOnly
		}
	case "Fetch.continueRequest":
		if p.URL != nil && !Loadable(url) {
			return webOnly
		}
	case "Target.attachToTarget", "Target.setAutoAttach", "Target.autoAttachRelated":
		if string(p.Flatten) != "true" {
			return "only flattened sessions, so every message passes the filter"
		}
	case "Target.detachFromTarget":
		if own(p.SessionID) {
			return "the session is Berth's"
		}
	case "Target.createBrowserContext":
		if len(p.ProxyServer) > 0 || len(p.ProxyBypassList) > 0 {
			return "a context's proxy stays the browser's"
		}
	case "Input.dispatchKeyEvent":
		for _, c := range p.Commands {
			lc := strings.ToLower(c)
			if strings.Contains(lc, "paste") || strings.Contains(lc, "copy") || strings.Contains(lc, "cut") || strings.Contains(lc, "clipboard") {
				return "editing commands may not reach the clipboard"
			}
		}
	case "Input.dispatchDragEvent":
		if p.Data != nil && len(p.Data.Files) > 0 {
			return "it hands local files to a page"
		}
	}
	return ""
}

// Loadable reports whether a URL is web content: http, https, data, blob or about:blank. It
// strips what Chrome's URL parser strips first (C0 controls and spaces at either end, tabs and
// newlines anywhere), so a file URL cannot hide behind them.
func Loadable(u string) bool {
	u = strings.TrimFunc(u, func(r rune) bool { return r <= ' ' })
	u = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(u)
	lower := strings.ToLower(u)
	if lower == "about:blank" {
		return true
	}
	scheme, _, ok := strings.Cut(lower, ":")
	if !ok || scheme == "" || scheme[0] < 'a' || scheme[0] > 'z' {
		return false
	}
	for _, c := range scheme {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return slices.Contains([]string{"http", "https", "data", "blob"}, scheme)
}
