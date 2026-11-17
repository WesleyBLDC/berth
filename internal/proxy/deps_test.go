package proxy

import (
	"testing"

	"berth/internal/testutil"
)

// berth-proxy holds secret values, so it links no terminal UI code.
func TestBerthProxyLinksNoUIModules(t *testing.T) {
	testutil.RequireNoUIModules(t, "berth/cmd/berth-proxy")
}
