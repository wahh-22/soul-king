package webplayer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeBrowserEnv makes the test binary act as a browser (see TestMain):
// its value is the fake's mode.
const fakeBrowserEnv = "WEBPLAYER_FAKE_BROWSER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeBrowserEnv); mode != "" {
		os.Exit(runFakeBrowser(mode))
	}
	os.Exit(m.Run())
}

// runFakeBrowser speaks the DevTools pipe on fd 3 (commands) and fd 4
// (responses) like a browser started with --remote-debugging-pipe.
//
// Modes: "normal" exits 0 on Browser.close; "ignore-close" answers
// Browser.close but keeps running, with a child in its process group;
// "silent" never answers; "sleeper" is that child.
func runFakeBrowser(mode string) int {
	if mode == "sleeper" {
		time.Sleep(time.Hour)
		return 0
	}
	in := bufio.NewReader(os.NewFile(3, "devtools-in"))
	out := os.NewFile(4, "devtools-out")
	childPID := 0
	if mode == "ignore-close" {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), fakeBrowserEnv+"=sleeper")
		if err := child.Start(); err != nil {
			return 9
		}
		childPID = child.Process.Pid
	}
	for {
		raw, err := in.ReadBytes(0)
		if err != nil {
			if mode == "normal" {
				return 7 // a clean exit must come from Browser.close
			}
			time.Sleep(time.Hour)
		}
		var m peerMsg
		if json.Unmarshal(raw[:len(raw)-1], &m) != nil || mode == "silent" {
			continue
		}
		var result any = map[string]any{}
		switch m.Method {
		case "Browser.getVersion":
			result = map[string]any{"product": "Fake/1"}
		case "Fake.info":
			result = map[string]any{"args": os.Args[1:], "pid": os.Getpid(), "pgid": syscall.Getpgrp(), "child": childPID}
		case "Fake.exit":
			return 3
		}
		b, _ := json.Marshal(map[string]any{"id": m.ID, "result": result})
		if _, err := out.Write(append(b, 0)); err != nil {
			return 8
		}
		if m.Method == "Browser.close" && mode == "normal" {
			return 0
		}
	}
}

// --- Discover ---

// tree is a fake root filesystem under a temporary directory; paths are
// absolute within it and symlinks are relative so they stay inside.
type tree struct {
	t    *testing.T
	root string
}

func newTree(t *testing.T) tree {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return tree{t: t, root: root}
}

func (tr tree) file(path string, mode fs.FileMode) {
	tr.t.Helper()
	p := filepath.Join(tr.root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), mode); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr tree) exe(path string) { tr.file(path, 0o755) }

// cdm adds a Widevine library under dir for arch (linux_x64, linux_arm64).
func (tr tree) cdm(dir, arch string) {
	tr.file(filepath.Join(dir, "_platform_specific", arch, "libwidevinecdm.so"), 0o644)
}

func (tr tree) link(path, target string) {
	tr.t.Helper()
	p := filepath.Join(tr.root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		tr.t.Fatal(err)
	}
}

type rootFS struct{ root string }

func (r rootFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(r.root + name) }

func (r rootFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(r.root + name) }

func (r rootFS) EvalSymlinks(name string) (string, error) {
	p, err := filepath.EvalSymlinks(r.root + name)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(p, r.root), nil
}

type discoverCase struct {
	path   map[string]string // command name -> path on PATH
	vars   map[string]string
	goarch string
}

func (tr tree) discover(c discoverCase) (Installation, error) {
	return discover(discoverEnv{
		getenv: func(k string) string { return c.vars[k] },
		lookPath: func(name string) (string, error) {
			if p, ok := c.path[name]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		},
		fs:     rootFS{tr.root},
		goarch: c.goarch,
		home:   "/home/u",
	})
}

func noBrowser(t *testing.T, err error) *NoBrowserError {
	t.Helper()
	var nb *NoBrowserError
	if !errors.As(err, &nb) || !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("want NoBrowserError, got %v", err)
	}
	return nb
}

