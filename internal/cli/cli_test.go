package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/client"
)

func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}

type fakeSecrets struct {
	berthv1connect.UnimplementedSecretsServiceHandler
	store     berthv1.StoreOutcome
	storeErr  error
	reasons   []string
	list      *berthv1.ListSecretsResponse
	use       berthv1.UseOutcome
	useArgv   []string
	useExe    string
	useErr    error
	passEnv   []string
	authorize int
	target    *berthv1.ProxyTarget
	grant     berthv1.GrantOutcome
	revoked   berthv1.GrantState
	remove    *berthv1.RemoveSecretResponse
	rotate    berthv1.StoreOutcome
	lifeErr   error
}

func (f *fakeSecrets) GrantProxy(_ context.Context, req *berthv1.GrantProxyRequest) (*berthv1.GrantProxyResponse, error) {
	f.target = req.GetTarget()
	if f.lifeErr != nil {
		return nil, f.lifeErr
	}
	return &berthv1.GrantProxyResponse{Outcome: f.grant, Consumer: &berthv1.Consumer{
		GrantId: 9, Kind: berthv1.ConsumerKind_CONSUMER_KIND_PROXY, Proxy: req.GetTarget(),
	}}, nil
}

func (f *fakeSecrets) RevokeGrant(context.Context, *berthv1.RevokeGrantRequest) (*berthv1.RevokeGrantResponse, error) {
	if f.lifeErr != nil {
		return nil, f.lifeErr
	}
	return &berthv1.RevokeGrantResponse{State: f.revoked}, nil
}

func (f *fakeSecrets) RemoveSecret(context.Context, *berthv1.RemoveSecretRequest) (*berthv1.RemoveSecretResponse, error) {
	if f.lifeErr != nil {
		return nil, f.lifeErr
	}
	return f.remove, nil
}

func (f *fakeSecrets) RotateSecret(context.Context, *berthv1.RotateSecretRequest) (*berthv1.RotateSecretResponse, error) {
	if f.lifeErr != nil {
		return nil, f.lifeErr
	}
	return &berthv1.RotateSecretResponse{Outcome: f.rotate}, nil
}

func (f *fakeSecrets) RequestSecret(_ context.Context, req *berthv1.RequestSecretRequest) (*berthv1.RequestSecretResponse, error) {
	f.reasons = append(f.reasons, req.GetReason())
	if f.storeErr != nil {
		return nil, f.storeErr
	}
	return &berthv1.RequestSecretResponse{Name: req.GetName(), Outcome: f.store}, nil
}

func (f *fakeSecrets) ListSecrets(context.Context, *berthv1.ListSecretsRequest) (*berthv1.ListSecretsResponse, error) {
	return f.list, nil
}

func (f *fakeSecrets) AuthorizeUse(_ context.Context, req *berthv1.AuthorizeUseRequest) (*berthv1.AuthorizeUseResponse, error) {
	f.authorize++
	f.useExe, f.useArgv = req.GetExePath(), req.GetArgv()
	if f.useErr != nil {
		return nil, f.useErr
	}
	resp := &berthv1.AuthorizeUseResponse{Outcome: f.use, PassEnv: f.passEnv}
	if f.use == berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED {
		resp.Ticket = bytes.Repeat([]byte{7}, 32)
	}
	return resp, nil
}

type result struct {
	code           int
	stdout, stderr string
}

func invoke(t *testing.T, fake *fakeSecrets, read ValueReader, args ...string) result {
	t.Helper()
	return invokeWith(t, fake, read, nil, args...)
}

