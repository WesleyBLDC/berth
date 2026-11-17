package mcp

import (
	"testing"

	"berth/internal/testutil"
)

// berth-mcp faces agents, so it links no terminal UI code and not the berth command.
func TestBerthMCPLinksNoUIModules(t *testing.T) {
	testutil.RequireNoUIModules(t, "berth/cmd/berth-mcp")
}
