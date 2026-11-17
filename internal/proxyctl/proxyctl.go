// Package proxyctl holds the two JSON lines berth and berth-proxy exchange, so berth-proxy, which
// holds secret values, links nothing of the berth command beyond them.
package proxyctl

// Request is the one line berth writes to berth-proxy's stdin.
type Request struct {
	Secrets []string `json:"secrets"`
}

// Control is the one line berth-proxy writes to its stdout once it serves. It carries no value:
// the credential only lets the child use the proxy, and placeholders are stand ins the proxy
// swaps for nothing but its own header.
type Control struct {
	Port         int               `json:"port"`
	Credential   string            `json:"credential"`
	CAFile       string            `json:"ca_file"`
	BundleFile   string            `json:"bundle_file"`
	Placeholders map[string]string `json:"placeholders"`
}
