package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"berth/internal/logging"
)

func TestSecretsNeverReachTheLog(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(logging.NewRedacting(slog.NewTextHandler(&buf, nil)))
	const canary = "canary-7f3a9"
	log.With("session_token", canary).Info("hello",
		"password", canary, "Authorization", canary,
		slog.Group("grant", "api_key", canary, "site", "example.com"),
		"entity", "daemon")
	out := buf.String()
	if strings.Contains(out, canary) {
		t.Fatalf("secret leaked: %s", out)
	}
	if !strings.Contains(out, "entity=daemon") || !strings.Contains(out, "grant.site=example.com") {
		t.Fatalf("ordinary attributes lost: %s", out)
	}
}
