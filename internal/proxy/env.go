// Package proxy is berth-proxy: a loopback HTTPS forward proxy that writes one secret header into
// requests to exactly the hosts a person granted, and tunnels everything else untouched. The
// value never becomes a Go string, lives in locked memory outside the Go heap, and is zeroed on
// every exit path (docs/plans/e2b-header-proxy.md).
package proxy

import (
	"fmt"
	"slices"
	"strings"
)

// allowedEnv is the whole environment berth-proxy accepts. Anything else (GODEBUG, SSL_CERT_FILE,
// HTTPS_PROXY, GOTRACEBACK, DYLD_*) could change how TLS is verified, where it dials, or what a
// crash prints, so the proxy refuses to start rather than guess which variables are harmless.
var allowedEnv = []string{"HOME", "PATH", "BERTH_HOME", "TMPDIR"}

// CheckEnv refuses an environment holding any variable beyond allowedEnv and extra (extra is
// only ever set by binaries built for the end to end tests).
func CheckEnv(environ, extra []string) error {
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("refusing to start: the environment holds a malformed entry")
		}
		if !slices.Contains(allowedEnv, k) && !slices.Contains(extra, k) {
			return fmt.Errorf("refusing to start: the environment holds %s; berth-proxy accepts only HOME, PATH, BERTH_HOME and TMPDIR", k)
		}
	}
	return nil
}
