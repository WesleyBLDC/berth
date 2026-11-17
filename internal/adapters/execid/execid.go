// Package execid identifies an executable by reading the file and its code signature, so a
// grant pins what is on disk rather than what a caller claims.
package execid

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"berth/internal/core/ports"
)

// DefaultCodesign is the system codesign tool.
const DefaultCodesign = "/usr/bin/codesign"

// Identity implements ports.ExecIdentity.
type Identity struct {
	codesign string
}

// New returns an Identity that asks the codesign tool at path about signatures.
func New(codesign string) *Identity { return &Identity{codesign: codesign} }

var _ ports.ExecIdentity = (*Identity)(nil)

// Identify resolves path, hashes the file, and reads its signature. A missing file returns an
// error wrapping os.ErrNotExist; a file that is not signed, or whose signature does not verify,
// has empty TeamID and SigningID.
func (x *Identity) Identify(ctx context.Context, path string) (ports.Executable, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ports.Executable{}, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ports.Executable{}, fmt.Errorf("resolve executable: %w", err)
	}
	sum, err := hashFile(resolved)
	if err != nil {
		return ports.Executable{}, err
	}
	team, signing, err := x.signature(ctx, resolved)
	if err != nil {
		return ports.Executable{}, err
	}
	return ports.Executable{Path: resolved, SHA256: sum, TeamID: team, SigningID: signing}, nil
}

func hashFile(path string) ([32]byte, error) {
	f, err := os.Open(path) //nolint:gosec // identifying an arbitrary executable is this adapter's job
	if err != nil {
		return [32]byte{}, fmt.Errorf("open executable: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return [32]byte{}, fmt.Errorf("stat executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return [32]byte{}, fmt.Errorf("executable %s is not a regular file", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [32]byte{}, fmt.Errorf("read executable: %w", err)
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum, nil
}

// signature verifies the code signature before trusting what it names: codesign -dv alone
// prints the identifiers of a binary that was modified after signing.
func (x *Identity) signature(ctx context.Context, path string) (team, signing string, err error) {
	verify := exec.CommandContext(ctx, x.codesign, "--verify", "--strict", path) //nolint:gosec // fixed tool, path is an argument
	if err := verify.Run(); err != nil {
		if unsigned(ctx, err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("codesign verify: %w", err)
	}
	var stderr bytes.Buffer
	show := exec.CommandContext(ctx, x.codesign, "-d", "--verbose=2", path) //nolint:gosec // fixed tool, path is an argument
	show.Stderr = &stderr
	if err := show.Run(); err != nil {
		if unsigned(ctx, err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("codesign display: %w", err)
	}
	team, signing = parseDisplay(stderr.Bytes())
	return team, signing, nil
}

// Satisfies reports whether the file at path verifies against a code requirement, for example
// one that pins Apple's certificate chain and a team, which a self signed copy cannot fake the
// way it can fake the TeamIdentifier that codesign -d prints.
func (x *Identity) Satisfies(ctx context.Context, path, requirement string) (bool, error) {
	cmd := exec.CommandContext(ctx, x.codesign, "--verify", "--strict", "-R="+requirement, path) //nolint:gosec // fixed tool and requirement, path is an argument
	if err := cmd.Run(); err != nil {
		if unsigned(ctx, err) {
			return false, nil
		}
		return false, fmt.Errorf("codesign requirement: %w", err)
	}
	return true, nil
}

// unsigned reports whether codesign ran and said no; a cancelled context or a missing tool is a
// real error, so a broken environment never makes a signed binary look unsigned.
func unsigned(ctx context.Context, err error) bool {
	var exit *exec.ExitError
	return ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() > 0
}

// parseDisplay reads Identifier and TeamIdentifier from codesign -dv output. Platform and ad
// hoc signatures say "TeamIdentifier=not set", which is an empty team.
func parseDisplay(out []byte) (team, signing string) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "Identifier":
			signing = v
		case "TeamIdentifier":
			if v != "not set" {
				team = v
			}
		}
	}
	return team, signing
}
