// Package envpolicy decides which environment variables berth run hands a consumer. The child
// gets a fixed allowlist, the names its grant lists, and the secret keys, never the agent's
// whole environment, because variables like NODE_OPTIONS or GIT_CONFIG_* turn a granted tool
// into something that runs code the agent chose.
package envpolicy

import "strings"

// Base is what every consumer gets from berth's environment.
var Base = []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL", "LC_CTYPE", "USER", "LOGNAME", "SHELL", "TMPDIR"}

// Stripped reports a variable that is never passed, even when a grant lists it: the dynamic
// loader's DYLD_* and LD_*, and the *_OPTIONS, *_OPTS and *OPT families (NODE_OPTIONS,
// JAVA_TOOL_OPTIONS, MAVEN_OPTS, PERL5OPT, RUBYOPT) that make a tool load or run other code.
func Stripped(name string) bool {
	return strings.HasPrefix(name, "DYLD_") || strings.HasPrefix(name, "LD_") ||
		strings.HasSuffix(name, "_OPTIONS") || strings.HasSuffix(name, "_OPTS") || strings.HasSuffix(name, "OPT")
}

// Filter drops stripped and repeated names, keeping the order.
func Filter(names []string) []string {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, n := range names {
		if !Stripped(n) && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// Child builds a consumer's environment from berth's own: the Base names, then the passed
// names, each taken from environ when present. Stripped names never pass. The secret entries
// are added by the caller.
func Child(environ, passed []string) []string {
	allowed := map[string]bool{}
	for _, n := range Base {
		allowed[n] = true
	}
	for _, n := range passed {
		allowed[n] = true
	}
	var out []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if ok && allowed[k] && !Stripped(k) {
			out = append(out, kv)
		}
	}
	return out
}
