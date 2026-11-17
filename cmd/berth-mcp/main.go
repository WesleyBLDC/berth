// berth-mcp is an MCP server over stdio for agents without a shell. It reaches berthd's agent
// socket (BERTH_AGENT_SOCKET) with the session token from the file BERTH_AGENT_TOKEN_FILE names,
// as berth does in agent mode, so the token never enters the model's context.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"berth/internal/client"
	"berth/internal/mcp"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "berth-mcp:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	sock := os.Getenv("BERTH_AGENT_SOCKET")
	if sock == "" {
		return fmt.Errorf("BERTH_AGENT_SOCKET is not set; berth-mcp serves agents through berthd's agent socket")
	}
	token, err := client.ReadAgentToken(os.Getenv("BERTH_AGENT_TOKEN_FILE"))
	if err != nil {
		return err
	}
	c, err := client.ConnectAgent(ctx, sock, "berth-mcp", token)
	if err != nil {
		return err
	}
	s := &mcp.Server{Browsers: c.Browsers, Secrets: c.Secrets, TokenDir: os.TempDir(), Version: version}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, os.Stdin, os.Stdout) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// A read of stdin cannot be interrupted, so a signal removes the token files and exits.
		s.Close()
		return nil
	}
}
