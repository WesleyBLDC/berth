package envpolicy_test

import (
	"slices"
	"testing"

	"berth/internal/core/envpolicy"
)

func TestChildGetsOnlyTheAllowlistAndPassedNames(t *testing.T) {
	environ := []string{
		"HOME=/h", "PATH=/bin", "TERM=xterm", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.sshCommand",
		"NODE_OPTIONS=--require /tmp/x.js", "DYLD_INSERT_LIBRARIES=/tmp/x.dylib", "PERL5OPT=-Mx", "GH_HOST=example.com",
		"LD_PRELOAD=/x", "AWS_REGION=eu-west-1",
	}
	got := envpolicy.Child(environ, []string{"AWS_REGION", "NODE_OPTIONS", "DYLD_INSERT_LIBRARIES"})
	want := []string{"HOME=/h", "PATH=/bin", "TERM=xterm", "AWS_REGION=eu-west-1"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestStrippedAndFilter(t *testing.T) {
	for name, want := range map[string]bool{
		"DYLD_LIBRARY_PATH": true, "LD_PRELOAD": true, "NODE_OPTIONS": true, "JAVA_TOOL_OPTIONS": true,
		"MAVEN_OPTS": true, "PERL5OPT": true, "RUBYOPT": true, "AWS_REGION": false, "GH_HOST": false,
	} {
		if envpolicy.Stripped(name) != want {
			t.Errorf("%s: want stripped %v", name, want)
		}
	}
	if got := envpolicy.Filter([]string{"A", "NODE_OPTIONS", "A", "B"}); !slices.Equal(got, []string{"A", "B"}) {
		t.Fatalf("filter %v", got)
	}
}

func TestKeepDropsEntriesWithoutAValue(t *testing.T) {
	got := envpolicy.Keep([]string{"A=1", "B", "C=", "D=4"}, func(k string) bool { return k != "D" })
	if !slices.Equal(got, []string{"A=1", "C="}) {
		t.Fatalf("got %v", got)
	}
}
