//go:build berth_e2e

package main

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"

	"berth/internal/adapters/execid"
	"berth/internal/codetrust"
	"berth/internal/core/ports"
	"berth/internal/paths"
)

// testRootDER is a base64 DER certificate the end to end tests compile in with
// -ldflags "-X main.testRootDER=...". Compiling it in, rather than reading a file at run time,
// leaves nothing on disk that could be swapped while the proxy runs and needs no extra
// environment variable, which the proxy would refuse.
var testRootDER string

// End to end builds also accept BERTH_NATIVE and grants on localhost or IP literals, since the
// test servers listen on loopback.
var (
	extraEnv   = []string{"BERTH_NATIVE"}
	allowLocal = true
)

// nativeHelper honours BERTH_NATIVE only in binaries built with -tags berth_e2e.
func nativeHelper() (string, error) {
	if h := os.Getenv("BERTH_NATIVE"); h != "" {
		return h, nil
	}
	return paths.Beside("berth-native")
}

// filePinner accepts the ad hoc signed test berthd by its identifier, without Apple's chain;
// the live check still demands a valid signature and the hardened runtime.
func filePinner(x *execid.Identity) codetrust.Pinner {
	return func(ctx context.Context, path, identifier string) (ports.CDHash, bool, error) {
		return x.PinSigned(ctx, path, `identifier "`+identifier+`"`)
	}
}

// roots is the system pool plus the compiled in test root; on macOS the system pool keeps the
// platform verifier and falls back to the added root.
func roots() *x509.CertPool {
	if testRootDER == "" {
		return nil
	}
	der, err := base64.StdEncoding.DecodeString(testRootDER)
	if err != nil {
		panic(fmt.Sprintf("berth-proxy: malformed test root: %v", err))
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		panic(fmt.Sprintf("berth-proxy: malformed test root: %v", err))
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	pool.AddCert(cert)
	return pool
}
