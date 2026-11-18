package execid

import (
	"context"
	"debug/macho"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"berth/internal/core/ports"
)

// copyLimits bound what one pin copies, since a same user process can put anything where the
// trusted binaries live. A variable so tests can lower it.
var copyLimits = struct {
	bytes int64
	files int
	depth int
}{bytes: 512 << 20, files: 10_000, depth: 32}

// pinFolderPrefix names the folders pins copy into, under the temporary folder.
const pinFolderPrefix = "berth-pin-"

// PinSigned returns the cdhash of every architecture slice of a Berth binary that satisfies
// requirement, and false when any check fails.
//
// It copies the file, or for an executable in X.app/Contents/MacOS (which its bundle seals, so it
// cannot be checked alone) the whole bundle, into a new 0700 folder, and checks the copy against
// requirement on every slice. For a bundle the executable must also be the one Info.plist names.
// The copy's folder is the user's own, so a same user process could replace the copy after it is
// checked; each slice's cdhash is therefore read and then checked again in one codesign call
// against requirement and that exact cdhash, so only a cdhash whose code satisfies requirement is
// ever pinned, whatever happens to the copy in between.
//
// The source can still be swapped before it is copied: a swapped pin then names other code that
// also satisfies requirement (signed by Berth's team in release builds).
func (x *Identity) PinSigned(ctx context.Context, path, requirement string) (sums []ports.CDHash, ok bool, err error) {
	check := func(signed, exe string) error {
		if ok, err = x.satisfies(ctx, signed, requirement, ""); err != nil || !ok {
			return err
		}
		if x.beforeRead != nil {
			x.beforeRead(exe)
		}
		sums, ok, err = x.boundSlices(ctx, signed, exe, requirement)
		return err
	}
	if bundle, inner, isApp := appBundleOf(path); isApp {
		err = withPrivateBundle(ctx, bundle, func(cp string) error {
			if named, err := bundleExecutable(ctx, cp); err != nil || named != filepath.Base(inner) {
				ok = false
				if err == nil {
					err = fmt.Errorf("%s names %q as its executable, not %q", bundle, named, filepath.Base(inner))
				}
				return err
			}
			return check(cp, filepath.Join(cp, inner))
		})
	} else {
		err = withPrivateCopy(ctx, path, func(cp string) error { return check(cp, cp) })
	}
	if err != nil || !ok {
		return nil, false, err
	}
	return sums, true, nil
}

// boundSlices reads each slice's cdhash from exe and keeps it only if signed, checked on that
// slice in one call, satisfies requirement and has that cdhash.
func (x *Identity) boundSlices(ctx context.Context, signed, exe, requirement string) ([]ports.CDHash, bool, error) {
	archs, err := sliceArchs(exe)
	if err != nil {
		return nil, false, err
	}
	sums := make([]ports.CDHash, 0, len(archs))
	for _, arch := range archs {
		h, err := x.sliceCDHash(ctx, exe, arch)
		if err != nil {
			return nil, false, err
		}
		bound := "(" + requirement + `) and cdhash H"` + hex.EncodeToString(h[:]) + `"`
		ok, err := x.satisfies(ctx, signed, bound, arch)
		if err != nil || !ok {
			return nil, false, err
		}
		sums = append(sums, h)
	}
	return sums, true, nil
}

