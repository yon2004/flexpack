package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise the ext4 writer against real filesystems built with the
// same mkfs parameters ChromeOS uses, and validate every result with e2fsck.
//
// e2fsck is the point. An ext4 writer that returns no error can still have
// produced a filesystem the kernel will reject — that is exactly how go-diskfs
// fails. Only a clean e2fsck counts as a pass.
//
// Skipped automatically where e2fsprogs is unavailable (Windows, macOS).

func requireE2fsprogs(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"mke2fs", "e2fsck", "debugfs"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("e2fsprogs not installed (%s missing); skipping filesystem tests", bin)
		}
	}
}

// newFS builds an ext4 image using ChromeOS's own mkfs parameters, taken from
// installer/chromeos-install: mkfs.ext4 -F -b 4096 -L "..." <dev> <blocks>
func newFS(t *testing.T, megabytes int, dirs ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.img")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(megabytes) << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	run(t, "mke2fs", "-q", "-F", "-t", "ext4", "-b", "4096", "-L", "STATE", path)
	for _, d := range dirs {
		run(t, "debugfs", "-w", "-R", "mkdir "+d, path)
	}
	fsckClean(t, path)
	return path
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func fsckClean(t *testing.T, path string) {
	t.Helper()
	out, err := exec.Command("e2fsck", "-fn", path).CombinedOutput()
	if err != nil {
		t.Fatalf("e2fsck reported errors — the filesystem is corrupt:\n%s", out)
	}
}

func readFile(t *testing.T, img, path string) string {
	t.Helper()
	out, err := exec.Command("debugfs", "-R", "cat "+path, img).Output()
	if err != nil {
		t.Fatalf("debugfs cat %s: %v", path, err)
	}
	return string(out)
}

// injectInto writes data at target inside the filesystem image.
func injectInto(t *testing.T, img, target, data string) error {
	t.Helper()
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	fs, err := OpenFS(f, 0)
	if err != nil {
		return err
	}

	parts := splitPath(target)
	dirParts, base := parts[:len(parts)-1], parts[len(parts)-1]

	cur, err := fs.Resolve(nil)
	if err != nil {
		return err
	}
	for i, name := range dirParts {
		next, err := fs.Resolve(dirParts[:i+1])
		if err != nil {
			return err
		}
		if next == nil {
			if next, err = fs.Mkdir(cur, name); err != nil {
				return err
			}
		}
		cur = next
	}
	if _, err := fs.CreateFile(cur, base, []byte(data)); err != nil {
		return err
	}
	return fs.Sync()
}

const testConfig = `{"enrollmentToken": "12345678-90ab-cdef-1234-567890abcdef", "source": "PACKAGING_TOOL"}`

func TestInjectPopulatedParent(t *testing.T) {
	requireE2fsprogs(t)
	img := newFS(t, 512, "/unencrypted", "/dev_image", "/var_overlay")

	if err := injectInto(t, img, flexConfigPath, testConfig); err != nil {
		t.Fatal(err)
	}
	fsckClean(t, img)

	if got := strings.TrimSpace(readFile(t, img, flexConfigPath)); got != testConfig {
		t.Errorf("readback mismatch:\n got: %s\nwant: %s", got, testConfig)
	}
}

func TestInjectEmptyParent(t *testing.T) {
	requireE2fsprogs(t)
	img := newFS(t, 512, "/unencrypted")

	if err := injectInto(t, img, flexConfigPath, testConfig); err != nil {
		t.Fatal(err)
	}
	fsckClean(t, img)
}

// Neither directory exists, so both levels get created.
func TestInjectCreatesBothDirectories(t *testing.T) {
	requireE2fsprogs(t)
	img := newFS(t, 512)

	if err := injectInto(t, img, flexConfigPath, testConfig); err != nil {
		t.Fatal(err)
	}
	fsckClean(t, img)

	if got := strings.TrimSpace(readFile(t, img, flexConfigPath)); got != testConfig {
		t.Errorf("readback mismatch: %s", got)
	}
}

// Larger filesystems have many block groups, which exercises the group
// descriptor and bitmap accounting rather than just group 0.
func TestInjectManyBlockGroups(t *testing.T) {
	requireE2fsprogs(t)
	if testing.Short() {
		t.Skip("large filesystem test skipped in -short mode")
	}
	img := newFS(t, 8192, "/unencrypted")

	if err := injectInto(t, img, flexConfigPath, testConfig); err != nil {
		t.Fatal(err)
	}
	fsckClean(t, img)
}

// A name clash must be refused *before* anything is allocated. Allocating
// first and failing later strands an inode, which e2fsck reports as
// "Unattached inode".
func TestInjectNameClashLeavesNoOrphan(t *testing.T) {
	requireE2fsprogs(t)
	img := newFS(t, 512, "/unencrypted")

	if err := injectInto(t, img, flexConfigPath, testConfig); err != nil {
		t.Fatal(err)
	}
	err := injectInto(t, img, flexConfigPath, `{"enrollmentToken": "other"}`)
	if err == nil {
		t.Fatal("second injection should have been refused")
	}
	if _, ok := err.(*ExistsError); !ok {
		t.Errorf("want *ExistsError so callers can offer --force, got %T: %v", err, err)
	}
	fsckClean(t, img) // the important assertion
}

// Overwrite reuses the existing block and allocates nothing.
func TestOverwriteInPlace(t *testing.T) {
	requireE2fsprogs(t)
	img := newFS(t, 512, "/unencrypted")

	if err := injectInto(t, img, flexConfigPath, testConfig); err != nil {
		t.Fatal(err)
	}

	replacement := `{"enrollmentToken": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "source": "PACKAGING_TOOL"}`
	f, err := os.OpenFile(img, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := OpenFS(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	in, err := fs.Resolve(splitPath(flexConfigPath))
	if err != nil || in == nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := fs.Overwrite(in, []byte(replacement)); err != nil {
		t.Fatal(err)
	}
	if err := fs.Sync(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	fsckClean(t, img)
	if got := strings.TrimSpace(readFile(t, img, flexConfigPath)); got != replacement {
		t.Errorf("overwrite mismatch:\n got: %s\nwant: %s", got, replacement)
	}
}

// The writer must refuse work it cannot do correctly rather than guess.
func TestRefusesOversizedFile(t *testing.T) {
	requireE2fsprogs(t)
	img := newFS(t, 512, "/unencrypted")

	big := strings.Repeat("x", 5000) // larger than one 4 KiB block
	err := injectInto(t, img, flexConfigPath, big)
	if err == nil {
		t.Fatal("expected a refusal for a file larger than one block")
	}
	if !strings.Contains(err.Error(), "single-block") {
		t.Errorf("error should explain the limit, got: %v", err)
	}
	fsckClean(t, img)
}

func TestOpenFSReadsGeometry(t *testing.T) {
	requireE2fsprogs(t)
	img := newFS(t, 512, "/unencrypted")

	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	fs, err := OpenFS(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fs.Label() != "STATE" {
		t.Errorf("Label = %q, want STATE", fs.Label())
	}
	if fs.blockSize != 4096 {
		t.Errorf("blockSize = %d, want 4096", fs.blockSize)
	}
	// s_desc_size lives at 0xFE, not 0xFC. Reading the wrong offset yields
	// 257 and silently corrupts every group descriptor checksum.
	if fs.is64Bit && fs.descSize != 64 {
		t.Errorf("descSize = %d, want 64 — check the s_desc_size offset", fs.descSize)
	}
}

func TestOpenFSRejectsNonExt4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.img")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	defer f.Close()

	if _, err := OpenFS(f, 0); err == nil {
		t.Fatal("expected a magic-number error on a zeroed image")
	}
}
