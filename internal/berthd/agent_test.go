package berthd

import (
	"os"
	"testing"

	"berth/internal/paths"
)

func TestNewAgentSocketFlags(t *testing.T) {
	for _, c := range []struct {
		name     string
		uid, gid int
		path     string
		want     *AgentSocket
		wantErr  bool
	}{
		{name: "none set is off", uid: -1, gid: -1},
		{name: "default path", uid: 502, gid: 701, want: &AgentSocket{Path: paths.AgentSocket(os.Getuid()), UID: 502, GID: 701}},
		{name: "given path", uid: 502, gid: 701, path: "/tmp/a/agent.sock", want: &AgentSocket{Path: "/tmp/a/agent.sock", UID: 502, GID: 701}},
		{name: "root", uid: 0, gid: 701, wantErr: true},
		{name: "no gid", uid: 502, gid: -1, wantErr: true},
		{name: "gid without uid", uid: -1, gid: 701, wantErr: true},
		{name: "path without uid", uid: -1, gid: -1, path: "/tmp/a/agent.sock", wantErr: true},
		{name: "too big", uid: 1 << 33, gid: 701, wantErr: true},
	} {
		got, err := NewAgentSocket(c.uid, c.gid, c.path)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err %v, want error %v", c.name, err, c.wantErr)
			continue
		}
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}
