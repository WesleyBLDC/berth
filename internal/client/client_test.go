package client_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"berth/internal/client"
)

func TestReadAgentToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("berth_at_abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := client.ReadAgentToken(path); err != nil || got != "berth_at_abc" {
		t.Fatalf("got %q %v", got, err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadAgentToken(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a group readable token file: %v", err)
	}
	if _, err := client.ReadAgentToken(dir); err == nil {
		t.Fatal("a folder was read as a token file")
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReadAgentToken(empty); err == nil {
		t.Fatal("an empty token file was accepted")
	}
}
