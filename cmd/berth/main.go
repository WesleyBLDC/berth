// berth is the command line client for berthd.
package main

import (
	"context"
	"fmt"
	"os"

	"berth/internal/adapters/native"
	"berth/internal/cli"
	"berth/internal/client"
	"berth/internal/core/ports"
	"berth/internal/paths"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.Env{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Connect: func(ctx context.Context) (*client.Client, error) {
			home, err := paths.Home()
			if err != nil {
				return nil, err
			}
			return client.Connect(ctx, paths.Socket(home), "berth-cli")
		},
		Read: read,
	}))
}

// read hands the ticket to berth-native. BERTH_NATIVE only picks which helper berth starts; it
// loosens nothing, since berthd redeems a ticket only for the signed helper.
func read(ctx context.Context, ticket []byte) ([]byte, error) {
	var t ports.Ticket
	if len(ticket) != len(t) {
		return nil, fmt.Errorf("berthd sent a %d byte ticket", len(ticket))
	}
	copy(t[:], ticket)
	helper := os.Getenv("BERTH_NATIVE")
	if helper == "" {
		var err error
		if helper, err = paths.Beside("berth-native"); err != nil {
			return nil, err
		}
	}
	return native.Read(ctx, helper, t)
}