func TestDiscoverChromiumWithWidevineNextToRealBinary(t *testing.T) {
	tr := newTree(t)
	tr.exe("/usr/bin/chromium") // Debian's wrapper script
	tr.exe("/usr/lib/chromium/chromium")
	tr.cdm("/usr/lib/chromium/WidevineCdm", "linux_arm64")

	got, err := tr.discover(discoverCase{path: map[string]string{"chromium": "/usr/bin/chromium"}, goarch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	want := Installation{
		Path:     "/usr/bin/chromium",
		Binary:   "/usr/lib/chromium/chromium",
		Widevine: "/usr/lib/chromium/WidevineCdm/_platform_specific/linux_arm64/libwidevinecdm.so",
	}
	if got.Path != want.Path || got.Binary != want.Binary || got.Widevine != want.Widevine {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if !strings.Contains(got.Reason, "chromium") || !strings.Contains(got.Reason, "Widevine") {
		t.Fatalf("unhelpful reason %q", got.Reason)
	}
}

func TestDiscoverRejectsChromiumWithoutWidevine(t *testing.T) {
	tr := newTree(t)
	tr.exe("/usr/bin/chromium")
	tr.exe("/usr/lib/chromium/chromium")

	_, err := tr.discover(discoverCase{path: map[string]string{"chromium": "/usr/bin/chromium"}, goarch: "arm64"})
	nb := noBrowser(t, err)
	if !slices.Equal(nb.WithoutWidevine, []string{"/usr/bin/chromium"}) {
		t.Fatalf("WithoutWidevine = %v", nb.WithoutWidevine)
	}
}

func TestDiscoverEnvOverride(t *testing.T) {
	tr := newTree(t)
	tr.exe("/opt/google/chrome/chrome")
	tr.cdm("/opt/google/chrome/WidevineCdm", "linux_x64")
	tr.exe("/srv/browser/chrome")
	tr.cdm("/srv/browser/WidevineCdm", "linux_x64")
	tr.exe("/srv/bare/chrome")
	path := map[string]string{"google-chrome": "/opt/google/chrome/chrome"}

	got, err := tr.discover(discoverCase{path: path, goarch: "amd64",
		vars: map[string]string{BrowserEnv: "/srv/browser/chrome"}})
	if err != nil || got.Path != "/srv/browser/chrome" || !strings.Contains(got.Reason, BrowserEnv) {
		t.Fatalf("override not used: %+v, %v", got, err)
	}

	// The override is authoritative: no fallback to PATH when it is unusable.
	_, err = tr.discover(discoverCase{path: path, goarch: "amd64",
		vars: map[string]string{BrowserEnv: "/srv/bare/chrome"}})
	if nb := noBrowser(t, err); !slices.Equal(nb.WithoutWidevine, []string{"/srv/bare/chrome"}) {
		t.Fatalf("WithoutWidevine = %v", nb.WithoutWidevine)
	}

	for _, bad := range []string{"chrome", "/srv/missing/chrome", "/srv/browser"} {
		_, err := tr.discover(discoverCase{path: path, goarch: "amd64", vars: map[string]string{BrowserEnv: bad}})
		if err == nil || !strings.Contains(err.Error(), BrowserEnv) {
			t.Fatalf("%s: want an error naming %s, got %v", bad, BrowserEnv, err)
		}
	}
}

func TestDiscoverChromeWrapperScriptResolvedViaInstallDir(t *testing.T) {
	tr := newTree(t)
	tr.exe("/usr/bin/google-chrome") // a shell script that execs /opt/google/chrome/chrome
	tr.exe("/opt/google/chrome/chrome")
	tr.cdm("/opt/google/chrome/WidevineCdm", "linux_x64")

	got, err := tr.discover(discoverCase{path: map[string]string{"google-chrome": "/usr/bin/google-chrome"}, goarch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/usr/bin/google-chrome" || got.Binary != "/opt/google/chrome/chrome" ||
		got.Widevine != "/opt/google/chrome/WidevineCdm/_platform_specific/linux_x64/libwidevinecdm.so" {
		t.Fatalf("got %+v", got)
	}
}

func TestDiscoverResolvesSymlinkChain(t *testing.T) {
	tr := newTree(t)
	tr.exe("/opt/google/chrome/google-chrome")
	tr.cdm("/opt/google/chrome/WidevineCdm", "linux_arm64")
	tr.link("/etc/alternatives/google-chrome", "../../opt/google/chrome/google-chrome")
	tr.link("/usr/bin/google-chrome-stable", "../../etc/alternatives/google-chrome")

	got, err := tr.discover(discoverCase{path: map[string]string{"google-chrome-stable": "/usr/bin/google-chrome-stable"}, goarch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/usr/bin/google-chrome-stable" || got.Binary != "/opt/google/chrome/google-chrome" {
		t.Fatalf("got %+v", got)
	}
}

func TestDiscoverComponentUpdatedWidevine(t *testing.T) {
	tr := newTree(t)
	tr.exe("/usr/bin/chromium")
	tr.cdm("/home/u/.config/chromium/WidevineCdm/4.9.0.0", "linux_x64")
	tr.cdm("/home/u/.config/chromium/WidevineCdm/4.10.2830.0", "linux_x64")
	tr.cdm("/home/u/.config/chromium/WidevineCdm/4.11.0.0", "linux_arm64")
	tr.file("/home/u/.config/chromium/WidevineCdm/latest-component-updated-widevine-cdm", 0o644)

	got, err := tr.discover(discoverCase{path: map[string]string{"chromium": "/usr/bin/chromium"}, goarch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Widevine != "/home/u/.config/chromium/WidevineCdm/4.10.2830.0/_platform_specific/linux_x64/libwidevinecdm.so" {
		t.Fatalf("want newest matching version, got %+v", got)
	}

	// XDG_CONFIG_HOME moves the browser's config directory, and a Chrome
	// copy does not count for Chromium.
	tr2 := newTree(t)
	tr2.exe("/usr/bin/chromium")
	tr2.cdm("/xdg/google-chrome/WidevineCdm/4.10.0.0", "linux_x64")
	tr2.cdm("/home/u/.config/chromium/WidevineCdm/4.10.0.0", "linux_x64") // shadowed by XDG
	xdg := map[string]string{"XDG_CONFIG_HOME": "/xdg"}
	_, err = tr2.discover(discoverCase{path: map[string]string{"chromium": "/usr/bin/chromium"}, vars: xdg, goarch: "amd64"})
	noBrowser(t, err)

	tr2.exe("/usr/bin/google-chrome")
	got, err = tr2.discover(discoverCase{
		path:   map[string]string{"google-chrome": "/usr/bin/google-chrome", "chromium": "/usr/bin/chromium"},
		vars:   xdg,
		goarch: "amd64",
	})
	if err != nil || got.Path != "/usr/bin/google-chrome" ||
		got.Widevine != "/xdg/google-chrome/WidevineCdm/4.10.0.0/_platform_specific/linux_x64/libwidevinecdm.so" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestDiscoverRejectsWidevineForAnotherArch(t *testing.T) {
	tr := newTree(t)
	tr.exe("/usr/bin/chromium")
	tr.exe("/usr/lib/chromium/chromium")
	tr.cdm("/usr/lib/chromium/WidevineCdm", "linux_x64")

	_, err := tr.discover(discoverCase{path: map[string]string{"chromium": "/usr/bin/chromium"}, goarch: "arm64"})
	nb := noBrowser(t, err)
	if nb.Arch != "linux_arm64" || !strings.Contains(err.Error(), "linux_arm64") {
		t.Fatalf("error should name the needed platform: %v", err)
	}
	_, err = tr.discover(discoverCase{path: map[string]string{"chromium": "/usr/bin/chromium"}, goarch: "riscv64"})
	noBrowser(t, err)
}

func TestDiscoverPreferenceAndErrorListing(t *testing.T) {
	tr := newTree(t)
	tr.exe("/opt/google/chrome/google-chrome")
	tr.link("/usr/bin/google-chrome", "../../opt/google/chrome/google-chrome")
	tr.link("/usr/bin/google-chrome-stable", "../../opt/google/chrome/google-chrome")
	tr.exe("/usr/bin/chromium-browser")
	path := map[string]string{
		"google-chrome":        "/usr/bin/google-chrome",
		"google-chrome-stable": "/usr/bin/google-chrome-stable",
		"chromium-browser":     "/usr/bin/chromium-browser",
	}

	_, err := tr.discover(discoverCase{path: path, goarch: "amd64"})
	nb := noBrowser(t, err)
	if !slices.Equal(nb.WithoutWidevine, []string{"/usr/bin/google-chrome", "/usr/bin/chromium-browser"}) {
		t.Fatalf("want each browser once, in preference order: %v", nb.WithoutWidevine)
	}
	for _, s := range []string{"/usr/bin/google-chrome", "/usr/bin/chromium-browser", "Widevine"} {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("error %q should mention %q", err, s)
		}
	}

	// With Widevine everywhere, Google Chrome comes first.
	tr.cdm("/opt/google/chrome/WidevineCdm", "linux_x64")
	tr.exe("/usr/lib/chromium-browser/chromium-browser")
	tr.cdm("/usr/lib/chromium-browser/WidevineCdm", "linux_x64")
	got, err := tr.discover(discoverCase{path: path, goarch: "amd64"})
	if err != nil || got.Path != "/usr/bin/google-chrome" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestDiscoverInstallDirWithoutPathEntry(t *testing.T) {
	tr := newTree(t)
	tr.exe("/usr/lib64/chromium-browser/chromium-browser")
	tr.cdm("/usr/lib64/chromium-browser/WidevineCdm", "linux_x64")

	got, err := tr.discover(discoverCase{goarch: "amd64"})
	if err != nil || got.Path != "/usr/lib64/chromium-browser/chromium-browser" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestDiscoverSnapHint(t *testing.T) {
	tr := newTree(t)
	tr.exe("/snap/bin/chromium")
	tr.file("/snap/chromium/current/marker", 0o644)
	_, err := tr.discover(discoverCase{goarch: "amd64"})
	nb := noBrowser(t, err)
	for _, want := range []string{"Snap", "no Widevine", "cannot be driven", "google.com/chrome", ".deb/.rpm", "libwidevinecdm0"} {
		if !strings.Contains(nb.Error(), want) {
			t.Errorf("error %q should mention %q", nb.Error(), want)
		}
	}
}

func TestDiscoverSandboxedMarkers(t *testing.T) {
	for _, marker := range []string{
		"/snap/bin/chromium", "/snap/chromium/current",
		"/var/lib/flatpak/app/org.chromium.Chromium",
		"/var/lib/flatpak/app/com.google.Chrome",
		"/var/lib/flatpak/app/com.github.Eloston.UngoogledChromium",
		"/home/u/.local/share/flatpak/app/org.chromium.Chromium",
		"/home/u/.local/share/flatpak/app/com.google.Chrome",
		"/home/u/.local/share/flatpak/app/com.github.Eloston.UngoogledChromium",
		"/var/lib/flatpak/exports/bin/com.google.Chrome",
	} {
		t.Run(marker, func(t *testing.T) {
			tr := newTree(t)
			if strings.Contains(marker, "/bin/") {
				tr.exe(marker)
			} else {
				tr.file(marker+"/marker", 0o644)
			}
			_, err := tr.discover(discoverCase{goarch: "amd64"})
			nb := noBrowser(t, err)
			if len(nb.Sandboxed) != 1 {
				t.Fatalf("Sandboxed = %+v", nb.Sandboxed)
			}
			wantName, wantKind := filepath.Base(marker), "flatpak"
			if strings.HasPrefix(marker, "/snap/") {
				wantName, wantKind = "chromium", "snap"
			}
			if nb.Sandboxed[0] != (SandboxedBrowser{Name: wantName, Kind: wantKind}) {
				t.Fatalf("Sandboxed = %+v", nb.Sandboxed)
			}
			if !strings.Contains(nb.Error(), "cannot") || !strings.Contains(nb.Error(), ".deb/.rpm") {
				t.Fatalf("unhelpful error: %v", nb)
			}
			if wantName == "com.google.Chrome" && strings.Contains(nb.Error(), "no Widevine") {
				t.Fatalf("Flatpak Chrome includes Widevine: %v", nb)
			}
		})
	}
}

func TestDiscoverSandboxedNeverChosen(t *testing.T) {
	tr := newTree(t)
	tr.exe("/snap/bin/chromium")
	tr.cdm("/home/u/.config/chromium/WidevineCdm/1.0", "linux_x64")
	c := discoverCase{goarch: "amd64", path: map[string]string{"chromium": "/snap/bin/chromium"}}
	_, err := tr.discover(c)
	noBrowser(t, err)
	c.vars = map[string]string{BrowserEnv: "/snap/bin/chromium"}
	_, err = tr.discover(c)
	noBrowser(t, err)
	c.vars = nil
	tr.exe("/opt/google/chrome/chrome")
	tr.cdm("/opt/google/chrome/WidevineCdm", "linux_x64")
	got, err := tr.discover(c)
	if err != nil || got.Path != "/opt/google/chrome/chrome" {
		t.Fatalf("real Chrome should win: %+v, %v", got, err)
	}
}

func TestDiscoverFlatpakNeverChosen(t *testing.T) {
	for _, base := range []string{"/var/lib/flatpak", "/home/u/.local/share/flatpak"} {
		t.Run(base, func(t *testing.T) {
			tr := newTree(t)
			p := base + "/exports/bin/com.google.Chrome"
			tr.exe(p)
			tr.cdm(base+"/exports/bin/WidevineCdm", "linux_x64")
			tr.link("/usr/bin/google-chrome", "../../"+strings.TrimPrefix(p, "/"))
			_, err := tr.discover(discoverCase{goarch: "amd64", path: map[string]string{"google-chrome": "/usr/bin/google-chrome"}})
			if nb := noBrowser(t, err); len(nb.Sandboxed) != 1 {
				t.Fatalf("Sandboxed = %+v", nb.Sandboxed)
			}
		})
	}
}

func TestDiscoverSandboxedDeduplicatesMarkers(t *testing.T) {
	tr := newTree(t)
	tr.exe("/snap/bin/chromium")
	tr.file("/snap/chromium/current/marker", 0o644)
	tr.file("/var/lib/flatpak/app/com.google.Chrome/marker", 0o644)
	tr.file("/home/u/.local/share/flatpak/app/com.google.Chrome/marker", 0o644)
	_, err := tr.discover(discoverCase{goarch: "amd64"})
	if nb := noBrowser(t, err); len(nb.Sandboxed) != 2 {
		t.Fatalf("Sandboxed = %+v; want one per kind/name", nb.Sandboxed)
	}
}

func TestDiscoverSandboxedARM64Hint(t *testing.T) {
	tr := newTree(t)
	tr.exe("/snap/bin/chromium")
	_, err := tr.discover(discoverCase{goarch: "arm64"})
	for _, want := range []string{"no Linux ARM64 Chrome", "distribution Chromium with Widevine"} {
		if !strings.Contains(noBrowser(t, err).Error(), want) {
			t.Fatalf("error %v should mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), ".deb/.rpm") {
		t.Fatalf("must not recommend Chrome on ARM64: %v", err)
	}
}

func TestDiscoverNothingInstalled(t *testing.T) {
	tr := newTree(t)
	_, err := tr.discover(discoverCase{goarch: "amd64"})
	nb := noBrowser(t, err)
	if len(nb.WithoutWidevine) != 0 || !strings.Contains(err.Error(), "no Google Chrome or Chromium") {
		t.Fatalf("unexpected error %v", err)
	}
}

// --- ProfileDir ---

func TestProfileDirCreatesOwnerOnlyDirectory(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	for _, tc := range []struct {
		name, xdg, want string
	}{
		{"xdg", xdg, filepath.Join(xdg, "nu11signal", "webplayer")},
		{"home", "", filepath.Join(home, ".config", "nu11signal", "webplayer")},
		{"relative xdg ignored", "relative", filepath.Join(home, ".config", "nu11signal", "webplayer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string {
				if k == "XDG_CONFIG_HOME" {
					return tc.xdg
				}
				return ""
			}
			for range 2 { // idempotent
				got, err := profileDir(getenv, home)
				if err != nil || got != tc.want {
					t.Fatalf("got %q, %v; want %q", got, err, tc.want)
				}
			}
			fi, err := os.Stat(tc.want)
			if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
				t.Fatalf("mode %v, %v", fi.Mode(), err)
			}
		})
	}
}

func TestProfileDirRejectsUnsafeExisting(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "nu11signal", "webplayer")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	noenv := func(string) string { return "" }
	if _, err := profileDir(noenv, home); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("want a permissions error, got %v", err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o750 {
		t.Fatalf("existing directory must not be changed: %v", fi.Mode())
	}

	home2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home2, ".config", "nu11signal"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(home2, ".config", "nu11signal", "webplayer")); err != nil {
		t.Fatal(err)
	}
	if _, err := profileDir(noenv, home2); err == nil {
		t.Fatal("a symlinked profile directory must be rejected")
	}

	if _, err := profileDir(noenv, ""); err == nil {
		t.Fatal("no home and no XDG_CONFIG_HOME must fail")
	}
}

// --- Launch ---

func TestBrowserArgs(t *testing.T) {
	required := []string{
		"--remote-debugging-pipe",
		"--user-data-dir=/p",
		"--no-first-run",
		"--no-default-browser-check",
		"--password-store=basic",
		"--autoplay-policy=no-user-gesture-required",
		"--disable-background-media-suspend",
		"--disable-renderer-backgrounding",
		"--disable-background-timer-throttling",
		"--hide-crash-restore-bubble",
	}
	args := browserArgs(Options{Profile: "/p", URL: "https://music.apple.com/", ExtraFlags: []string{"--no-sandbox"}})
	for _, f := range required {
		if !slices.Contains(args, f) {
			t.Fatalf("missing %s in %v", f, args)
		}
	}
	if slices.Contains(args, "--headless=new") {
		t.Fatalf("headless flag without Headless: %v", args)
	}
	if args[len(args)-1] != "https://music.apple.com/" || args[len(args)-2] != "--no-sandbox" {
		t.Fatalf("extra flags then URL must come last: %v", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--remote-debugging-port") || strings.HasPrefix(a, "--remote-debugging-address") {
			t.Fatalf("must never open a debugging port: %v", args)
		}
	}
	if args := browserArgs(Options{Profile: "/p", Headless: true}); !slices.Contains(args, "--headless=new") {
		t.Fatalf("missing --headless=new: %v", args)
	}
}

func fakeOptions(t *testing.T, mode string) Options {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Options{
		Browser:      exe,
		Profile:      t.TempDir(),
		URL:          "about:blank",
		Headless:     true,
		ExtraFlags:   []string{"--mute-audio"},
		Env:          append(os.Environ(), fakeBrowserEnv+"="+mode),
		ReadyTimeout: 5 * time.Second,
		CloseTimeout: 5 * time.Second,
	}
}

func launchFake(t *testing.T, opts Options) *Browser {
	t.Helper()
	b, err := Launch(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

type fakeInfo struct {
	Args  []string `json:"args"`
	PID   int      `json:"pid"`
	PGID  int      `json:"pgid"`
	Child int      `json:"child"`
}

func info(t *testing.T, b *Browser) fakeInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := b.Client().Call(ctx, "", "Fake.info", nil)
	if err != nil {
		t.Fatal(err)
	}
	var fi fakeInfo
	if err := json.Unmarshal(raw, &fi); err != nil {
		t.Fatal(err)
	}
	return fi
}

func waitDone(t *testing.T, b *Browser) {
	t.Helper()
	select {
	case <-b.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("browser not reaped")
	}
}

func TestLaunchWiresPipeAndOwnProcessGroup(t *testing.T) {
	opts := fakeOptions(t, "normal")
	b := launchFake(t, opts)
	fi := info(t, b)
	if fi.PID != b.PID() || fi.PGID != b.PID() {
		t.Fatalf("browser must lead its own process group: pid %d pgid %d (PID() %d)", fi.PID, fi.PGID, b.PID())
	}
	if !slices.Equal(fi.Args, browserArgs(opts)) {
		t.Fatalf("args %v, want %v", fi.Args, browserArgs(opts))
	}

	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitDone(t, b)
	if err := b.Err(); err != nil {
		t.Fatalf("exit after Browser.close should be clean: %v", err)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("Close is idempotent: %v", err)
	}
	if _, err := b.Client().Call(context.Background(), "", "X.y", nil); !errors.Is(err, ErrBrowserGone) {
		t.Fatalf("calls after Close: want ErrBrowserGone, got %v", err)
	}
}

func TestCloseKillsProcessGroupAfterGrace(t *testing.T) {
	opts := fakeOptions(t, "ignore-close")
	opts.CloseTimeout = 300 * time.Millisecond
	b := launchFake(t, opts)
	child := info(t, b).Child
	if child == 0 {
		t.Fatal("fake did not start its child")
	}

	start := time.Now()
	err := b.Close(context.Background())
	if err == nil || !strings.Contains(err.Error(), "killed") {
		t.Fatalf("want a killed error, got %v", err)
	}
	if d := time.Since(start); d < opts.CloseTimeout {
		t.Fatalf("killed before the grace period: %v", d)
	}
	waitDone(t, b)
	waitGone(t, child)
}

func TestCloseContextCutsGraceShort(t *testing.T) {
	opts := fakeOptions(t, "ignore-close")
	opts.CloseTimeout = time.Minute
	b := launchFake(t, opts)
	child := info(t, b).Child

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := b.Close(ctx); err == nil {
		t.Fatal("want a killed error")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Close ignored its context: %v", d)
	}
	waitDone(t, b)
	waitGone(t, child)
}

// waitGone waits until pid no longer exists (its orphan is reaped by init).
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d in the browser's group survived Close", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUnexpectedExitFailsCallsAndReportsErr(t *testing.T) {
	b := launchFake(t, fakeOptions(t, "normal"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.Client().Call(ctx, "", "Fake.exit", nil); !errors.Is(err, ErrBrowserGone) {
		t.Fatalf("want ErrBrowserGone, got %v", err)
	}
	waitDone(t, b)
	if err := b.Err(); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("Err = %v", err)
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("Close after exit: %v", err)
	}
}

func TestLaunchFailsWhenBrowserDoesNotAnswer(t *testing.T) {
	opts := fakeOptions(t, "silent")
	opts.ReadyTimeout = 300 * time.Millisecond
	start := time.Now()
	if _, err := Launch(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("want a readiness error, got %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Launch took %v", d)
	}
}

func TestBrowserFlagsFromTheEnvironment(t *testing.T) {
	env := func(v string) func(string) string {
		return func(name string) string {
			if name == BrowserFlagsEnv {
				return v
			}
			return ""
		}
	}
	for _, tt := range []struct {
		value string
		want  []string
	}{
		{"", nil},
		{"   ", nil},
		{"--no-sandbox", []string{"--no-sandbox"}},
		{" --no-sandbox  --ozone-platform=wayland ", []string{"--no-sandbox", "--ozone-platform=wayland"}},
	} {
		got, err := browserFlags(env(tt.value))
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("browserFlags(%q) = %q, %v; want %q", tt.value, got, err, tt.want)
		}
	}
	for _, bad := range []string{
		"--remote-debugging-port=9222",
		"--no-sandbox -remote-debugging-pipe",
		"--user-data-dir=/tmp/other",
		"no-sandbox",
		"-no-sandbox",
		"--",
		"--=x",
		"---x",
	} {
		_, err := browserFlags(env(bad))
		if err == nil || !strings.Contains(err.Error(), BrowserFlagsEnv) {
			t.Errorf("browserFlags(%q) = %v; want an error naming %s", bad, err, BrowserFlagsEnv)
		}
	}
}

func TestLaunchValidatesOptions(t *testing.T) {
	base := fakeOptions(t, "normal")
	for name, mutate := range map[string]func(*Options){
		"no browser":        func(o *Options) { o.Browser = "" },
		"missing browser":   func(o *Options) { o.Browser = filepath.Join(t.TempDir(), "nope") },
		"relative profile":  func(o *Options) { o.Profile = "profile" },
		"debugging port":    func(o *Options) { o.ExtraFlags = []string{"--remote-debugging-port=9222"} },
		"debugging address": func(o *Options) { o.ExtraFlags = []string{"--remote-debugging-address=0.0.0.0"} },
		"another profile":   func(o *Options) { o.ExtraFlags = []string{"--user-data-dir=/tmp/x"} },
		"a bare word":       func(o *Options) { o.ExtraFlags = []string{"https://example.com/"} },
		"a single dash":     func(o *Options) { o.ExtraFlags = []string{"-no-sandbox"} },
		"url as a flag":     func(o *Options) { o.URL = "--disable-web-security" },
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			mutate(&o)
			if b, err := Launch(context.Background(), o); err == nil {
				_ = b.Close(context.Background())
				t.Fatal("want an error")
			}
		})
	}
}
