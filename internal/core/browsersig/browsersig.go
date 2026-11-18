// Package browsersig recognises Chromium family browsers from an executable path and, when the
// kernel gives them, the process's arguments. It is a pure table of rules: a renamed or copied
// browser is missed, and anything at a matching path is listed, which costs a wrong line in
// berth ps, never a grant.
package browsersig

import (
	"path"
	"strconv"
	"strings"
)

type Kind string

const (
	Chrome           Kind = "Chrome"
	ChromeForTesting Kind = "ChromeForTesting"
	Chromium         Kind = "Chromium"
	Edge             Kind = "Edge"
	Brave            Kind = "Brave"
	HeadlessShell    Kind = "HeadlessShell"
)

type Role string

const (
	// RoleBrowser is a browser's main process, which can root a session.
	RoleBrowser Role = "Browser"
	// RoleHelper is a renderer, GPU, network or crash process, which never roots a session.
	RoleHelper Role = "Helper"
)

// Driver is the automation library a browser points to; DriverUnknown when the arguments were
// unreadable and the path said nothing.
type Driver string

const (
	DriverUnknown Driver = ""
	DriverNone    Driver = "None"
	Playwright    Driver = "Playwright"
	Puppeteer     Driver = "Puppeteer"
	// Automation is --enable-automation with no sign of which library set it.
	Automation Driver = "Automation"
)

type CDP string

const (
	CDPNone CDP = "None"
	CDPPipe CDP = "Pipe"
	CDPPort CDP = "Port"
)

// Signature is everything the rules learned about one process.
type Signature struct {
	Kind Kind
	Role Role
	// ArgsKnown is false for a process whose arguments the kernel refused (another user's), so
	// Headless, CDP and UserDataDir are unknown and Driver comes from the path alone.
	ArgsKnown   bool
	Driver      Driver
	Headless    bool
	CDP         CDP
	CDPPort     int
	UserDataDir string
}

// bundle is one browser app: its .app folder name and the executable inside Contents/MacOS.
type bundle struct {
	kind Kind
	app  string
	exe  string
}

var bundles = []bundle{
	{Chrome, "Google Chrome", "Google Chrome"},
	{Chrome, "Google Chrome Beta", "Google Chrome Beta"},
	{Chrome, "Google Chrome Dev", "Google Chrome Dev"},
	{Chrome, "Google Chrome Canary", "Google Chrome Canary"},
	{ChromeForTesting, "Google Chrome for Testing", "Google Chrome for Testing"},
	{Chromium, "Chromium", "Chromium"},
	{Edge, "Microsoft Edge", "Microsoft Edge"},
	{Brave, "Brave Browser", "Brave Browser"},
}

var headlessShells = []string{"chrome-headless-shell", "headless_shell"}

// crashHandler sits in every Chromium bundle and reparents itself to launchd, so it must never
// look like a browser of its own.
const crashHandler = "chrome_crashpad_handler"

// Classify reports whether exe is a browser process and what it is. args are used only when
// argsKnown; raw arguments never leave this function, only the facts parsed from them.
func Classify(exe string, args []string, argsKnown bool) (Signature, bool) {
	kind, role, ok := byPath(exe)
	if !ok {
		return Signature{}, false
	}
	sig := Signature{Kind: kind, Role: role, ArgsKnown: argsKnown, Headless: kind == HeadlessShell}
	sig.Driver = driverByPath(exe)
	if !argsKnown {
		return sig, true
	}
	sig.CDP = CDPNone
	automation := false
	for _, a := range args {
		name, value, hasValue := switchOf(a)
		switch name {
		case "type":
			if hasValue {
				sig.Role = RoleHelper
			}
		case "headless":
			sig.Headless = true
		case "remote-debugging-pipe":
			if sig.CDP != CDPPort {
				sig.CDP = CDPPipe
			}
		case "remote-debugging-port":
			if port, err := strconv.Atoi(value); err == nil && port >= 0 && port <= 65535 {
				// A port wins over a pipe, since any local process can reach it.
				sig.CDP, sig.CDPPort = CDPPort, port
			}
		case "user-data-dir":
			sig.UserDataDir = value
		case "enable-automation":
			automation = true
		}
	}
	if sig.Driver == DriverUnknown {
		sig.Driver = driverByProfile(sig.UserDataDir)
	}
	if sig.Driver == DriverUnknown && automation {
		sig.Driver = Automation
	}
	if sig.Driver == DriverUnknown {
		sig.Driver = DriverNone
	}
	return sig, true
}

func byPath(exe string) (Kind, Role, bool) {
	base := path.Base(exe)
	for _, s := range headlessShells {
		if base == s {
			return HeadlessShell, RoleBrowser, true
		}
	}
	for _, b := range bundles {
		contents := "/" + b.app + ".app/Contents/"
		i := strings.LastIndex(exe, contents)
		if i < 0 {
			continue
		}
		if exe[i+len(contents):] == "MacOS/"+b.exe {
			return b.kind, RoleBrowser, true
		}
		if strings.HasPrefix(base, b.exe+" Helper") || base == crashHandler {
			return b.kind, RoleHelper, true
		}
	}
	return "", "", false
}

func driverByPath(exe string) Driver {
	switch {
	case strings.Contains(exe, "/ms-playwright/"):
		return Playwright
	case strings.Contains(exe, "/.cache/puppeteer/"):
		return Puppeteer
	}
	return DriverUnknown
}

func driverByProfile(dir string) Driver {
	base := path.Base(dir)
	switch {
	case dir == "":
		return DriverUnknown
	case strings.HasPrefix(base, "playwright_chromiumdev_profile-"), strings.Contains(dir, "/ms-playwright/mcp-"):
		return Playwright
	case strings.HasPrefix(base, "puppeteer_dev_chrome_profile-"), strings.Contains(dir, "/chrome-devtools-mcp/"):
		return Puppeteer
	}
	return DriverUnknown
}

// switchOf splits a Chromium switch, which may start with one dash or two.
func switchOf(arg string) (name, value string, hasValue bool) {
	if !strings.HasPrefix(arg, "-") {
		return "", "", false
	}
	arg = strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	name, value, hasValue = strings.Cut(arg, "=")
	return name, value, hasValue
}
