package proxysession_test

import (
	"slices"
	"testing"

	"berth/internal/core/proxysession"
)

// State and event names are stored data, so a rename must fail here first.
func TestNamesAreStable(t *testing.T) {
	var got []string
	for _, s := range proxysession.States {
		got = append(got, string(s))
	}
	for _, e := range proxysession.AllEvents {
		got = append(got, e.Name())
	}
	want := []string{"Authorized", "Serving", "Stopping", "Ended", "Lost", "ProxyReady", "StopRequested", "ProxyEnded", "ProxyLost"}
	if !slices.Equal(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	if proxysession.Entity("ab") != "proxy/ab" || proxysession.Initial != proxysession.Authorized {
		t.Fatal("entity or initial state changed")
	}
}
