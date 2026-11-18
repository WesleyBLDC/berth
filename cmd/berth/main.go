// berth is the command line client for berthd.
package main

import (
	"context"
	"os"

	"berth/internal/adapters/native"
	"berth/internal/cli"
	"berth/internal/client"
	"berth/internal/paths"
	"berth/internal/peertrust"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.Env{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Connect: func(ctx context.Context) (*client.Client, error) {
			if sock := os.Getenv("BERTH_AGENT_SOCKET"); sock != "" {
				token, err := client.ReadAgentToken(os.Getenv("BERTH_AGENT_TOKEN_FILE"))
				if err != nil {
					return nil, err
				}
				return client.ConnectAgent(ctx, sock, "berth-cli", token)
			}
			home, err := paths.Home()
			if err != nil {
				return nil, err
			}
			return client.Connect(ctx, paths.Socket(home), "berth-cli")
		},
		ConnectTrusted: connectTrusted,
		Read:           native.Reader(nativeHelper),
		StartProxy:     startProxy,
		Agent:          os.Getenv("BERTH_AGENT_SOCKET") != "",
		BeforeStart:    beforeStart,
	}))
}

// startProxy runs the berth-proxy beside berth. As with berth-native, the file does not decide
// trust: berthd authorizes only the signed proxy, judged by its live code identity.
func startProxy(ctx context.Context) (*cli.ProxyProcess, error) {
	path, err := proxyBinary()
	if err != nil {
		return nil, err
	}
	return cli.ExecProxy(ctx, path)
}

// connectTrusted refuses any server but the signed berthd beside this berth.
func connectTrusted(ctx context.Context) (*client.Client, error) {
	return peertrust.ConnectDaemon(ctx, "berth-cli", "berth")
}
