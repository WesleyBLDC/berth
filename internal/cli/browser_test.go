package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/client"
)

const cliToken = "cli-cdp-token-canary-91aa"

type fakeBrowsers struct {
	berthv1connect.UnimplementedBrowsersServiceHandler
	launched []*berthv1.LaunchBrowserRequest
	closed   []string
}

func (f *fakeBrowsers) LaunchBrowser(_ context.Context, req *berthv1.LaunchBrowserRequest) (*berthv1.LaunchBrowserResponse, error) {
	f.launched = append(f.launched, req)
	return &berthv1.LaunchBrowserResponse{
		Browser: &berthv1.ManagedBrowser{
			Id: "0123456789abcdef", Profile: req.GetProfile(), State: berthv1.BrowserState_BROWSER_STATE_READY,
			CdpEndpoint: "http://127.0.0.1:4321",
		},
		CdpToken: cliToken,
	}, nil
}

func (f *fakeBrowsers) ListBrowsers(context.Context, *berthv1.ListBrowsersRequest) (*berthv1.ListBrowsersResponse, error) {
	return &berthv1.ListBrowsersResponse{Browsers: []*berthv1.ManagedBrowser{
		{Id: "0123456789abcdef", Profile: "work", State: berthv1.BrowserState_BROWSER_STATE_READY, SessionId: "5001-1800000000000000", Headless: true, CdpEndpoint: "http://127.0.0.1:4321", Product: "HeadlessChrome/156.0"},
		{Id: "fedcba9876543210", Profile: "old", State: berthv1.BrowserState_BROWSER_STATE_CLOSED, CloseReason: "launch failed: the browser did not start"},
	}}, nil
}

func (f *fakeBrowsers) CloseBrowser(_ context.Context, req *berthv1.CloseBrowserRequest) (*berthv1.CloseBrowserResponse, error) {
	f.closed = append(f.closed, req.GetId())
	return &berthv1.CloseBrowserResponse{Browser: &berthv1.ManagedBrowser{Id: req.GetId(), State: berthv1.BrowserState_BROWSER_STATE_CLOSED}}, nil
}

func invokeBrowsers(t *testing.T, fake *fakeBrowsers, args ...string) result {
	t.Helper()
	server := connect.NewServer()
	berthv1connect.RegisterBrowsersServiceHandler(server, fake)
	cl := &client.Client{Browsers: berthv1connect.NewBrowsersServiceClient(connect.NewClient(connectinprocess.New(server)))}
	var out, errOut bytes.Buffer
	code := Run(t.Context(), args, Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut,
		Connect: func(context.Context) (*client.Client, error) { return cl, nil },
	})
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func TestBrowserLaunchPrintsEndpointAndToken(t *testing.T) {
	fake := &fakeBrowsers{}
	r := invokeBrowsers(t, fake, "browser", "launch", "work", "--headless")
	want := "browser 0123456789abcdef on profile work is ready\nendpoint http://127.0.0.1:4321\ntoken " + cliToken + "\n"
	if r.code != ExitOK || r.stdout != want || !strings.Contains(r.stderr, "X-Berth-Token") {
		t.Fatalf("launch: %d %q %q", r.code, r.stdout, r.stderr)
	}
	if len(fake.launched) != 1 || !fake.launched[0].GetHeadless() || fake.launched[0].GetProfile() != "work" {
		t.Fatalf("request = %v", fake.launched)
	}
}

// With --token-file the token goes only into a new file only the person can read.
func TestBrowserLaunchTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	r := invokeBrowsers(t, &fakeBrowsers{}, "browser", "launch", "--token-file", path, "work")
	if r.code != ExitOK || strings.Contains(r.stdout+r.stderr, cliToken) {
		t.Fatalf("launch: %d %q %q", r.code, r.stdout, r.stderr)
	}
	st, err := os.Stat(path)
	b, _ := os.ReadFile(path)
	if err != nil || st.Mode().Perm() != 0o600 || string(b) != cliToken+"\n" {
		t.Fatalf("token file: %v %v %q", st, err, b)
	}
}

func TestBrowserLsAndClose(t *testing.T) {
	fake := &fakeBrowsers{}
	r := invokeBrowsers(t, fake, "browser", "ls", "--all")
	want := `ID                PROFILE  STATE                                              SESSION                HEADLESS  ENDPOINT               PRODUCT
0123456789abcdef  work     ready                                              5001-1800000000000000  yes       http://127.0.0.1:4321  HeadlessChrome/156.0
fedcba9876543210  old      closed (launch failed: the browser did not start)  -                      no        -                      -
`
	if r.code != ExitOK || r.stdout != want {
		t.Fatalf("ls:\n%s\nwant:\n%s", r.stdout, want)
	}
	r = invokeBrowsers(t, fake, "browser", "close", "0123456789abcdef")
	if r.code != ExitOK || r.stdout != "browser 0123456789abcdef is closed\n" || len(fake.closed) != 1 {
		t.Fatalf("close: %d %q", r.code, r.stdout)
	}
	for _, args := range [][]string{{"browser"}, {"browser", "close", "nope"}, {"browser", "launch", "Bad Name"}, {"browser", "launch"}} {
		if r := invokeBrowsers(t, fake, args...); r.code != ExitUsage {
			t.Fatalf("%v: code %d", args, r.code)
		}
	}
}

// The token file is created new: an existing file or a symlink is refused, not written through.
func TestBrowserLaunchTokenFileIsNeverReused(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing")
	if err := os.WriteFile(existing, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{existing, link} {
		r := invokeBrowsers(t, &fakeBrowsers{}, "browser", "launch", "--token-file", path, "work")
		if r.code != ExitError || strings.Contains(r.stdout+r.stderr, cliToken) {
			t.Fatalf("%s: %d %q %q", path, r.code, r.stdout, r.stderr)
		}
	}
	if b, _ := os.ReadFile(existing); string(b) != "keep" {
		t.Fatal("an existing file was overwritten")
	}
	if _, err := os.Lstat(filepath.Join(dir, "elsewhere")); err == nil {
		t.Fatal("the token was written through a symlink")
	}
}