func invokeWith(t *testing.T, fake *fakeSecrets, read ValueReader, start ProxyStarter, args ...string) result {
	t.Helper()
	server := connect.NewServer()
	berthv1connect.RegisterSecretsServiceHandler(server, fake)
	cl := &client.Client{Secrets: berthv1connect.NewSecretsServiceClient(connect.NewClient(connectinprocess.New(server)))}
	var out, errOut bytes.Buffer
	code := Run(t.Context(), args, Env{
		Stdin:          strings.NewReader(""),
		Stdout:         &out,
		Stderr:         &errOut,
		Connect:        func(context.Context) (*client.Client, error) { return cl, nil },
		ConnectTrusted: func(context.Context) (*client.Client, error) { return cl, nil },
		Read:           read,
		StartProxy:     start,
	})
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func reasonError(t *testing.T, reason berthv1.ErrorReason) error {
	t.Helper()
	detail, err := connectproto.NewErrorDetail(&berthv1.ErrorInfo{Reason: reason})
	if err != nil {
		t.Fatal(err)
	}
	return connect.NewError(connect.CodeAlreadyExists, "exists").WithDetail(detail)
}

func TestRequestAndAddMapOutcomes(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		fake       *fakeSecrets
		code       int
		stdout     string
		stderrHas  string
		wantReason string
	}{
		{
			"request stored",
			[]string{"secret", "request", "gh-token", "--why", "open a PR"},
			&fakeSecrets{store: berthv1.StoreOutcome_STORE_OUTCOME_STORED}, ExitOK, "saved gh-token\n", "", "open a PR",
		},
		{
			"request denied",
			[]string{"secret", "request", "--why", "open a PR", "gh-token"},
			&fakeSecrets{store: berthv1.StoreOutcome_STORE_OUTCOME_DENIED}, ExitDenied, "", "denied\n", "open a PR",
		},
		{
			"request failed",
			[]string{"secret", "request", "gh-token", "--why", "x"},
			&fakeSecrets{store: berthv1.StoreOutcome_STORE_OUTCOME_FAILED}, ExitFailed, "", "failed\n", "x",
		},
		{
			"add stored",
			[]string{"secret", "add", "gh-token"},
			&fakeSecrets{store: berthv1.StoreOutcome_STORE_OUTCOME_STORED}, ExitOK, "saved gh-token\n", "", addReason,
		},
		{
			"add exists",
			[]string{"secret", "add", "gh-token"},
			&fakeSecrets{storeErr: reasonError(t, berthv1.ErrorReason_ERROR_REASON_SECRET_EXISTS)}, ExitError, "",
			"secret gh-token already exists; use it with berth run", addReason,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := invoke(t, tc.fake, nil, tc.args...)
			if r.code != tc.code || r.stdout != tc.stdout || !strings.Contains(r.stderr, tc.stderrHas) {
				t.Fatalf("got code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
			}
			if tc.stderrHas == "" && r.stderr != "" {
				t.Fatalf("unexpected stderr %q", r.stderr)
			}
			if len(tc.fake.reasons) != 1 || tc.fake.reasons[0] != tc.wantReason {
				t.Fatalf("reasons sent: %q", tc.fake.reasons)
			}
		})
	}
}

func TestRequestWithoutWhyIsUsage(t *testing.T) {
	fake := &fakeSecrets{}
	if r := invoke(t, fake, nil, "secret", "request", "gh-token"); r.code != ExitUsage || len(fake.reasons) != 0 {
		t.Fatalf("got %+v, calls %d", r, len(fake.reasons))
	}
}

