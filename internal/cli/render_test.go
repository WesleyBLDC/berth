package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
	"berth/internal/client"
)

// hostile is text a process or an agent chose, with an escape that would recolour the terminal.
const hostile = "evil\x1b[31mred"

type fakeDebug struct {
	berthv1connect.UnimplementedDebugServiceHandler
}

func (fakeDebug) GetState(context.Context, *berthv1.GetStateRequest) (*berthv1.GetStateResponse, error) {
	return &berthv1.GetStateResponse{Entity: "session/1-1", Kind: "session", State: "Leftover", Version: 2, History: []*berthv1.Transition{
		{Version: 2, Event: hostile, FromState: "Active", ToState: "Leftover", Detail: hostile},
	}}, nil
}

type fakeAgents struct {
	berthv1connect.UnimplementedAgentServiceHandler
}

func (fakeAgents) ListAgentSessions(context.Context, *berthv1.ListAgentSessionsRequest) (*berthv1.ListAgentSessionsResponse, error) {
	return &berthv1.ListAgentSessionsResponse{Sessions: []*berthv1.AgentSession{{Id: strings.Repeat("a", 32), Name: hostile}}}, nil
}

func invokeAll(t *testing.T, args ...string) result {
	t.Helper()
	server := connect.NewServer()
	berthv1connect.RegisterDebugServiceHandler(server, fakeDebug{})
	berthv1connect.RegisterAgentServiceHandler(server, fakeAgents{})
	berthv1connect.RegisterSecretsServiceHandler(server, &fakeSecrets{list: &berthv1.ListSecretsResponse{Secrets: []*berthv1.SecretSummary{
		{Name: "gh-token", Consumers: []*berthv1.Consumer{{GrantId: 1, ExePath: "/tmp/" + hostile}}},
	}}})
	conn := connect.NewClient(connectinprocess.New(server))
	cl := &client.Client{
		Debug: berthv1connect.NewDebugServiceClient(conn), Agents: berthv1connect.NewAgentServiceClient(conn),
		Secrets: berthv1connect.NewSecretsServiceClient(conn),
	}
	var out, errOut bytes.Buffer
	code := Run(t.Context(), args, Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errOut,
		Connect: func(context.Context) (*client.Client, error) { return cl, nil },
	})
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// Every command that prints text a process or an agent chose prints it through shown.
func TestChosenTextIsShownEscaped(t *testing.T) {
	for _, args := range [][]string{{"debug", "state", "session/1-1"}, {"agent", "session", "ls"}, {"secret", "ls"}} {
		r := invokeAll(t, args...)
		if r.code != ExitOK || strings.Contains(r.stdout, "\x1b") || !strings.Contains(r.stdout, `\x1b[31m`) {
			t.Errorf("%v: code %d, stderr %q, stdout %q", args, r.code, r.stderr, r.stdout)
		}
	}
}
