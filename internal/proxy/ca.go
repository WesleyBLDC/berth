package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// MaxCALifetime bounds a run's CA; a run that outlives it stops, since clients would refuse it.
const MaxCALifetime = 24 * time.Hour

// clockSkew backdates the CA so a client whose clock is slightly behind still accepts it.
const clockSkew = time.Minute

// CA is the per run certificate authority. Its key exists only in this process, it may only
// issue for the granted hosts (critical name constraints, path length 0), and Leaf refuses any
// other host.
type CA struct {
	cert     *x509.Certificate
	key      *ecdsa.PrivateKey
	leafKey  *ecdsa.PrivateKey
	hosts    map[string]bool
	notAfter time.Time

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// NewCA makes a CA valid from now for at most MaxCALifetime, permitting only hosts.
func NewCA(hosts []string, now time.Time) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "berth-proxy run CA", Organization: []string{"Berth"}},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(MaxCALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	constrain(tmpl, hosts)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		allowed[h] = true
	}
	return &CA{cert: cert, key: key, leafKey: leafKey, hosts: allowed, notAfter: tmpl.NotAfter, leaves: map[string]*tls.Certificate{}}, nil
}

// constrain permits exactly the granted names. A DNS constraint also permits every subdomain, so
// each host's subdomains are excluded unless another granted host lives under it. With no IP
// grant, every IP address is excluded, since a name type without constraints is unconstrained.
func constrain(tmpl *x509.Certificate, hosts []string) {
	tmpl.PermittedDNSDomainsCritical = true
	var ips []net.IP
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
			continue
		}
		tmpl.PermittedDNSDomains = append(tmpl.PermittedDNSDomains, h)
		nested := false
		for _, other := range hosts {
			if strings.HasSuffix(other, "."+h) {
				nested = true
			}
		}
		if !nested {
			tmpl.ExcludedDNSDomains = append(tmpl.ExcludedDNSDomains, "."+h)
		}
	}
	if len(ips) == 0 {
		tmpl.ExcludedIPRanges = []*net.IPNet{
			{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		}
		return
	}
	for _, ip := range ips {
		bits := 128
		if ip4 := ip.To4(); ip4 != nil {
			ip, bits = ip4, 32
		}
		tmpl.PermittedIPRanges = append(tmpl.PermittedIPRanges, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
}

func randomSerial() *big.Int {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return new(big.Int).SetBytes(b)
}

// PEM is the CA certificate, for ca.pem and bundle.pem.
func (c *CA) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

// Certificate is the parsed CA certificate.
func (c *CA) Certificate() *x509.Certificate { return c.cert }

// NotAfter is when the CA, and so the run, expires.
func (c *CA) NotAfter() time.Time { return c.notAfter }

// Leaf returns the cached leaf for a granted host and refuses any other.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	if !c.hosts[host] {
		return nil, fmt.Errorf("no leaf for %s: it is not granted", host)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaf, ok := c.leaves[host]; ok {
		return leaf, nil
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    c.cert.NotBefore,
		NotAfter:     c.notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &c.leafKey.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: c.leafKey, Leaf: parsed}
	c.leaves[host] = leaf
	return leaf, nil
}
