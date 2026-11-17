package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"net"
	"strings"
	"testing"
	"time"

	berthv1 "berth/gen/berth/v1"
)

var oidNameConstraints = asn1.ObjectIdentifier{2, 5, 29, 30}

func TestCAIsConstrainedToTheGrantedHosts(t *testing.T) {
	now := time.Now()
	ca, err := NewCA([]string{"api.example.com", "other.test"}, now)
	if err != nil {
		t.Fatal(err)
	}
	cert := ca.Certificate()
	if !cert.IsCA || cert.MaxPathLen != 0 || !cert.MaxPathLenZero {
		t.Fatalf("IsCA %v MaxPathLen %d zero %v", cert.IsCA, cert.MaxPathLen, cert.MaxPathLenZero)
	}
	if _, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || cert.PublicKey.(*ecdsa.PublicKey).Curve != elliptic.P256() {
		t.Fatal("CA key is not ECDSA P-256")
	}
	critical := false
	for _, e := range cert.Extensions {
		if e.Id.Equal(oidNameConstraints) {
			critical = e.Critical
		}
	}
	if !critical {
		t.Fatal("name constraints missing or not critical")
	}
	if got := strings.Join(cert.PermittedDNSDomains, ","); got != "api.example.com,other.test" {
		t.Fatalf("permitted %s", got)
	}
	if life := cert.NotAfter.Sub(now); life > MaxCALifetime {
		t.Fatalf("CA lives %s", life)
	}

	if _, err := ca.Leaf("evil.test"); err == nil {
		t.Fatal("leaf issued for a host that is not granted")
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	leaf, err := ca.Leaf("api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := ca.Leaf("api.example.com")
	if again != leaf {
		t.Fatal("leaf not cached")
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "api.example.com"}); err != nil {
		t.Fatalf("granted leaf: %v", err)
	}
	// Even a leaf forged with the CA's own key fails for any name the grants do not cover.
	for _, forged := range []*x509.Certificate{
		{DNSNames: []string{"evil.test"}},
		{DNSNames: []string{"x.api.example.com"}},
		{DNSNames: []string{"example.com"}},
		{IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}},
	} {
		forged.SerialNumber = randomSerial()
		forged.Subject = pkix.Name{CommonName: "forged"}
		forged.NotBefore, forged.NotAfter = cert.NotBefore, cert.NotAfter
		forged.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		der, err := x509.CreateCertificate(rand.Reader, forged, cert, &ca.leafKey.PublicKey, ca.key)
		if err != nil {
			t.Fatal(err)
		}
		parsed, _ := x509.ParseCertificate(der)
		if _, err := parsed.Verify(x509.VerifyOptions{Roots: roots}); err == nil {
			t.Fatalf("forged leaf for %v %v verified", forged.DNSNames, forged.IPAddresses)
		}
	}
	// A CA can never sign a further CA: path length 0.
	sub := &x509.Certificate{
		SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: "sub"}, NotBefore: cert.NotBefore,
		NotAfter: cert.NotAfter, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	subDER, _ := x509.CreateCertificate(rand.Reader, sub, cert, &ca.leafKey.PublicKey, ca.key)
	subCert, _ := x509.ParseCertificate(subDER)
	leafTmpl := &x509.Certificate{
		SerialNumber: randomSerial(), DNSNames: []string{"api.example.com"}, NotBefore: cert.NotBefore,
		NotAfter: cert.NotAfter, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTmpl, subCert, &ca.leafKey.PublicKey, ca.leafKey)
	chained, _ := x509.ParseCertificate(leafDER)
	inter := x509.NewCertPool()
	inter.AddCert(subCert)
	if _, err := chained.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, DNSName: "api.example.com"}); err == nil {
		t.Fatal("a sub CA chained under the run CA")
	}
}

func TestNestedGrantsKeepEachOther(t *testing.T) {
	ca, err := NewCA([]string{"example.com", "api.example.com"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Certificate())
	for _, h := range []string{"example.com", "api.example.com"} {
		leaf, err := ca.Leaf(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: h}); err != nil {
			t.Fatalf("%s: %v", h, err)
		}
	}
}

func TestGrantChecksRepeatTheContract(t *testing.T) {
	ok := func() *berthv1.ProxyTarget {
		return &berthv1.ProxyTarget{Host: "api.example.com", HeaderName: "X-Api-Key"}
	}
	if g, err := newGrant("k", ok(), false); err != nil || g.Port != 443 || g.Header != "X-Api-Key" {
		t.Fatalf("%v %v", g, err)
	}
	for name, mod := range map[string]func(*berthv1.ProxyTarget){
		"host header":        func(p *berthv1.ProxyTarget) { p.HeaderName = "host" },
		"content length":     func(p *berthv1.ProxyTarget) { p.HeaderName = "Content-Length" },
		"proxy header":       func(p *berthv1.ProxyTarget) { p.HeaderName = "Proxy-Authorization" },
		"transfer encoding":  func(p *berthv1.ProxyTarget) { p.HeaderName = "transfer-encoding" },
		"bad token":          func(p *berthv1.ProxyTarget) { p.HeaderName = "X Key" },
		"IP literal":         func(p *berthv1.ProxyTarget) { p.Host = "127.0.0.1" },
		"localhost":          func(p *berthv1.ProxyTarget) { p.Host = "localhost" },
		"numeric last label": func(p *berthv1.ProxyTarget) { p.Host = "127.1" },
		"uppercase host":     func(p *berthv1.ProxyTarget) { p.Host = "API.example.com" },
		"CRLF prefix":        func(p *berthv1.ProxyTarget) { p.ValuePrefix = "a\r\nb" },
		"bad glob":           func(p *berthv1.ProxyTarget) { p.PathGlob = "/v1/[" },
		"lowercase method":   func(p *berthv1.ProxyTarget) { p.Methods = []string{"get"} },
		"port":               func(p *berthv1.ProxyTarget) { p.Port = 70000 },
		"owned placeholder":  func(p *berthv1.ProxyTarget) { p.PlaceholderEnv = "SSL_CERT_FILE" },
		"bad placeholder":    func(p *berthv1.ProxyTarget) { p.PlaceholderEnv = "lower" },
	} {
		p := ok()
		mod(p)
		if _, err := newGrant("k", p, false); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	local := ok()
	local.Host = "127.0.0.1"
	if _, err := newGrant("k", local, true); err != nil {
		t.Fatalf("end to end builds accept loopback grants: %v", err)
	}
	if _, err := buildGrants([]string{"a", "b"}, []*berthv1.ProxyTicket{
		{Name: "a", Ticket: make([]byte, 32), Target: ok()},
		{Name: "b", Ticket: make([]byte, 32), Target: ok()},
	}, false); err == nil {
		t.Fatal("two grants on one host and port accepted")
	}
}

func TestLockValueRefusesAndZeroes(t *testing.T) {
	for _, v := range []string{"a\rb", "a\nb", "a\x00b", ""} {
		raw := []byte(v)
		if _, err := lockValue(raw); err == nil || strings.Contains(err.Error(), v+"x") {
			t.Fatalf("%q accepted", v)
		}
		if string(raw) != string(make([]byte, len(raw))) {
			t.Fatalf("%q not zeroed", v)
		}
	}
	raw := []byte(canary)
	v, err := lockValue(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(make([]byte, len(raw))) || string(v.Bytes()) != canary {
		t.Fatal("value not moved")
	}
	v.Zero()
	if string(v.Bytes()) != string(make([]byte, len(canary))) {
		t.Fatal("value not zeroed")
	}
}
