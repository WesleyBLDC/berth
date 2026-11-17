// Package proxyctl holds what berth, berthd and berth-proxy must agree on: the two JSON lines
// they exchange and the rules both ends check. It imports only the standard library, so
// berth-proxy, which holds secret values, links nothing of the berth command through it.
package proxyctl

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

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

// MaxNames is how many secrets one proxy run may name.
const MaxNames = 16

const (
	// ReadyTimeout is how long a proxy has from its start to serving: berthd makes a session that
	// opened no watch stream by then Lost, and berth stops waiting for the control line.
	ReadyTimeout = 30 * time.Second
	// StopTimeout is how long a proxy has to end its session and exit once its stdin closed,
	// before its starter kills it.
	StopTimeout = 5 * time.Second
)

// SystemBundle is the trust store bundle.pem starts with.
const SystemBundle = "/etc/ssl/cert.pem"

var (
	// NamePattern is a secret name, as proto validation has it.
	NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	// PlaceholderPattern is a placeholder variable's name.
	PlaceholderPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
)

// BerthOwned are the variables berth run sets in a proxied child. They are removed from the
// caller's environment, so a caller's proxy or trust setting can neither bypass the proxy nor
// trust another CA, and a placeholder may never name one.
var BerthOwned = []string{
	"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
	"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "NODE_USE_ENV_PROXY",
}

// Env is the whole environment berth-proxy accepts and its starters pass. Anything else
// (GODEBUG, SSL_CERT_FILE, HTTPS_PROXY, GOTRACEBACK, DYLD_*) could change how TLS is verified,
// where it dials, or what a crash prints.
var Env = []string{"HOME", "PATH", "BERTH_HOME", "TMPDIR"}

// WriteTrustFiles makes a folder under parent (empty means os.TempDir) that only this user can
// read, holding ca.pem and bundle.pem, the system roots at systemBundle followed by ca. The paths
// are absolute, since TMPDIR may be relative. dir is returned whenever it was made, so the caller
// removes it even on an error.
func WriteTrustFiles(parent, systemBundle string, ca []byte) (dir, caFile, bundleFile string, err error) {
	system, err := os.ReadFile(systemBundle) //nolint:gosec // the fixed system trust store, or a test's
	if err != nil {
		return "", "", "", fmt.Errorf("cannot read the system trust bundle: %w", err)
	}
	if dir, err = os.MkdirTemp(parent, "berth-proxy-"); err != nil {
		return "", "", "", fmt.Errorf("cannot make the run folder: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir, "", "", fmt.Errorf("cannot resolve the run folder: %w", err)
	}
	caFile, bundleFile = filepath.Join(abs, "ca.pem"), filepath.Join(abs, "bundle.pem")
	if err := os.WriteFile(caFile, ca, 0o600); err != nil {
		return dir, "", "", fmt.Errorf("cannot write ca.pem: %w", err)
	}
	bundle := append(append(system, '\n'), ca...)
	if err := os.WriteFile(bundleFile, bundle, 0o600); err != nil { //nolint:gosec // inside the folder MkdirTemp just made
		return dir, "", "", fmt.Errorf("cannot write bundle.pem: %w", err)
	}
	return dir, caFile, bundleFile, nil
}
