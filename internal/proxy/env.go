// Package proxy is berth-proxy: a loopback HTTPS forward proxy that writes one secret header into
// requests to exactly the hosts a person granted, and tunnels everything else untouched. The
// value never becomes a Go string, lives in locked memory outside the Go heap, and is zeroed on
// every exit path (docs/plans/e2b-header-proxy.md).
package proxy

import (
	"fmt"
	"slices"
	"strings"

	"berth/internal/proxyctl"
)

// CheckEnv refuses an environment holding any variable beyond proxyctl.Env and extra (extra is
// only ever set by binaries built for the end to end tests), rather than guess which variables
// are harmless.
func CheckEnv(environ, extra []string) error {
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("refusing to start: the environment holds a malformed entry")
		}
		if !slices.Contains(proxyctl.Env, k) && !slices.Contains(extra, k) {
			return fmt.Errorf("refusing to start: the environment holds %s; berth-proxy accepts only HOME, PATH, BERTH_HOME and TMPDIR", k)
		}
	}
	return nil
}
