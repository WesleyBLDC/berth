package proxy

import (
	"os/exec"
	"strings"
	"testing"
)

// berth-proxy holds secret values, so it links no terminal UI code: none of the modules berth top
// brings in may reach it, in a release build or the end to end one.
func TestBerthProxyLinksNoUIModules(t *testing.T) {
	for _, tags := range []string{"", "berth_e2e"} {
		out, err := exec.CommandContext(t.Context(), "go", "list", "-tags", tags, "-deps", "berth/cmd/berth-proxy").CombinedOutput()
		if err != nil {
			t.Fatalf("go list -tags %q: %v\n%s", tags, err, out)
		}
		for dep := range strings.Lines(string(out)) {
			dep = strings.TrimSpace(dep)
			if dep == "berth/internal/cli" {
				t.Errorf("berth-proxy (tags %q) depends on the berth command", tags)
			}
			for _, banned := range []string{"charm.land/", "github.com/charmbracelet/", "github.com/muesli/"} {
				if strings.HasPrefix(dep, banned) {
					t.Errorf("berth-proxy (tags %q) depends on %s", tags, dep)
				}
			}
		}
	}
}
