package hostpolicy_test

import (
	"testing"

	"berth/internal/core/hostpolicy"
)

func TestLocal(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "api.localhost": true, "127.0.0.1": true, "127.1": true, "10.0.0.8": true, "::1": true, "2130706433": true,
		"api.example.com": false, "localhost.example.com": false, "a1.example.com": false, "api.example.c0m": false,
	} {
		if got := hostpolicy.Local(host); got != want {
			t.Errorf("Local(%q) = %v, want %v", host, got, want)
		}
	}
}