func TestSecretListGolden(t *testing.T) {
	at := func(s string) *timestamppb.Timestamp {
		ts, err := time.Parse(time.DateTime, s)
		if err != nil {
			t.Fatal(err)
		}
		return timestamppb.New(ts)
	}
	fake := &fakeSecrets{list: &berthv1.ListSecretsResponse{Secrets: []*berthv1.SecretSummary{
		{Name: "gh-token", State: berthv1.SecretState_SECRET_STATE_STORED, Consumers: []*berthv1.Consumer{
			{
				GrantId: 1, State: berthv1.GrantState_GRANT_STATE_ACTIVE, ExePath: "/opt/homebrew/bin/gh",
				Sha256: strings.Repeat("ab", 32), TeamId: "XPJ7XA42J4", ArgsGlob: "pr *",
				LastUsedAt: at("2026-11-16 09:30:00"), UseCount: 3,
			},
			{
				GrantId: 2, State: berthv1.GrantState_GRANT_STATE_STALE, ExePath: "/usr/local/bin/deploy",
				Sha256: strings.Repeat("0f", 32), LastUsedAt: at("2026-11-16 10:45:00"), UseCount: 2,
			},
		}},
		{Name: "openai-key", State: berthv1.SecretState_SECRET_STATE_ROTATING, Consumers: []*berthv1.Consumer{
			{
				GrantId: 12, State: berthv1.GrantState_GRANT_STATE_ACTIVE, Kind: berthv1.ConsumerKind_CONSUMER_KIND_PROXY,
				Proxy: &berthv1.ProxyTarget{
					Host: "api.openai.com", Port: 443, HeaderName: "Authorization", ValuePrefix: "Bearer ",
					PathGlob: "/v1/*", Methods: []string{"GET", "POST"},
				},
				UseCount: 4,
			},
			{
				GrantId: 13, State: berthv1.GrantState_GRANT_STATE_REVOKED, Kind: berthv1.ConsumerKind_CONSUMER_KIND_PROXY,
				Proxy: &berthv1.ProxyTarget{Host: "api.example.com", Port: 8443, HeaderName: "X-Api-Key"},
			},
		}},
		{Name: "npm-token", State: berthv1.SecretState_SECRET_STATE_PENDING},
	}}}
	r := invoke(t, fake, nil, "secret", "ls")
	want := `NAME        STATE     CONSUMERS  LAST USED            USES
gh-token    stored    2          2026-11-16 10:45:00  5
openai-key  rotating  2          never                4
npm-token   pending   0          never                0

gh-token
  1  /opt/homebrew/bin/gh   abababababab  XPJ7XA42J4  pr *      active
  2  /usr/local/bin/deploy  0f0f0f0f0f0f  unsigned    any args  stale

openai-key
  12  api.openai.com:443    Authorization  GET,POST    /v1/*     active
  13  api.example.com:8443  X-Api-Key      any method  any path  revoked
`
	if r.code != ExitOK || r.stdout != want {
		t.Fatalf("code %d, stderr %q, stdout:\n%s\nwant:\n%s", r.code, r.stderr, r.stdout, want)
	}
	if j := invoke(t, fake, nil, "secret", "ls", "--json"); j.code != ExitOK || !strings.Contains(j.stdout, `"exePath"`) {
		t.Fatalf("json: %+v", j)
	}
}

