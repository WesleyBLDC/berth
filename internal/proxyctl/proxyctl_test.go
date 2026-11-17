package proxyctl_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"berth/internal/proxyctl"
)

// berth-proxy links this package, so it must stay standard library only.
func TestImportsOnlyTheStandardLibrary(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "berth/internal/proxyctl").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "berth/internal/proxyctl" {
		t.Fatalf("proxyctl depends on more than the standard library:\n%s", got)
	}
}

func TestWriteTrustFiles(t *testing.T) {
	parent := t.TempDir()
	system := filepath.Join(parent, "cert.pem")
	if err := os.WriteFile(system, []byte("SYSTEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, caFile, bundleFile, err := proxyctl.WriteTrustFiles(parent, system, []byte("CA"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(caFile) || !filepath.IsAbs(bundleFile) || filepath.Dir(caFile) != filepath.Dir(bundleFile) {
		t.Fatalf("paths %q %q", caFile, bundleFile)
	}
	for file, want := range map[string]string{caFile: "CA", bundleFile: "SYSTEM\nCA"} {
		got, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(got, []byte(want)) {
			t.Fatalf("%s = %q %v, want %q", file, got, err, want)
		}
		if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", file, st.Mode())
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("folder mode %v", st.Mode())
	}

	if dir, _, _, err := proxyctl.WriteTrustFiles(parent, filepath.Join(parent, "missing"), []byte("CA")); err == nil || dir != "" {
		t.Fatalf("a missing system bundle: dir %q err %v", dir, err)
	}
}
