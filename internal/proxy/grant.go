package proxy

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/core/hostpolicy"
	"berth/internal/proxyctl"
)

// The same rules proto validation applies to ProxyTarget; berth-proxy checks them again so a
// wrong or compromised daemon reply cannot widen what it injects.
var (
	hostPattern   = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?[.])*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	headerPattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,64}$")
	methodPattern = regexp.MustCompile(`^[A-Z]{1,16}$`)
	affixPattern  = regexp.MustCompile(`^[\x20-\x7E]{0,128}$`)
	globPattern   = regexp.MustCompile(`^[\x21-\x7E]*$`)
)

// ungrantable frame or route a request, or talk to the proxy itself.
var ungrantable = []string{"host", "content-length", "transfer-encoding", "connection", "upgrade", "te", "trailer"}

// Placeholder is what berth run puts in a grant's placeholder variable, for tools that refuse
// to start without a key. It is a fixed public string, never a value.
const Placeholder = "berth-proxy-placeholder"

// Grant is one header the proxy writes on one host and port.
type Grant struct {
	Name        string
	Host        string
	Port        int
	Header      string
	Prefix      []byte
	Suffix      []byte
	PathGlob    string
	Methods     []string
	Placeholder string
	value       *Value
}

func (g *Grant) authority() string { return net.JoinHostPort(g.Host, strconv.Itoa(g.Port)) }

func grantableHeader(name string) bool {
	l := strings.ToLower(name)
	return headerPattern.MatchString(name) && !slices.Contains(ungrantable, l) && !strings.HasPrefix(l, "proxy-")
}

// newGrant checks a target from berthd; allowLocal is true only in end to end builds.
func newGrant(name string, t *berthv1.ProxyTarget, allowLocal bool) (*Grant, error) {
	if t == nil {
		return nil, errors.New("berthd sent a ticket without a target")
	}
	host := t.GetHost()
	switch {
	case !hostPattern.MatchString(host) || len(host) > 253:
		return nil, fmt.Errorf("grant %s: host is not a lowercase DNS name", name)
	case hostpolicy.Local(host) && !allowLocal:
		return nil, fmt.Errorf("grant %s: IP literals and localhost cannot be granted", name)
	case t.GetPort() > 65535:
		return nil, fmt.Errorf("grant %s: port out of range", name)
	case !grantableHeader(t.GetHeaderName()):
		return nil, fmt.Errorf("grant %s: header %q cannot be granted", name, t.GetHeaderName())
	case !affixPattern.MatchString(t.GetValuePrefix()) || !affixPattern.MatchString(t.GetValueSuffix()):
		return nil, fmt.Errorf("grant %s: value prefix and suffix must be printable ASCII", name)
	case !globPattern.MatchString(t.GetPathGlob()) || len(t.GetPathGlob()) > 1024:
		return nil, fmt.Errorf("grant %s: malformed path glob", name)
	case t.GetPlaceholderEnv() != "" && !proxyctl.PlaceholderPattern.MatchString(t.GetPlaceholderEnv()),
		slices.Contains(proxyctl.BerthOwned, t.GetPlaceholderEnv()):
		return nil, fmt.Errorf("grant %s: malformed placeholder variable", name)
	case len(t.GetMethods()) > 16:
		return nil, fmt.Errorf("grant %s: too many methods", name)
	}
	if _, err := path.Match(t.GetPathGlob(), ""); err != nil {
		return nil, fmt.Errorf("grant %s: malformed path glob", name)
	}
	for _, m := range t.GetMethods() {
		if !methodPattern.MatchString(m) {
			return nil, fmt.Errorf("grant %s: malformed method", name)
		}
	}
	port := int(t.GetPort())
	if port == 0 {
		port = 443
	}
	return &Grant{
		Name:        name,
		Host:        host,
		Port:        port,
		Header:      http.CanonicalHeaderKey(t.GetHeaderName()),
		Prefix:      []byte(t.GetValuePrefix()),
		Suffix:      []byte(t.GetValueSuffix()),
		PathGlob:    t.GetPathGlob(),
		Methods:     slices.Clone(t.GetMethods()),
		Placeholder: t.GetPlaceholderEnv(),
	}, nil
}