func TestLifecycleCommandsMapOutcomes(t *testing.T) {
	busy := reasonError(t, berthv1.ErrorReason_ERROR_REASON_BUSY)
	cases := []struct {
		name      string
		args      []string
		fake      *fakeSecrets
		code      int
		stdout    string
		stderrHas string
	}{
		{
			"proxy granted",
			[]string{
				"secret", "proxy", "openai-key", "--host", "api.openai.com", "--header", "Authorization", "--prefix", "Bearer ",
				"--method", "GET", "--method", "POST", "--path", "/v1/*", "--placeholder-env", "OPENAI_API_KEY",
			},
			&fakeSecrets{grant: berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED}, ExitOK,
			"granted openai-key to the proxy for api.openai.com:443\n  grant        9\n  header       Authorization: Bearer ****\n" +
				"  methods      GET,POST\n  path         /v1/*\n  placeholder  OPENAI_API_KEY\n", "",
		},
		{
			"proxy denied",
			[]string{"secret", "proxy", "k", "--host", "h.example", "--header", "X-Key"},
			&fakeSecrets{grant: berthv1.GrantOutcome_GRANT_OUTCOME_DENIED}, ExitDenied, "", "denied",
		},
		{
			"proxy bad target",
			[]string{"secret", "proxy", "k", "--host", "h.example", "--header", "X-Key"},
			&fakeSecrets{lifeErr: reasonError(t, berthv1.ErrorReason_ERROR_REASON_BAD_PROXY_TARGET)}, ExitError, "",
			"berthd will not proxy to h.example: use a DNS host name",
		},
		{"proxy without header", []string{"secret", "proxy", "k", "--host", "h.example"}, &fakeSecrets{}, ExitUsage, "", "usage:"},
		{
			"revoke",
			[]string{"secret", "revoke", "4"},
			&fakeSecrets{revoked: berthv1.GrantState_GRANT_STATE_REVOKED}, ExitOK, "revoked grant 4\n", "",
		},
		{
			"revoke not found",
			[]string{"secret", "revoke", "4"},
			&fakeSecrets{lifeErr: reasonError(t, berthv1.ErrorReason_ERROR_REASON_GRANT_NOT_FOUND)}, ExitError, "",
			"no grant has id 4; berth secret ls lists the grants",
		},
		{"revoke not an id", []string{"secret", "revoke", "gh-token"}, &fakeSecrets{}, ExitUsage, "", "usage:"},
		{
			"rm removed",
			[]string{"secret", "rm", "gh-token"},
			&fakeSecrets{remove: &berthv1.RemoveSecretResponse{
				Outcome: berthv1.RemoveOutcome_REMOVE_OUTCOME_REMOVED, RevokedGrantIds: []int64{1, 12},
			}}, ExitOK, "removed gh-token\nrevoked grants 1, 12\n", "",
		},
		{
			"rm without grants",
			[]string{"secret", "rm", "gh-token"},
			&fakeSecrets{remove: &berthv1.RemoveSecretResponse{Outcome: berthv1.RemoveOutcome_REMOVE_OUTCOME_REMOVED}},
			ExitOK, "removed gh-token\nrevoked no grants\n", "",
		},
		{
			"rm denied",
			[]string{"secret", "rm", "gh-token"},
			&fakeSecrets{remove: &berthv1.RemoveSecretResponse{Outcome: berthv1.RemoveOutcome_REMOVE_OUTCOME_DENIED}}, ExitDenied, "", "denied",
		},
		{"rm busy", []string{"secret", "rm", "gh-token"}, &fakeSecrets{lifeErr: busy}, ExitBusy, "", "secret gh-token is busy"},
		{
			"rotate",
			[]string{"secret", "rotate", "gh-token"},
			&fakeSecrets{rotate: berthv1.StoreOutcome_STORE_OUTCOME_STORED}, ExitOK,
			"rotated gh-token; runs already going keep the old value until they end\n", "",
		},
		{
			"rotate failed",
			[]string{"secret", "rotate", "gh-token"},
			&fakeSecrets{rotate: berthv1.StoreOutcome_STORE_OUTCOME_FAILED}, ExitFailed, "", "failed",
		},
		{"rotate busy", []string{"secret", "rotate", "gh-token"}, &fakeSecrets{lifeErr: busy}, ExitBusy, "", "secret gh-token is busy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := invoke(t, tc.fake, nil, tc.args...)
			if r.code != tc.code || r.stdout != tc.stdout || !strings.Contains(r.stderr, tc.stderrHas) {
				t.Fatalf("got code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
			}
			if tc.stderrHas == "" && r.stderr != "" {
				t.Fatalf("unexpected stderr %q", r.stderr)
			}
		})
	}
}

func TestSecretProxySendsTheTarget(t *testing.T) {
	fake := &fakeSecrets{grant: berthv1.GrantOutcome_GRANT_OUTCOME_GRANTED}
	invoke(t, fake, nil, "secret", "proxy", "k", "--host", "api.example.com", "--header", "Authorization", "--prefix", "Bearer ",
		"--suffix", "!", "--method", "POST", "--path", "/v1/*", "--placeholder-env", "K")
	want := &berthv1.ProxyTarget{
		Host: "api.example.com", Port: 443, HeaderName: "Authorization", ValuePrefix: "Bearer ", ValueSuffix: "!",
		PathGlob: "/v1/*", Methods: []string{"POST"}, PlaceholderEnv: "K",
	}
	if !proto.Equal(fake.target, want) {
		t.Fatalf("sent %v, want %v", fake.target, want)
	}
}
