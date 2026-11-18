package berthd

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	"berth/internal/core/session"
)

// ProfileInUse reports whether a live browser the scan saw uses dir as its profile folder,
// comparing cleaned absolute paths with symlinks resolved on both sides. A process chose those
// paths, and resolving one can block on a slow or hung mount, so it happens outside b.mu.
func (b *browsers) ProfileInUse(dir string) bool {
	want := profilePath(dir)
	if want == "" {
		return false
	}
	b.mu.Lock()
	var dirs []string
	for _, t := range b.live {
		if t.disc.State != session.Stopping {
			dirs = append(dirs, t.obs.Signature.UserDataDir)
		}
	}
	b.mu.Unlock()
	return slices.ContainsFunc(dirs, func(d string) bool { return profilePath(d) == want })
}

// StopUnder stops every live browser of this user whose profile folder is inside root, as a
// forced stop, and returns how many it asked to stop. Boot recovery uses it for the browsers a
// previous berthd launched on Berth's profiles and lost.
func (b *browsers) StopUnder(ctx context.Context, root string) int {
	prefix := profilePath(root)
	if prefix == "" {
		return 0
	}
	prefix = strings.TrimSuffix(prefix, "/") + "/"
	b.mu.Lock()
	dirs := map[string]string{}
	for id, t := range b.live {
		if !t.obs.OtherUser {
			dirs[id] = t.obs.Signature.UserDataDir
		}
	}
	b.mu.Unlock()
	n := 0
	for id, dir := range dirs {
		if strings.HasPrefix(profilePath(dir), prefix) {
			b.send(ctx, id, session.Event{Kind: session.StopRequested, Note: "left on a Berth profile"})
			n++
		}
	}
	return n
}

// resolve is filepath.EvalSymlinks, replaced in tests.
var resolve = filepath.EvalSymlinks

// profilePath is a profile folder as a cleaned absolute path with symlinks resolved where the
// folder exists, or "" for a relative path, whose base only its own process knows.
func profilePath(p string) string {
	if !filepath.IsAbs(p) {
		return ""
	}
	p = filepath.Clean(p)
	if real, err := resolve(p); err == nil {
		return real
	}
	return p
}
