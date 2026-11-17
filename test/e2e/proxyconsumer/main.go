// Command proxyconsumer is a compiled stand in for a tool berth run --proxy starts in the end to
// end tests. It holds no secret: it only gets the proxy's address, credential and CA files, and
// a placeholder.
//
//	proxyconsumer canary <A> <B> <C>   one round of requests, a CONNECT to B, then its environment
//	proxyconsumer loop <A>             requests every 100 ms until one fails after a success
//
// Each address is host:port. Go on macOS ignores SSL_CERT_FILE, so the consumer loads it itself,
// plus E2E_EXTRA_CA (the test root, which B's certificate chains to).
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: proxyconsumer canary <A> <B> <C> | loop <A>")
		os.Exit(2)
	}
	client, proxyURL, err := newClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "proxyconsumer:", err)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "canary":
		if len(os.Args) != 5 {
			os.Exit(2)
		}
		canary(client, proxyURL, os.Args[2], os.Args[3], os.Args[4])
	case "loop":
		os.Exit(loop(client, os.Args[2]))
	default:
		os.Exit(2)
	}
}

func newClient() (*http.Client, *url.URL, error) {
	pool := x509.NewCertPool()
	for _, v := range []string{"SSL_CERT_FILE", "E2E_EXTRA_CA"} {
		pem, err := os.ReadFile(os.Getenv(v)) //nolint:gosec // the CA files berth run and the test name
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", v, err)
		}
		pool.AppendCertsFromPEM(pem)
	}
	// http.ProxyFromEnvironment never proxies localhost, so the consumer reads the variable itself.
	proxyURL, err := url.Parse(os.Getenv("HTTPS_PROXY"))
	if err != nil {
		return nil, nil, err
	}
	transport := &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}, proxyURL, nil
}

// get sends the placeholder as the header the proxy owns on the granted host.
func get(client *http.Client, u string) string {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil) //nolint:gosec // the test servers the test names
	if err != nil {
		return "error " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("E2E_KEY"))
	resp, err := client.Do(req) //nolint:gosec // as above
	if err != nil {
		return "error " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return fmt.Sprint(resp.StatusCode)
}

func canary(client *http.Client, proxyURL *url.URL, a, b, c string) {
	fmt.Println("A", get(client, "https://"+a+"/a"))
	fmt.Println("B", get(client, "https://"+b+"/b"))
	fmt.Println("PLAIN", get(client, "http://"+a+"/plain"))
	fmt.Println("C", get(client, "https://"+c+"/c"))
	fmt.Println("WRONG", connectStatus(proxyURL, a, "berth:wrong"))
	fmt.Println("NOAUTH", connectStatus(proxyURL, a, ""))
	fmt.Println("OTHER", connectStatus(proxyURL, b, proxyURL.User.String()))
	env, _ := json.Marshal(os.Environ())
	fmt.Println("ENV", string(env))
}

// connectStatus sends one CONNECT with the given user:password and returns the status code.
func connectStatus(proxyURL *url.URL, target, userinfo string) string {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(context.Background(), "tcp", proxyURL.Host)
	if err != nil {
		return "error " + err.Error()
	}
	defer func() { _ = conn.Close() }()
	head := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if userinfo != "" {
		head += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(userinfo)) + "\r\n"
	}
	if _, err := conn.Write([]byte(head + "\r\n")); err != nil {
		return "error " + err.Error()
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "error " + err.Error()
	}
	if f := strings.Fields(line); len(f) >= 2 {
		return f[1]
	}
	return "error " + line
}

func loop(client *http.Client, a string) int {
	ok := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		got := get(client, "https://"+a+"/loop")
		fmt.Println(got)
		if got == "200" {
			ok = true
		} else if ok {
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 1
}
