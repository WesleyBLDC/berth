package testutil

import (
	"os/exec"
	"strings"
	"testing"
)

// RequireNoUIModules fails t when the package, in a release build or the end to end one, depends
// on the berth command or a module berth top brings in: binaries that hold secrets or face agents
// stay small.
func RequireNoUIModules(t *testing.T, pkg string) {
	t.Helper()
	for _, tags := range []string{"", "berth_e2e"} {
		out, err := exec.CommandContext(t.Context(), "go", "list", "-tags", tags, "-deps", pkg).CombinedOutput() //nolint:gosec // a package path the test names
		if err != nil {
			t.Fatalf("go list -tags %q: %v\n%s", tags, err, out)
		}
		for dep := range strings.Lines(string(out)) {
			dep = strings.TrimSpace(dep)
			if dep == "berth/internal/cli" {
				t.Errorf("%s (tags %q) depends on the berth command", pkg, tags)
			}
			for _, banned := range []string{"charm.land/", "github.com/charmbracelet/", "github.com/muesli/"} {
				if strings.HasPrefix(dep, banned) {
					t.Errorf("%s (tags %q) depends on %s", pkg, tags, dep)
				}
			}
		}
	}
}
