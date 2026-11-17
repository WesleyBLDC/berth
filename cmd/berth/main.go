// berth is the command line client for berthd.
package main

import (
	"context"
	"os"

	"berth/internal/cli"
	"berth/internal/client"
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
	}))
}