// sliceArchs names each architecture slice of a Mach-O file, one for a thin file.
func sliceArchs(path string) ([]string, error) {
	fat, err := macho.OpenFat(path)
	if errors.Is(err, macho.ErrNotFat) {
		thin, err := macho.Open(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		defer func() { _ = thin.Close() }()
		name, err := archName(thin.Cpu, thin.SubCpu)
		return []string{name}, err
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = fat.Close() }()
	names := make([]string, 0, len(fat.Arches))
	for _, a := range fat.Arches {
		name, err := archName(a.Cpu, a.SubCpu)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

// archName is codesign's name for a slice; a slice Berth cannot name is refused, not skipped.
func archName(cpu macho.Cpu, sub uint32) (string, error) {
	const subtypeMask, arm64e, x8664h = 0x00ffffff, 2, 8
	switch {
	case cpu == macho.CpuArm64 && sub&subtypeMask == arm64e:
		return "arm64e", nil
	case cpu == macho.CpuArm64:
		return "arm64", nil
	case cpu == macho.CpuAmd64 && sub&subtypeMask == x8664h:
		return "x86_64h", nil
	case cpu == macho.CpuAmd64:
		return "x86_64", nil
	}
	return "", fmt.Errorf("a slice of CPU type %v that Berth cannot name", cpu)
}

// appBundleOf finds the bundle an executable in X.app/Contents/MacOS belongs to, and the
// executable's path inside it.
func appBundleOf(path string) (bundle, inner string, ok bool) {
	macOS := filepath.Dir(path)
	contents := filepath.Dir(macOS)
	bundle = filepath.Dir(contents)
	if filepath.Base(macOS) != "MacOS" || filepath.Base(contents) != "Contents" || filepath.Ext(bundle) != ".app" {
		return "", "", false
	}
	return bundle, filepath.Join("Contents", "MacOS", filepath.Base(path)), true
}

// bundleExecutable reads CFBundleExecutable from a bundle's Info.plist.
func bundleExecutable(ctx context.Context, bundle string) (string, error) {
	plist := filepath.Join(bundle, "Contents", "Info.plist")
	out, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", "CFBundleExecutable", "raw", "-o", "-", plist).Output() //nolint:gosec // fixed tool, the plist is an argument
	if err != nil {
		return "", fmt.Errorf("read CFBundleExecutable: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// copyBudget counts what one pin has copied against copyLimits.
type copyBudget struct {
	bytes int64
	files int
}

func (b *copyBudget) take(size int64) error {
	b.files++
	b.bytes += size
	if b.files > copyLimits.files || b.bytes > copyLimits.bytes {
		return fmt.Errorf("more than %d files or %d bytes to copy", copyLimits.files, copyLimits.bytes)
	}
	return nil
}

// withPrivateBundle copies a bundle into a new 0700 folder, runs do on the copy, and removes it.
// Every entry is opened relative to its already opened folder without following a symlink, so
// a folder replaced by a symlink during the copy is refused rather than followed; only folders
// and regular files are copied, with their permission bits.
func withPrivateBundle(ctx context.Context, bundle string, do func(cp string) error) error {
	return inPinFolder(func(dir string) error {
		cp := filepath.Join(dir, filepath.Base(bundle))
		fd, err := unix.Open(bundle, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", bundle, err)
		}
		if err := copyFolder(ctx, fd, cp, 0, &copyBudget{}); err != nil {
			return fmt.Errorf("copy %s: %w", bundle, err)
		}
		return do(cp)
	})
}

// copyFolder copies the open folder fd to dst, taking ownership of fd.
func copyFolder(ctx context.Context, fd int, dst string, depth int, budget *copyBudget) error {
	dir := os.NewFile(uintptr(fd), dst)
	defer func() { _ = dir.Close() }()
	if depth > copyLimits.depth {
		return fmt.Errorf("folders nested deeper than %d", copyLimits.depth)
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", name, err)
		}
		var st unix.Stat_t
		if err := unix.Fstat(child, &st); err != nil {
			_ = unix.Close(child)
			return err
		}
		target := filepath.Join(dst, name)
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if err := copyFolder(ctx, child, target, depth+1, budget); err != nil {
				return err
			}
		case unix.S_IFREG:
			if err := copyOpenFile(ctx, child, target, st, budget); err != nil {
				return err
			}
		default:
			_ = unix.Close(child)
			return fmt.Errorf("%s is neither a folder nor a regular file", name)
		}
	}
	return nil
}

// copyOpenFile copies the open regular file fd to a new file at dst, taking ownership of fd.
func copyOpenFile(ctx context.Context, fd int, dst string, st unix.Stat_t, budget *copyBudget) error {
	src := os.NewFile(uintptr(fd), dst)
	defer func() { _ = src.Close() }()
	if err := budget.take(st.Size); err != nil {
		return err
	}
	// A file may only keep its owner's execute bits; never setuid, setgid or writable by others.
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(st.Mode)&0o755) //nolint:gosec // a fresh private folder
	if err != nil {
		return err
	}
	// The size read by fstat bounds the copy, so a file that grows while it is copied cannot pass
	// the budget.
	if _, err := io.Copy(out, io.LimitReader(ctxReader{ctx, src}, st.Size)); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ctxReader stops a copy when its context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// withPrivateCopy copies one regular file into a new 0700 folder, runs do on the copy, and
// removes it; the source is opened without following a symlink.
func withPrivateCopy(ctx context.Context, path string, do func(cp string) error) error {
	return inPinFolder(func(dir string) error {
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			_ = unix.Close(fd)
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			_ = unix.Close(fd)
			return fmt.Errorf("%s is not a regular file", path)
		}
		cp := filepath.Join(dir, filepath.Base(path))
		if err := copyOpenFile(ctx, fd, cp, st, &copyBudget{}); err != nil {
			return err
		}
		return do(cp)
	})
}

func inPinFolder(do func(dir string) error) error {
	dir, err := os.MkdirTemp("", pinFolderPrefix)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	return do(dir)
}

// RemoveLeftoverPins removes pin folders a process that died mid pin left in the temporary
// folder. Only the running user's folders older than age go, since berth and berth-proxy pin
// berthd at any time.
func RemoveLeftoverPins(age time.Duration, now time.Time) error {
	return removeLeftoverPins(age, now, uint32(os.Getuid())) //nolint:gosec // user IDs fit in 32 bits
}

func removeLeftoverPins(age time.Duration, now time.Time, uid uint32) error {
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), pinFolderPrefix+"*"))
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range matches {
		info, err := os.Lstat(m)
		if err != nil || !info.IsDir() || now.Sub(info.ModTime()) < age {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != uid {
			continue
		}
		errs = append(errs, os.RemoveAll(m))
	}
	return errors.Join(errs...)
}
