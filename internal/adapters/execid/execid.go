// Package execid identifies an executable by reading the file and its code signature, so a
// grant pins what is on disk rather than what a caller claims.
package execid

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// Identify resolves path, hashes the file, and reads its signature and cdhash. A missing file
// returns an error wrapping os.ErrNotExist; a file that is not signed, or whose signature does not
// verify, has SignatureUnsigned and empty TeamID and SigningID. A #! script's CDHash is its interpreter's, since that is
// the image the kernel runs; a file with no code directory has a zero CDHash.
func (x *Identity) Identify(ctx context.Context, path string) (ports.Executable, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ports.Executable{}, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ports.Executable{}, fmt.Errorf("resolve executable: %w", err)
	}
	sum, interpreter, shebang, err := hashFile(resolved)
	if err != nil {
		return ports.Executable{}, err
	}
	team, signing, signature, err := x.signature(ctx, resolved)
	if err != nil {
		return ports.Executable{}, err
	}
	image := resolved
	if shebang {
		image = interpreter
	}
	var cdhash ports.CDHash
	if image != "" {
		if cdhash, err = x.CDHash(ctx, image); err != nil {
			cdhash = ports.CDHash{}
		}
	}
	return ports.Executable{Path: sendable(resolved), SHA256: sum, TeamID: team, SigningID: signing, Signature: signature, Shebang: shebang, CDHash: cdhash}, nil
}

// sendable is a path as grant records and audit notes keep it: protobuf must send those, so
// invalid UTF-8, which APFS refuses but another volume may hold, shows as U+FFFD. The hashes,
// not the path, pin the file.
func sendable(path string) string { return strings.ToValidUTF8(path, "\uFFFD") }

// hashFile hashes the file and, for a #! script, returns the interpreter path its first line
// names (empty unless absolute).
func hashFile(path string) (sum [32]byte, interpreter string, shebang bool, err error) {
	f, err := os.Open(path) //nolint:gosec // identifying an arbitrary executable is this adapter's job
	if err != nil {
		return sum, "", false, fmt.Errorf("open executable: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return sum, "", false, fmt.Errorf("stat executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return sum, "", false, fmt.Errorf("executable %s is not a regular file", path)
	}
	h := sha256.New()
	var head headWriter
	if _, err := io.Copy(io.MultiWriter(h, &head), f); err != nil {
		return sum, "", false, fmt.Errorf("read executable: %w", err)
	}
	h.Sum(sum[:0])
	line, ok := bytes.CutPrefix(head.b[:min(head.n, len(head.b))], []byte("#!"))
	if !ok {
		return sum, "", false, nil
	}
	line, _, _ = bytes.Cut(line, []byte("\n"))
	if fields := strings.Fields(string(line)); len(fields) > 0 && filepath.IsAbs(fields[0]) {
		interpreter = fields[0]
	}
	return sum, interpreter, true, nil
}

// headWriter keeps the first bytes written to it, enough for the #! line the kernel reads.
type headWriter struct {
	b [512]byte
	n int
}

func (w *headWriter) Write(p []byte) (int, error) {
	w.n += copy(w.b[min(w.n, len(w.b)):], p)
	return len(p), nil
}

// CDHash returns the code directory hash codesign reports for the file's native slice, which
// is what the kernel reports for a process running it.
func (x *Identity) CDHash(ctx context.Context, path string) (ports.CDHash, error) {
	var stderr bytes.Buffer
	show := exec.CommandContext(ctx, x.codesign, "-d", "--verbose=4", path) //nolint:gosec // fixed tool, path is an argument
	show.Stderr = &stderr
	if err := show.Run(); err != nil {
		return ports.CDHash{}, fmt.Errorf("codesign display: %w", err)
	}
	sc := bufio.NewScanner(&stderr)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "CDHash="); ok {
			raw, err := hex.DecodeString(v)
			var h ports.CDHash
			if err != nil || len(raw) != len(h) {
				return ports.CDHash{}, fmt.Errorf("codesign printed a malformed CDHash for %s", path)
			}
			copy(h[:], raw)
			return h, nil
		}
	}
	return ports.CDHash{}, fmt.Errorf("%s has no code directory hash", path)
}

// signature verifies the code signature before trusting what it names: codesign -dv alone
// prints the identifiers of a binary that was modified after signing.
func (x *Identity) signature(ctx context.Context, path string) (team, signing string, kind ports.Signature, err error) {
	verify := exec.CommandContext(ctx, x.codesign, "--verify", "--strict", path) //nolint:gosec // fixed tool, path is an argument
	if err := verify.Run(); err != nil {
		if unsigned(ctx, err) {
			return "", "", ports.SignatureUnsigned, nil
		}
		return "", "", "", fmt.Errorf("codesign verify: %w", err)
	}
	var stderr bytes.Buffer
	show := exec.CommandContext(ctx, x.codesign, "-d", "--verbose=2", path) //nolint:gosec // fixed tool, path is an argument
	show.Stderr = &stderr
	if err := show.Run(); err != nil {
		if unsigned(ctx, err) {
			return "", "", ports.SignatureUnsigned, nil
		}
		return "", "", "", fmt.Errorf("codesign display: %w", err)
	}
	team, signing, adhoc := parseDisplay(stderr.Bytes())
	switch {
	case team != "":
		return team, signing, ports.SignatureTeam, nil
	case adhoc:
		return "", signing, ports.SignatureAdHoc, nil
	}
	// Authority names are certificate subjects anyone can copy into a self signed certificate, so
	// Apple is recognised by its anchor, never by the "Software Signing" name codesign prints.
	apple, err := x.Satisfies(ctx, path, "anchor apple")
	if err != nil {
		return "", "", "", err
	}
	if apple {
		return "", signing, ports.SignatureApple, nil
	}
	return "", signing, ports.SignatureOther, nil
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

// parseDisplay reads Identifier, TeamIdentifier and the ad hoc marker from codesign -dv output.
// Platform and ad hoc signatures say "TeamIdentifier=not set", which is an empty team.
func parseDisplay(out []byte) (team, signing string, adhoc bool) {
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
		case "Signature":
			adhoc = v == "adhoc"
		}
	}
	return team, signing, adhoc
}

// PinSigned copies the file at path into a private folder, checks the copy against
// requirement and reads the copy's cdhash, so the check and the hash see the same bytes. The
// copy is not safe from a same-user process swapping the file before it is copied: a swapped
// pin can at worst make Berth trust a fake CLI or helper, and berth-native's receiver check
// (the value goes only to its parent, running the signed berth) stops that fake from getting
// a value. ok is false when the copy does not satisfy the requirement.
func (x *Identity) PinSigned(ctx context.Context, path, requirement string) (sum ports.CDHash, ok bool, err error) {
	err = x.withPrivateCopy(path, func(cp string) error {
		if ok, err = x.Satisfies(ctx, cp, requirement); err != nil || !ok {
			return err
		}
		sum, err = x.CDHash(ctx, cp)
		return err
	})
	return sum, ok && err == nil, err
}

// withPrivateCopy copies path to a 0600 file in a fresh 0700 folder, runs do on it, and removes
// both; see PinSigned for what the copy does and does not protect.
func (*Identity) withPrivateCopy(path string, do func(cp string) error) error {
	dir, err := os.MkdirTemp("", "berth-pin-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	src, err := os.Open(path) //nolint:gosec // the trusted Berth binaries beside the running one
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = src.Close() }()
	cp := filepath.Join(dir, filepath.Base(path))
	dst, err := os.OpenFile(cp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // a fresh private folder
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return do(cp)
}
