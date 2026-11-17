package browsersig_test

import (
	"testing"

	"berth/internal/core/browsersig"
)

// Paths and arguments copied from processes on a Mac running macOS 26.5.
const (
	chrome       = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	chromeHelper = "/Applications/Google Chrome.app/Contents/Frameworks/Google Chrome Framework.framework/Versions/157.0.7710.0/Helpers/Google Chrome Helper (Renderer).app/Contents/MacOS/Google Chrome Helper (Renderer)"
	chromeCrash  = "/Applications/Google Chrome.app/Contents/Frameworks/Google Chrome Framework.framework/Versions/157.0.7710.0/Helpers/chrome_crashpad_handler"
	updater      = "/Applications/Google Chrome.app/Contents/Frameworks/Google Chrome Framework.framework/Versions/157.0.7710.0/Helpers/GoogleUpdater.app/Contents/MacOS/GoogleUpdater"
	pwShell      = "/Users/u/Library/Caches/ms-playwright/chromium_headless_shell-1248/chrome-headless-shell-mac-arm64/chrome-headless-shell"
	pwCFT        = "/Users/u/Library/Caches/ms-playwright/chromium-1248/chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"
	ppCFT        = "/Users/u/.cache/puppeteer/chrome/mac_arm-154.0.8037.57/chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"
	slackHelper  = "/Applications/Slack.app/Contents/Frameworks/Slack Helper.app/Contents/MacOS/Slack Helper"
	slackCrash   = "/Applications/Slack.app/Contents/Frameworks/Electron Framework.framework/Helpers/chrome_crashpad_handler"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name  string
		exe   string
		args  []string
		known bool
		want  browsersig.Signature
		ok    bool
	}{
		{
			"person's Chrome", chrome,
			[]string{chrome},
			true,
			browsersig.Signature{Kind: browsersig.Chrome, Role: browsersig.RoleBrowser, ArgsKnown: true, Driver: browsersig.DriverNone, CDP: browsersig.CDPNone},
			true,
		},
		{
			"Chrome renderer", chromeHelper,
			[]string{chromeHelper, "--type=renderer"},
			true,
			browsersig.Signature{Kind: browsersig.Chrome, Role: browsersig.RoleHelper, ArgsKnown: true, Driver: browsersig.DriverNone, CDP: browsersig.CDPNone},
			true,
		},
		{
			"Chrome crash handler without arguments", chromeCrash, nil, false,
			browsersig.Signature{Kind: browsersig.Chrome, Role: browsersig.RoleHelper},
			true,
		},
		{
			"Playwright headless shell", pwShell,
			[]string{pwShell, "--disable-field-trial-config", "--enable-automation", "--headless", "--user-data-dir=/var/folders/x/T/playwright_chromiumdev_profile-AbC123", "--remote-debugging-pipe", "about:blank"},
			true,
			browsersig.Signature{Kind: browsersig.HeadlessShell, Role: browsersig.RoleBrowser, ArgsKnown: true, Driver: browsersig.Playwright, Headless: true, CDP: browsersig.CDPPipe, UserDataDir: "/var/folders/x/T/playwright_chromiumdev_profile-AbC123"},
			true,
		},
		{
			"headless shell renderer", pwShell,
			[]string{pwShell, "--type=renderer", "--headless=old", "--remote-debugging-port=0"},
			true,
			browsersig.Signature{Kind: browsersig.HeadlessShell, Role: browsersig.RoleHelper, ArgsKnown: true, Driver: browsersig.Playwright, Headless: true, CDP: browsersig.CDPPort},
			true,
		},
		{
			"Playwright MCP Chrome for Testing", pwCFT,
			[]string{pwCFT, "--remote-debugging-port=9222", "--remote-debugging-pipe", "--user-data-dir=/Users/u/Library/Caches/ms-playwright/mcp-chrome-aa1b278"},
			true,
			browsersig.Signature{Kind: browsersig.ChromeForTesting, Role: browsersig.RoleBrowser, ArgsKnown: true, Driver: browsersig.Playwright, CDP: browsersig.CDPPort, CDPPort: 9222, UserDataDir: "/Users/u/Library/Caches/ms-playwright/mcp-chrome-aa1b278"},
			true,
		},
		{
			"Puppeteer Chrome for Testing", ppCFT,
			[]string{ppCFT, "--headless=new", "-remote-debugging-port=0"},
			true,
			browsersig.Signature{Kind: browsersig.ChromeForTesting, Role: browsersig.RoleBrowser, ArgsKnown: true, Driver: browsersig.Puppeteer, Headless: true, CDP: browsersig.CDPPort},
			true,
		},
		{
			"chrome-devtools-mcp drives the person's Chrome", chrome,
			[]string{chrome, "--user-data-dir=/Users/u/.cache/chrome-devtools-mcp/chrome-profile-stable", "--remote-debugging-pipe"},
			true,
			browsersig.Signature{Kind: browsersig.Chrome, Role: browsersig.RoleBrowser, ArgsKnown: true, Driver: browsersig.Puppeteer, CDP: browsersig.CDPPipe, UserDataDir: "/Users/u/.cache/chrome-devtools-mcp/chrome-profile-stable"},
			true,
		},
		{
			"automation from an unknown library", chrome,
			[]string{chrome, "--enable-automation", "--remote-debugging-port=notaport"},
			true,
			browsersig.Signature{Kind: browsersig.Chrome, Role: browsersig.RoleBrowser, ArgsKnown: true, Driver: browsersig.Automation, CDP: browsersig.CDPNone},
			true,
		},
		{
			"another user's Playwright shell", pwShell, nil, false,
			browsersig.Signature{Kind: browsersig.HeadlessShell, Role: browsersig.RoleBrowser, Driver: browsersig.Playwright, Headless: true},
			true,
		},
		{
			"Edge", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge", nil, true,
			browsersig.Signature{Kind: browsersig.Edge, Role: browsersig.RoleBrowser, ArgsKnown: true, Driver: browsersig.DriverNone, CDP: browsersig.CDPNone},
			true,
		},
		{
			"Brave", "/Applications/Brave Browser.app/Contents/MacOS/Brave Browser", nil, true,
			browsersig.Signature{Kind: browsersig.Brave, Role: browsersig.RoleBrowser, ArgsKnown: true, Driver: browsersig.DriverNone, CDP: browsersig.CDPNone},
			true,
		},
		{
			"Chromium", "/Applications/Chromium.app/Contents/MacOS/Chromium", nil, true,
			browsersig.Signature{Kind: browsersig.Chromium, Role: browsersig.RoleBrowser, ArgsKnown: true, Driver: browsersig.DriverNone, CDP: browsersig.CDPNone},
			true,
		},
		{"Chrome's updater is not a browser", updater, nil, true, browsersig.Signature{}, false},
		{"Electron helper", slackHelper, []string{slackHelper, "--type=gpu-process"}, true, browsersig.Signature{}, false},
		{"Electron crash handler", slackCrash, nil, true, browsersig.Signature{}, false},
		{"Arc is not in the registry", "/Applications/Arc.app/Contents/MacOS/Arc", nil, true, browsersig.Signature{}, false},
		{"a copy named Google Chrome elsewhere", "/tmp/Google Chrome", nil, true, browsersig.Signature{}, false},
	}
	for _, c := range cases {
		got, ok := browsersig.Classify(c.exe, c.args, c.known)
		if ok != c.ok || got != c.want {
			t.Errorf("%s:\n got %+v %v\nwant %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}
