package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	berthv1 "berth/gen/berth/v1"
)

const canary = "canary-7f3a9c-not-a-secret"

func TestRunMasksTheValueAndPassesTheExitCode(t *testing.T) {
	fake := &fakeSecrets{use: berthv1.UseOutcome_USE_OUTCOME_AUTHORIZED}
	var handed []byte
	read := func(_ context.Context, ticket []byte) ([]byte, error) {
		if len(ticket) != 32 {
			t.Errorf("ticket length %d", len(ticket))
		}
		handed = []byte(canary)
		return handed, nil
	}
	r := invoke(t, fake, read, "run", "--env", "KEY=gh-token", "--", "/bin/sh", "-c",
		`printf %s "$KEY"; printf %s "$KEY" >&2; exit 7`)
	if r.code != 7 {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	for name, out := range map[string]string{"stdout": r.stdout, "stderr": r.stderr} {
		if strings.Contains(out, canary) || out != maskReplacement {
			t.Fatalf("%s = %q", name, out)
		}
	}
	if fake.useExe != "/bin/sh" || strings.Join(fake.useArgv, " ") != `-c printf %s "$KEY"; printf %s "$KEY" >&2; exit 7` {
		t.Fatalf("authorized %q %q", fake.useExe, fake.useArgv)
	}
	if strings.Trim(string(handed), "\x00") != "" {
		t.Fatalf("value buffer not zeroed: %q", handed)
	}
}

func TestRunRefusalDoesNotStartTheChild(t *testing.T) {
	cases := []struct {
		outcome berthv1.UseOutcome
		code    int
		says    string
	}{
		{berthv1.UseOutcome_USE_OUTCOME_NO_GRANT, ExitNoGrant, "no grant lets /bin/sh use gh-token"},
		{berthv1.UseOutcome_USE_OUTCOME_STALE_GRANT, ExitStaleGrant, "/bin/sh changed since it was granted gh-token"},
	}
	for _, tc := range cases {
		t.Run(tc.outcome.String(), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "started")
			read := func(context.Context, []byte) ([]byte, error) {
				t.Error("a refused run read a value")
				return nil, nil
			}
			r := invoke(t, &fakeSecrets{use: tc.outcome}, read, "run", "--env", "KEY=gh-token", "--", "/bin/sh", "-c", "touch "+marker)
			if r.code != tc.code || !strings.Contains(r.stderr, tc.says) ||
				!strings.Contains(r.stderr, "berth secret grant gh-token --exe /bin/sh") {
				t.Fatalf("code %d, stderr %q", r.code, r.stderr)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the child ran: %v", err)
			}
		})
	}
}
