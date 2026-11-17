package execid

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHashFollowsContentAndSymlinks(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "tool")
	if err := os.WriteFile(file, []byte("#!/bin/sh\necho one\n"), 0o700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	id := New(DefaultCodesign)
	first, err := id.Identify(t.Context(), link)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path != file {
		t.Fatalf("path %q, want the resolved %q", first.Path, file)
	}
	if first.SHA256 != sha256.Sum256([]byte("#!/bin/sh\necho one\n")) {
		t.Fatalf("hash is not the file's SHA-256")
	}
	if first.Signed() || first.SigningID != "" {
		t.Fatalf("an unsigned script has signature facts: %+v", first)
	}
	if err := os.WriteFile(file, []byte("#!/bin/sh\necho two\n"), 0o700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	second, err := id.Identify(t.Context(), link)
	if err != nil {
		t.Fatal(err)
	}
	if second.SHA256 == first.SHA256 {
		t.Fatalf("a changed file kept its hash")
	}
}

func TestMissingFileWrapsNotExist(t *testing.T) {
	_, err := New(DefaultCodesign).Identify(t.Context(), filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

func TestDirectoryIsRejected(t *testing.T) {
	if _, err := New(DefaultCodesign).Identify(t.Context(), t.TempDir()); err == nil {
		t.Fatal("a directory was identified as an executable")
	}
}

func TestSignedSystemBinary(t *testing.T) {
	got, err := New(DefaultCodesign).Identify(t.Context(), "/bin/ls")
	if err != nil {
		t.Fatal(err)
	}
	// Apple platform binaries carry an identifier but no team.
	if got.SigningID != "com.apple.ls" {
		t.Fatalf("SigningID %q, want com.apple.ls", got.SigningID)
	}
	if got.Path != "/bin/ls" {
		t.Fatalf("path %q", got.Path)
	}
}

func TestBrokenCodesignIsAnError(t *testing.T) {
	if _, err := New("/nonexistent/codesign").Identify(t.Context(), "/bin/ls"); err == nil {
		t.Fatal("a missing codesign tool made /bin/ls look unsigned")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := New(DefaultCodesign).Identify(ctx, "/bin/ls"); err == nil {
		t.Fatal("a cancelled context made /bin/ls look unsigned")
	}
}

func TestParseDisplay(t *testing.T) {
	cases := []struct {
		out, team, signing string
	}{
		{"Executable=/x\nIdentifier=com.github.gh\nTeamIdentifier=ABCDE12345\n", "ABCDE12345", "com.github.gh"},
		{"Identifier=xadhoc\nSignature=adhoc\nTeamIdentifier=not set\n", "", "xadhoc"},
		{"", "", ""},
	}
	for _, c := range cases {
		team, signing := parseDisplay([]byte(c.out))
		if team != c.team || signing != c.signing {
			t.Fatalf("%q: got %q %q", c.out, team, signing)
		}
	}
}

func TestTamperedSignedBinaryLosesSignature(t *testing.T) {
	b, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	b[5000] ^= 0xff // inside a signed code page
	p := filepath.Join(t.TempDir(), "true")
	if err := os.WriteFile(p, b, 0o700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	got, err := New(DefaultCodesign).Identify(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.SigningID != "" || got.TeamID != "" {
		t.Fatalf("a tampered binary kept its signature facts: %+v", got)
	}
}

func TestSatisfiesChecksTheRequirement(t *testing.T) {
	id := New(DefaultCodesign)
	if ok, err := id.Satisfies(t.Context(), "/bin/ls", "anchor apple"); err != nil || !ok {
		t.Fatalf("/bin/ls against anchor apple: %v, %v", ok, err)
	}
	if ok, err := id.Satisfies(t.Context(), "/bin/ls", `anchor apple generic and certificate leaf[subject.OU] = "XPJ7XA42J4"`); err != nil || ok {
		t.Fatalf("/bin/ls against another team: %v, %v", ok, err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The Go linker signs ad hoc, which no anchor accepts.
	if ok, err := id.Satisfies(t.Context(), self, "anchor apple generic"); err != nil || ok {
		t.Fatalf("ad hoc test binary: %v, %v", ok, err)
	}
}

func TestShebangAndCDHash(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "deploy")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	id := New(DefaultCodesign)
	if exe, err := id.Identify(t.Context(), script); err != nil || !exe.Shebang {
		t.Fatalf("script: %+v, %v", exe, err)
	}
	if exe, err := id.Identify(t.Context(), "/bin/ls"); err != nil || exe.Shebang {
		t.Fatalf("/bin/ls: %+v, %v", exe, err)
	}
	h, err := id.CDHash(t.Context(), "/bin/ls")
	if err != nil || h == ([20]byte{}) {
		t.Fatalf("cdhash %x, %v", h, err)
	}
	if _, err := id.CDHash(t.Context(), script); err == nil {
		t.Fatal("an unsigned script has a cdhash")
	}
}
