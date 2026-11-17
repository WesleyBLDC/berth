package e2e

import (
	"cmp"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// findNodeModule returns the newest copy of an npm package already in npx's cache, or the one
// the variable names; the tests install nothing and skip without one.
func findNodeModule(t *testing.T, variable, name string) string {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH")
	}
	if p, ok := os.LookupEnv(variable); ok {
		if st, err := os.Stat(filepath.Join(p, "package.json")); err != nil || st.IsDir() {
			t.Skipf("%s=%s is not a %s folder", variable, p, name)
		}
		return p
	}
	home, _ := os.UserHomeDir()
	matches, _ := filepath.Glob(filepath.Join(home, ".npm/_npx/*/node_modules", name))
	best, bestVersion := "", ""
	for _, m := range matches {
		v := moduleVersion(m)
		if v != "" && (best == "" || compareVersions(v, bestVersion) > 0) {
			best, bestVersion = m, v
		}
	}
	if best == "" {
		t.Skipf("no %s in the npx cache; set %s to one", name, variable)
	}
	return best
}

func moduleVersion(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "package.json")) //nolint:gosec // the npx cache
	if err != nil {
		return ""
	}
	var pkg struct{ Version string }
	_ = json.Unmarshal(b, &pkg)
	return pkg.Version
}

// compareVersions orders x.y.z versions; a prerelease sorts before its release.
func compareVersions(a, b string) int {
	core := func(v string) ([]int, bool) {
		main, _, pre := strings.Cut(v, "-")
		var n []int
		for _, p := range strings.Split(main, ".") {
			i, _ := strconv.Atoi(p)
			n = append(n, i)
		}
		return n, pre
	}
	an, apre := core(a)
	bn, bpre := core(b)
	if c := slices.Compare(an, bn); c != 0 {
		return c
	}
	switch {
	case apre == bpre:
		return 0
	case apre:
		return -1
	}
	return cmp.Compare(1, 0)
}

const playwrightScript = `
const { chromium } = require(process.env.PW_MODULE);
(async () => {
  const browser = await chromium.connectOverCDP(process.env.CDP_ENDPOINT, { headers: { "X-Berth-Token": process.env.CDP_TOKEN } });
  const context = await browser.newContext();
  const page = await context.newPage();
  await page.goto("data:text/html,<title>berth via playwright</title><p>hi</p>");
  console.log("TITLE=" + await page.title());
  await context.close();
  await browser.close();
})().catch(e => { console.error(String(e && e.stack || e)); process.exit(1); });
`

const puppeteerScript = `
const puppeteer = require(process.env.PW_MODULE);
(async () => {
  const res = await fetch(process.env.CDP_ENDPOINT + "/json/version", { headers: { "X-Berth-Token": process.env.CDP_TOKEN } });
  const { webSocketDebuggerUrl } = await res.json();
  const browser = await puppeteer.connect({ browserWSEndpoint: webSocketDebuggerUrl, headers: { "X-Berth-Token": process.env.CDP_TOKEN } });
  const context = await browser.createBrowserContext();
  const page = await context.newPage();
  await page.goto("data:text/html,<title>berth via puppeteer</title><p>hi</p>");
  console.log("TITLE=" + await page.title());
  await context.close();
  await browser.disconnect();
})().catch(e => { console.error(String(e && e.stack || e)); process.exit(1); });
`

// runDriver runs a CDP library under node against a managed browser, with the token in the
// environment, never on a command line.
func runDriver(t *testing.T, module, script, want string, strict bool) {
	t.Helper()
	chrome := findChrome(t)
	h := home(t)
	start(t, h)
	waitServing(t, h)
	l := launchBrowser(t, h, chrome, "driver")
	cmd := exec.CommandContext(t.Context(), "node", "-e", script)
	cmd.Env = append(os.Environ(), "PW_MODULE="+module, "CDP_ENDPOINT="+l.endpoint, "CDP_TOKEN="+l.token)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "TITLE="+want) {
		t.Fatalf("%s through the proxy: %v\n%s", filepath.Base(module), err, out)
	}
	t.Logf("%s %s drove the managed browser through the proxy", filepath.Base(module), moduleVersion(module))
	berth(t, h, "browser", "close", l.id)
	if !strict {
		return
	}
	// Every method the library sent passed or was answered by the proxy: a refusal it tolerated
	// today is an allowlist gap tomorrow.
	for deadline := time.Now().Add(5 * time.Second); ; {
		log := readFile(t, filepath.Join(h, "berthd.log"))
		if i := strings.Index(log, `msg="CDP connection ended"`); i >= 0 {
			line, _, _ := strings.Cut(log[i:], "\n")
			if !strings.Contains(line, " refused=0 ") {
				t.Fatalf("the proxy refused methods %s sent: %s", filepath.Base(module), line)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("berthd logged no end of the CDP connection")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestPlaywrightThroughTheCDPProxy(t *testing.T) {
	runDriver(t, findNodeModule(t, "BERTH_E2E_PLAYWRIGHT_CORE", "playwright-core"), playwrightScript, "berth via playwright", true)
}

func TestPuppeteerThroughTheCDPProxy(t *testing.T) {
	// Puppeteer 24.43 also sends WebMCP.enable, a domain not reviewed for agents, and goes on
	// without it, so only its success is checked.
	runDriver(t, findNodeModule(t, "BERTH_E2E_PUPPETEER_CORE", "puppeteer-core"), puppeteerScript, "berth via puppeteer", false)
}
