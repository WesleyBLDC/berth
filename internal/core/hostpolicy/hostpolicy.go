// Package hostpolicy decides which proxy target hosts reach this Mac or a private network rather
// than a public API. berthd refuses to grant them and berth-proxy refuses to serve them, with
// one rule so the two never drift apart.
package hostpolicy

import (
	"net"
	"strings"
)

// Local is an IP literal, a name whose last label is all digits (which resolvers read as an
// address, like 127.1), or localhost and its subdomains.
func Local(host string) bool {
	if net.ParseIP(host) != nil || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	last := host[strings.LastIndexByte(host, '.')+1:]
	return strings.Trim(last, "0123456789") == ""
}
