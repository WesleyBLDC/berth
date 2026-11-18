package execid

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const appID = "com.wesleybldc.berth"

var appRequirement = `identifier "` + appID + `"`

func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "berth-pintest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func codesign(t *testing.T, identifier, path string, extra ...string) {
	t.Helper()
	args := append([]string{"--force", "-s", "-", "--options", "runtime", "-i", identifier}, extra...)
	if out, err := exec.CommandContext(t.Context(), DefaultCodesign, append(args, path)...).CombinedOutput(); err != nil {
		t.Fatalf("codesign %s: %v\n%s", path, err, out)
	}
}

// signedCopy writes a copy of the universal /usr/bin/true to path, signed ad hoc with the
// hardened runtime as identifier.
func signedCopy(t *testing.T, path, identifier string) {
	t.Helper()
	bin, err := os.ReadFile("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bin, 0o755); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	codesign(t, identifier, path)
}

// signedBundle builds Berth.app with an Info.plist naming named, the executable Contents/MacOS/Berth
// and a resource, signed ad hoc with the hardened runtime as scripts/build-app.sh signs it, and
// returns the executable's path. When named is not Berth, Contents/MacOS/<named> is the main
// executable and Berth a copy of it.
func signedBundle(t *testing.T, named string) string {
	t.Helper()
	app := filepath.Join(tempDir(t), "Berth.app")
	macOS := filepath.Join(app, "Contents", "MacOS")
	resources := filepath.Join(app, "Contents", "Resources")
	for _, d := range []string{macOS, resources} {
		if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // a bundle fixture
			t.Fatal(err)
		}
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleExecutable</key><string>` + named + `</string>
<key>CFBundleIdentifier</key><string>` + appID + `</string>
<key>CFBundlePackageType</key><string>APPL</string>
</dict></plist>
`
	for path, b := range map[string]string{filepath.Join(app, "Contents", "Info.plist"): plist, filepath.Join(resources, "note.txt"): "sealed"} {
		if err := os.WriteFile(path, []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	signedCopy(t, filepath.Join(macOS, named), appID)
	if named != "Berth" {
		// The same signed bytes, so only the Info.plist check tells the two apart.
		b, err := os.ReadFile(filepath.Join(macOS, named))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(macOS, "Berth"), b, 0o755); err != nil { //nolint:gosec // an executable fixture
			t.Fatal(err)
		}
	}
	codesign(t, appID, app)
	return filepath.Join(macOS, "Berth")
}

// A bundle signed executable cannot be checked alone ("invalid Info.plist"), so its whole bundle
// is checked and the executable's slices are pinned from the checked copy.
func TestPinSignedChecksAnAppBundle(t *testing.T) {
	exe := signedBundle(t, "Berth")
	id := New(DefaultCodesign)
	native, err := id.CDHash(t.Context(), exe)
	if err != nil {
		t.Fatal(err)
	}
	sums, ok, err := id.PinSigned(t.Context(), exe, appRequirement)
	if err != nil || !ok || len(sums) < 2 || !slices.Contains(sums, native) {
		t.Fatalf("pin %x %v %v, want every slice with %x", sums, ok, err, native)
	}
	if _, ok, _ := id.PinSigned(t.Context(), exe, `identifier "com.wesleybldc.berth.cli"`); ok {
		t.Fatal("pinned against another identifier")
	}
	note := filepath.Join(filepath.Dir(filepath.Dir(exe)), "Resources", "note.txt")
	if err := os.WriteFile(note, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := id.PinSigned(t.Context(), exe, appRequirement); ok {
		t.Fatal("pinned a bundle whose resource changed")
	}
}

// Only the executable the bundle's Info.plist names is pinned as the bundle's, refused by name
// before any cdhash is read (the cdhash check would also refuse it, as the bundle's code is
// another file's).
func TestPinSignedRefusesAnExecutableTheBundleDoesNotName(t *testing.T) {
	exe := signedBundle(t, "Other")
	sums, ok, err := New(DefaultCodesign).PinSigned(t.Context(), exe, appRequirement)
	if ok || err == nil || !strings.Contains(err.Error(), `names "Other" as its executable`) {
		t.Fatalf("pinned %x, ok %v, err %v; want a refusal by name", sums, ok, err)
	}
}

// A same user process that swaps the checked copy before its cdhashes are read gets nothing
// pinned: each cdhash is checked against the requirement on the code that has it.
func TestPinSignedRefusesACopySwappedAfterItsCheck(t *testing.T) {
	evil := filepath.Join(tempDir(t), "evil")
	signedCopy(t, evil, "evil")
	evilHash, err := New(DefaultCodesign).CDHash(t.Context(), evil)
	if err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(tempDir(t), "berth")
	signedCopy(t, good, "good")
	for name, tc := range map[string]struct{ path, requirement string }{
		"file":   {good, `identifier "good"`},
		"bundle": {signedBundle(t, "Berth"), appRequirement},
	} {
		t.Run(name, func(t *testing.T) {
			id := New(DefaultCodesign)
			swapped := false
			id.beforeRead = func(exe string) {
				b, err := os.ReadFile(evil)
				if err == nil {
					next := exe + ".swap"
					if err = os.WriteFile(next, b, 0o755); err == nil { //nolint:gosec // the attacker's executable
						err = os.Rename(next, exe)
					}
				}
				if err != nil {
					t.Error(err)
				}
				swapped = true
			}
			sums, ok, err := id.PinSigned(t.Context(), tc.path, tc.requirement)
			if !swapped || ok || slices.Contains(sums, evilHash) {
				t.Fatalf("after a swap: pinned %x, ok %v, err %v, swapped %v", sums, ok, err, swapped)
			}
		})
	}
}

// A universal binary with one slice that fails the requirement is refused whole.
func TestPinSignedRefusesAMismatchedSlice(t *testing.T) {
	if err := exec.CommandContext(t.Context(), "/usr/bin/lipo", "-archs", "/usr/bin/true").Run(); err != nil {
		t.Skipf("no working lipo: %v", err)
	}
	dir := tempDir(t)
	src := filepath.Join(dir, "src")
	signedCopy(t, src, "good")
	archs, err := sliceArchs(src)
	if err != nil || len(archs) < 2 {
		t.Fatalf("archs %v %v", archs, err)
	}
	var thin []string
	for i, arch := range archs {
		p := filepath.Join(dir, arch)
		if out, err := exec.CommandContext(t.Context(), "/usr/bin/lipo", src, "-thin", arch, "-output", p).CombinedOutput(); err != nil {
			t.Fatalf("lipo: %v\n%s", err, out)
		}
		identifier := "good"
		if i == len(archs)-1 {
			identifier = "bad"
		}
		codesign(t, identifier, p)
		thin = append(thin, p)
	}
	fat := filepath.Join(dir, "fat")
	if out, err := exec.CommandContext(t.Context(), "/usr/bin/lipo", append([]string{"-create", "-output", fat}, thin...)...).CombinedOutput(); err != nil {
		t.Fatalf("lipo: %v\n%s", err, out)
	}
	if sums, ok, err := New(DefaultCodesign).PinSigned(t.Context(), fat, `identifier "good"`); ok {
		t.Fatalf("pinned %x (%v) with a slice signed as another identifier", sums, err)
	}
}

// The copy takes only folders and regular files, opens nothing through a symlink, never blocks on
// a FIFO, and stops at its limits.
func TestPinCopyRefusesOddEntriesAndStopsAtItsLimits(t *testing.T) {
	newBundle := func(t *testing.T) string {
		t.Helper()
		app := filepath.Join(tempDir(t), "X.app")
		if err := os.MkdirAll(filepath.Join(app, "Contents", "MacOS"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(app, "Contents", "MacOS", "X"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return app
	}
	copyOf := func(t *testing.T, app string) error {
		t.Helper()
		return withPrivateBundle(t.Context(), app, func(string) error { return nil })
	}
	if err := copyOf(t, newBundle(t)); err != nil {
		t.Fatalf("a plain bundle: %v", err)
	}
	link := newBundle(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(link, "Contents", "Resources")); err != nil {
		t.Fatal(err)
	}
	fifo := newBundle(t)
	if err := syscall.Mkfifo(filepath.Join(fifo, "Contents", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, app := range map[string]string{"a folder symlink": link, "a FIFO": fifo} {
		done := make(chan error, 1)
		go func() { done <- copyOf(t, app) }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("copied a bundle holding %s", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the copy hung on %s", name)
		}
	}
	saved := copyLimits
	t.Cleanup(func() { copyLimits = saved })
	copyLimits.files = 0
	if err := copyOf(t, newBundle(t)); err == nil || !strings.Contains(err.Error(), "files") {
		t.Errorf("past the file limit: %v", err)
	}
	copyLimits = saved
	copyLimits.depth = 1
	if err := copyOf(t, newBundle(t)); err == nil || !strings.Contains(err.Error(), "deeper") {
		t.Errorf("past the depth limit: %v", err)
	}
	copyLimits = saved
	file := filepath.Join(tempDir(t), "link")
	if err := os.Symlink("/usr/bin/true", file); err != nil {
		t.Fatal(err)
	}
	if err := withPrivateCopy(t.Context(), file, func(string) error { return nil }); err == nil {
		t.Error("copied a single file through a symlink")
	}
}

func TestLeftoverPinFoldersGoAtBoot(t *testing.T) {
	old, err := os.MkdirTemp("", pinFolderPrefix)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := os.MkdirTemp("", pinFolderPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(old); _ = os.RemoveAll(fresh) })
	now := time.Now()
	if err := os.Chtimes(old, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Another user's folders are never removed.
	if err := removeLeftoverPins(time.Hour, now, uint32(os.Getuid())+1); err != nil { //nolint:gosec // user IDs fit in 32 bits
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("removed a folder of another uid: %v", err)
	}
	if err := RemoveLeftoverPins(time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("an old pin folder survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a fresh pin folder went: %v", err)
	}
}
