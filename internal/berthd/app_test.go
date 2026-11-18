package berthd

import (
	"path/filepath"
	"testing"

	"berth/internal/paths"
)

// berthd trusts only the app in its own bundle: Contents/MacOS/Berth beside Contents/Helpers.
func TestTheAppIsPinnedFromTheBundle(t *testing.T) {
	self, err := paths.Self()
	if err != nil {
		t.Fatal(err)
	}
	got, err := appExecutable(Config{})
	if want := filepath.Join(filepath.Dir(filepath.Dir(self)), "MacOS", "Berth"); err != nil || got != want {
		t.Fatalf("app path %q (%v), want %q", got, err, want)
	}
	if got, _ := appExecutable(Config{App: "/x/Berth"}); got != "/x/Berth" {
		t.Fatalf("a configured path became %q", got)
	}
}
