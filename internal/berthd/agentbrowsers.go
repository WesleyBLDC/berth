package berthd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	berthv1 "berth/gen/berth/v1"
	"berth/internal/adapters/api"
	"berth/internal/core/agentsession"
	"berth/internal/core/browser"
)

// Provisional limits, awaiting the maintainer (docs/plans/m4-managed-chromium.md, item 7). Each
// agent browser is a Chromium of the owner's on a fresh profile, so these bound the processes
// and disk an agent session can make the owner run. An agent that needs more pages opens new
// contexts in a browser it has, which costs nothing here.
const (
	maxAgentBrowsers     = 2
	maxAgentOpensPerHour = 10
	agentProfilePrefix   = "agent-"
)

// agentProfileName matches only the profiles agent browsers get, so boot never deletes another.
var agentProfileName = regexp.MustCompile(`^agent-[0-9a-f]{32}-[0-9]+$`)

// OpenAgentBrowser launches a headless browser for the caller's agent session on a fresh profile
// agent-<session>-<n>, deleted when the browser closes. It lives until CloseAgentBrowser or the
// agent session's end, not the call's: agents crash and reconnect, and a crashed harness ends its
// session through the root watch, which closes every browser the session held.
func (m *managed) OpenAgentBrowser(ctx context.Context, _ *berthv1.OpenAgentBrowserRequest) (*berthv1.OpenAgentBrowserResponse, error) {
	agent := api.AgentSession(ctx)
	if agent == "" || !m.agentLive(ctx, agent) {
		return nil, api.ErrAgentTokenInvalid
	}
	// Agent browsers never get a named executable: berthd's own pick only.
	exe, err := m.executable("")
	if err != nil {
		return nil, err
	}
	// Headless until M5, since a window on the person's screen is not the agent's to drive.
	b, token := m.newBrowser("", exe, true, agent)
	err = m.start(ctx, b, func() error { return m.admitAgent(b) })
	if errors.Is(err, api.ErrRateLimited) {
		m.Log.Warn("refused an agent browser over the session's limit", "agent_session", agent)
	}
	if err != nil {
		return nil, err
	}
	m.Log.Info("opened an agent browser", "agent_session", agent, "browser", b.id)
	return &berthv1.OpenAgentBrowserResponse{Browser: m.agentProto(b), CdpToken: token}, nil
}

// admitAgent counts an open against the session's limits and names its profile; it runs under
// m.mu, so two opens cannot both take the last slot. A failed launch still counts.
func (m *managed) admitAgent(b *managedBrowser) error {
	open := 0
	for _, other := range m.live {
		if other.agent == b.agent {
			open++
		}
	}
	ok, kept := agentsession.Limit{Open: maxAgentBrowsers, PerHour: maxAgentOpensPerHour}.Admit(open, m.opened[b.agent], m.Clock.Now())
	m.opened[b.agent] = kept
	if !ok {
		return api.ErrRateLimited
	}
	// After a restart the numbers start again, so skip a folder a previous berthd left behind.
	for {
		m.seq[b.agent]++
		b.profile = fmt.Sprintf("%s%s-%d", agentProfilePrefix, b.agent, m.seq[b.agent])
		if _, err := os.Lstat(filepath.Join(m.profilesRoot(), b.profile)); errors.Is(err, os.ErrNotExist) && m.profiles[b.profile] == "" {
			return nil
		}
	}
}

// CloseAgentBrowser closes one of the caller's agent session's browsers. Any other browser
// answers as an unknown id, so an agent cannot learn which ids exist.
func (m *managed) CloseAgentBrowser(ctx context.Context, req *berthv1.CloseAgentBrowserRequest) (*berthv1.CloseAgentBrowserResponse, error) {
	agent := api.AgentSession(ctx)
	b := m.find(req.GetId())
	if agent == "" || b == nil || b.agent != agent {
		return nil, api.ErrBrowserNotFound
	}
	b, err := m.closeAndWait(ctx, b)
	if err != nil {
		return nil, err
	}
	return &berthv1.CloseAgentBrowserResponse{Browser: m.agentProto(b)}, nil
}

// ListAgentBrowsers lists the caller's agent session's live browsers, and no other.
func (m *managed) ListAgentBrowsers(ctx context.Context) (*berthv1.ListAgentBrowsersResponse, error) {
	agent := api.AgentSession(ctx)
	m.mu.Lock()
	var mine []*managedBrowser
	for _, b := range m.live {
		if agent != "" && b.agent == agent {
			mine = append(mine, b)
		}
	}
	m.mu.Unlock()
	resp := &berthv1.ListAgentBrowsersResponse{}
	for _, b := range mine {
		resp.Browsers = append(resp.Browsers, m.agentProto(b))
	}
	slices.SortFunc(resp.Browsers, func(x, y *berthv1.AgentBrowser) int { return strings.Compare(x.GetId(), y.GetId()) })
	return resp, nil
}

// closeAgent closes every browser of an agent session that ended. It runs on berthd's context,
// from the agent session's saved hook, and does not wait for the browsers to close.
func (m *managed) closeAgent(ctx context.Context, agent string) {
	m.mu.Lock()
	var ids []string
	for id, b := range m.live {
		if b.agent == agent {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	m.forgetAgent(agent)
	for _, id := range ids {
		// A browser still in Requested may have no row and takes no event; its launch closes
		// it once it starts.
		if st := m.state(id); st != browser.Closing && st != browser.Requested {
			m.send(ctx, id, browser.Event{Kind: browser.CloseRequested, Note: "agent session ended"})
		}
	}
	if len(ids) > 0 {
		m.Log.Info("closing the browsers of an agent session that ended", "agent_session", agent, "count", len(ids))
	}
}

// forgetAgent drops an ended session's limit counts; an open racing the end that recreated them
// calls it again once its launch settles.
func (m *managed) forgetAgent(agent string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.opened, agent)
	delete(m.seq, agent)
}

func (m *managed) agentLive(ctx context.Context, agent string) bool {
	return m.AgentLive == nil || m.AgentLive(ctx, agent)
}

// removeProfile deletes an agent browser's profile; the name was made by admitAgent, so the
// folder is always one of Berth's own under profiles.
func (m *managed) removeProfile(name string) {
	if !agentProfileName.MatchString(name) {
		return
	}
	if err := os.RemoveAll(filepath.Join(m.profilesRoot(), name)); err != nil {
		m.Log.Warn("could not delete an agent browser's profile", "profile", name, "err", err)
	}
}

// removeLeftAgentProfiles deletes, at boot, the agent profiles a previous berthd left; one that a
// browser still uses is left for the next boot.
func (m *managed) removeLeftAgentProfiles() {
	entries, err := os.ReadDir(m.profilesRoot())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && agentProfileName.MatchString(e.Name()) && !m.Seen.ProfileInUse(filepath.Join(m.profilesRoot(), e.Name())) {
			m.removeProfile(e.Name())
		}
	}
}

func (m *managed) agentProto(b *managedBrowser) *berthv1.AgentBrowser {
	m.mu.Lock()
	defer m.mu.Unlock()
	return &berthv1.AgentBrowser{
		Id: b.id, State: browserStateProto(b.state), Product: b.product, CdpEndpoint: b.endpoint,
		StateSince: timestamppb.New(b.since), CloseReason: b.reason,
	}
}
