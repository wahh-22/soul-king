package webplayer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type pulseTestFS struct {
	fileSystem
	present string
	checked func(string)
}

func (f pulseTestFS) Stat(path string) (fs.FileInfo, error) {
	if f.checked != nil {
		f.checked(path)
	}
	if path == f.present {
		return pulseFileInfo{}, nil
	}
	return nil, fs.ErrNotExist
}

type pulseFileInfo struct{}

func (pulseFileInfo) Name() string       { return "libpulse.so.0" }
func (pulseFileInfo) Size() int64        { return 1 }
func (pulseFileInfo) Mode() fs.FileMode  { return 0644 }
func (pulseFileInfo) ModTime() time.Time { return time.Time{} }
func (pulseFileInfo) IsDir() bool        { return false }
func (pulseFileInfo) Sys() any           { return nil }

func TestLoginMissingLibpulseWarns(t *testing.T) {
	ev := &events{}
	l := newLauncher(t, newFakeBrowser(ev), statusPage(ev, [2]bool{true, true}))
	l.fs = pulseTestFS{}
	var out strings.Builder
	if err := l.login(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	want := "warning: libpulse is not installed, so the browser will play without sound; install it (Debian/Ubuntu: sudo apt install libpulse0; Fedora: sudo dnf install pulseaudio-libs)"
	if !strings.Contains(out.String(), want+"\n") {
		t.Fatalf("login output = %q; want libpulse warning %q", out.String(), want)
	}
}

func TestLibpulsePresentInEachDirectory(t *testing.T) {
	for _, dir := range libpulseDirs() {
		t.Run(dir, func(t *testing.T) {
			ev := &events{}
			l := newLauncher(t, newFakeBrowser(ev), statusPage(ev, [2]bool{true, true}))
			l.fs = pulseTestFS{present: filepath.Join(dir, "libpulse.so.0")}
			var out strings.Builder
			if err := l.login(context.Background(), &out); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "warning:") {
				t.Fatalf("unexpected warning: %s", &out)
			}
			p, err := l.open(context.Background(), OpenOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if p.StartupWarning() != "" {
				t.Fatal(p.StartupWarning())
			}
		})
	}
}

func TestOpenMissingLibpulseWithoutDisplay(t *testing.T) {
	ev := &events{}
	l := newLauncher(t, newFakeBrowser(ev), statusPage(ev, [2]bool{true, true}))
	l.getenv = func(string) string { return "" }
	l.fs = pulseTestFS{}
	p, err := l.open(context.Background(), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.StartupWarning() != "apple music has no sound: install libpulse (libpulse0)" {
		t.Fatalf("warning = %q", p.StartupWarning())
	}
	if l.launched != 1 || !l.opts[0].Headless {
		t.Fatal("headless launch was prevented")
	}
}

func TestNonLinuxSkipsLibpulse(t *testing.T) {
	ev := &events{}
	l := newLauncher(t, newFakeBrowser(ev), statusPage(ev, [2]bool{true, true}))
	l.goos = "darwin"
	l.fs = pulseTestFS{checked: func(string) { t.Fatal("non-Linux checked host libpulse") }}
	var out strings.Builder
	if err := l.login(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "warning:") {
		t.Fatal(out.String())
	}
}

func TestLoginNeedsDisplayBeforeLaunch(t *testing.T) {
	ev := &events{}
	l := newLauncher(t, newFakeBrowser(ev), statusPage(ev, [2]bool{true, true}))
	l.getenv = func(string) string { return "" }
	err := l.login(context.Background(), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "no display: run nu11signal --apple-music-login") || !strings.Contains(err.Error(), "not over SSH") {
		t.Fatalf("login error = %v", err)
	}
	if l.launched != 0 {
		t.Fatal("launched without a display")
	}
}

func TestLoginWaylandOnly(t *testing.T) {
	ev := &events{}
	l := newLauncher(t, newFakeBrowser(ev), statusPage(ev, [2]bool{true, true}))
	l.getenv = func(key string) string {
		if key == "WAYLAND_DISPLAY" {
			return "wayland-0"
		}
		return ""
	}
	if err := l.login(context.Background(), &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if l.launched != 1 {
		t.Fatal("Wayland display was refused")
	}
}

func TestLoginEarlyBrowserGoneFriendly(t *testing.T) {
	for _, stage := range []string{"launch", "attach", "status"} {
		t.Run(stage, func(t *testing.T) {
			ev := &events{}
			b := newFakeBrowser(ev)
			page := newPage()
			l := newLauncher(t, b, page)
			switch stage {
			case "launch":
				l.launch = func(context.Context, Options) (browser, error) { return nil, ErrBrowserGone }
			case "attach":
				l.attach = func(context.Context, *Client) (Evaluator, error) { return nil, ErrBrowserGone }
			case "status":
				page.setHandle(func(call) (any, error) { return nil, ErrBrowserGone })
			}
			err := l.login(context.Background(), &strings.Builder{})
			if !errors.Is(err, ErrBrowserGone) || !strings.Contains(err.Error(), "no display:") || !strings.Contains(err.Error(), "retry") || strings.Contains(err.Error(), ErrBrowserGone.Error()) {
				t.Fatalf("login error = %v", err)
			}
		})
	}
}

func TestVisibleLaunchErrorLeavesOtherFailuresAlone(t *testing.T) {
	for _, err := range []error{ErrProfileInUse, context.Canceled, errors.New("offline")} {
		if got := visibleLaunchError(err, nil, time.Second, context.Background()); got != err {
			t.Fatalf("changed %v to %v", err, got)
		}
	}
	if got := visibleLaunchError(ErrBrowserGone, nil, 6*time.Second, context.Background()); got != ErrBrowserGone {
		t.Fatal("late exit mislabeled as display failure")
	}
}

// events is an ordered log shared by a fake browser and its page.
type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, s)
}

func (e *events) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.log)
}

// fakeBrowser stands in for a launched browser: Close records itself and
// ends Done, as a browser that exited.
type fakeBrowser struct {
	events *events
	done   chan struct{}
	once   sync.Once
	closes int
	mu     sync.Mutex
}

func newFakeBrowser(ev *events) *fakeBrowser {
	return &fakeBrowser{events: ev, done: make(chan struct{})}
}

func (b *fakeBrowser) Client() *Client { return nil }

func (b *fakeBrowser) Done() <-chan struct{} { return b.done }

func (b *fakeBrowser) Close(context.Context) error {
	b.mu.Lock()
	b.closes++
	b.mu.Unlock()
	b.events.add("close browser")
	b.exit()
	return nil
}

// exit ends the browser on its own.
func (b *fakeBrowser) exit() { b.once.Do(func() { close(b.done) }) }

func (b *fakeBrowser) closed() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closes
}

// statusPage answers status() from a script of {ready, authorized} pairs
// (the last one repeats) and logs every other call in ev.
func statusPage(ev *events, script ...[2]bool) *fakePage {
	page := newPage()
	var mu sync.Mutex
	i := 0
	page.setHandle(func(c call) (any, error) {
		if c.fn != "status" {
			ev.add(c.String())
			return nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		s := script[min(i, len(script)-1)]
		i++
		return map[string]bool{"ready": s[0], "authorized": s[1]}, nil
	})
	return page
}

// fakeLauncher launches b with page, records the options, and finds
// the profile free unless inUse says otherwise.
type fakeLauncher struct {
	launcher
	opts     []Options
	launched int
}

func newLauncher(t *testing.T, b *fakeBrowser, page Evaluator) *fakeLauncher {
	t.Helper()
	profile := t.TempDir()
	f := &fakeLauncher{}
	f.launcher = launcher{
		fs:   pulseTestFS{present: "/lib/x86_64-linux-gnu/libpulse.so.0"},
		goos: "linux",
		discover: func() (Installation, error) {
			return Installation{Path: "/usr/bin/google-chrome", Binary: "/opt/google/chrome/chrome"}, nil
		},
		profileDir: func() (string, error) { return profile, nil },
		inUse:      func(string) bool { return false },
		getenv: func(key string) string {
			if key == "DISPLAY" {
				return ":0"
			}
			return ""
		},
		launch: func(_ context.Context, opts Options) (browser, error) {
			f.opts = append(f.opts, opts)
			f.launched++
			return b, nil
		},
		attach:       func(context.Context, *Client) (Evaluator, error) { return page, nil },
		readyTimeout: 2 * time.Second,
		readyEvery:   time.Millisecond,
		loginEvery:   time.Millisecond,
		loginTimeout: 2 * time.Second,
	}
	return f
}

type observingPage struct {
	Evaluator
	observe func(context.Context, string)
}

func (p observingPage) Evaluate(ctx context.Context, expr string) (json.RawMessage, error) {
	p.observe(ctx, expr)
	return p.Evaluator.Evaluate(ctx, expr)
}

func TestOpenAndLoginFlatpakProfileAndTimeout(t *testing.T) {
	for _, login := range []bool{false, true} {
		t.Run(fmt.Sprintf("login=%v", login), func(t *testing.T) {
			home := t.TempDir()
			profile := filepath.Join(home, ".var/app/com.google.Chrome/nu11signal-webplayer")
			ev := &events{}
			page := statusPage(ev, [2]bool{true, true})
			l := newLauncher(t, newFakeBrowser(ev), page)
			l.discover = func() (Installation, error) {
				return Installation{Path: "/usr/bin/flatpak", Flatpak: flatpakChromeID, Profile: profile}, nil
			}
			l.fs = pulseTestFS{checked: func(string) { t.Fatal("Flatpak checked host libpulse") }}
			l.profileDir = func() (string, error) {
				t.Fatal("Flatpak must not use the host config profile")
				return "", nil
			}
			l.inUse = func(got string) bool {
				if got != profile {
					t.Fatalf("lock checked on %q; want %q", got, profile)
				}
				return false
			}
			checkedTimeout := false
			l.attach = func(context.Context, *Client) (Evaluator, error) {
				return observingPage{Evaluator: page, observe: func(ctx context.Context, expr string) {
					if !strings.Contains(expr, namespace+".status(") {
						return
					}
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) < 59*time.Second || time.Until(deadline) > 60*time.Second {
						t.Errorf("Flatpak MusicKit wait must allow 60s; deadline %v", deadline)
					}
					checkedTimeout = true
				}}, nil
			}
			if login {
				if err := l.login(context.Background(), &strings.Builder{}); err != nil {
					t.Fatal(err)
				}
			} else {
				p, err := l.open(context.Background(), OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer p.Close()
			}
			if !checkedTimeout || len(l.opts) != 1 || l.opts[0].Profile != profile ||
				l.opts[0].Flatpak != flatpakChromeID || l.opts[0].ReadyTimeout != 60*time.Second {
				t.Fatalf("Flatpak startup options = %+v; timeout checked=%v", l.opts, checkedTimeout)
			}
			fi, err := os.Stat(profile)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o700 {
				t.Fatalf("profile permissions = %04o", fi.Mode().Perm())
			}
		})
	}
}

func TestFlatpakProfileRejectsUnsafeOrLockedDirectory(t *testing.T) {
	for _, kind := range []string{"permissions", "symlink", "locked"} {
		t.Run(kind, func(t *testing.T) {
			profile := filepath.Join(t.TempDir(), ".var/app/com.google.Chrome/nu11signal-webplayer")
			if err := os.MkdirAll(filepath.Dir(profile), 0o700); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Symlink(t.TempDir(), profile); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(profile, 0o700); err != nil {
					t.Fatal(err)
				}
				if kind == "permissions" {
					if err := os.Chmod(profile, 0o750); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink("elsewhere-1234", filepath.Join(profile, singletonLock)); err != nil {
					t.Fatal(err)
				}
			}
			l := newLauncher(t, newFakeBrowser(&events{}), newPage())
			l.discover = func() (Installation, error) {
				return Installation{Path: "/usr/bin/flatpak", Flatpak: flatpakChromeID, Profile: profile}, nil
			}
			l.inUse = profileLocked
			for _, login := range []bool{false, true} {
				var err error
				if login {
					err = l.login(context.Background(), &strings.Builder{})
				} else {
					_, err = l.open(context.Background(), OpenOptions{})
				}
				if err == nil || (kind == "locked" && !errors.Is(err, ErrProfileInUse)) {
					t.Fatalf("%s profile must be refused (login=%v): %v", kind, login, err)
				}
			}
			if l.launched != 0 {
				t.Fatal("unsafe or locked profile launched")
			}
		})
	}
}

func TestOpenLaunchesHeadlessWaitsUntilReadyAndHides(t *testing.T) {
	ev := &events{}
	b := newFakeBrowser(ev)
	page := statusPage(ev, [2]bool{false, false}, [2]bool{false, false}, [2]bool{true, true})
	l := newLauncher(t, b, page)
	p, err := l.open(context.Background(), OpenOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(l.opts) != 1 {
		t.Fatalf("launched %d times; want 1", len(l.opts))
	}
	o := l.opts[0]
	if !o.Headless || o.URL != URL || o.Browser != "/usr/bin/google-chrome" || !filepath.IsAbs(o.Profile) {
		t.Errorf("launch options = %+v; want headless at %s with the discovered browser and the profile", o, URL)
	}
	if got := ev.all(); !slices.Equal(got, []string{"hide()"}) {
		t.Errorf("page calls before use = %v; want hide() once ready", got)
	}
	if _, err := p.PlaySongs(context.Background(), []string{"111"}, 0); err != nil {
		t.Fatalf("PlaySongs: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := ev.all(), []string{"hide()", `play(["111"],0)`, "stop()", "close browser"}; !slices.Equal(got, want) {
		t.Errorf("events = %v; want %v (the browser closed after the stop)", got, want)
	}
	_ = p.Close()
	if b.closed() != 1 {
		t.Errorf("browser closed %d times; want 1", b.closed())
	}
}

func TestOpenWithoutABrowserLaunchesNothing(t *testing.T) {
	l := newLauncher(t, newFakeBrowser(&events{}), newPage())
	l.discover = func() (Installation, error) { return Installation{}, &NoBrowserError{Arch: "linux_x64"} }
	if _, err := l.open(context.Background(), OpenOptions{}); !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("open = %v; want ErrNoBrowser", err)
	}
	if l.launched != 0 {
		t.Fatal("launched a browser without one")
	}
}

func TestOpenRefusesAProfileInUse(t *testing.T) {
	t.Run("locked before the launch", func(t *testing.T) {
		l := newLauncher(t, newFakeBrowser(&events{}), newPage())
		l.inUse = func(string) bool { return true }
		if _, err := l.open(context.Background(), OpenOptions{}); !errors.Is(err, ErrProfileInUse) {
			t.Fatalf("open = %v; want ErrProfileInUse", err)
		}
		if l.launched != 0 {
			t.Fatal("launched a browser on a profile in use")
		}
	})
	t.Run("the browser exits on a lock taken meanwhile", func(t *testing.T) {
		l := newLauncher(t, newFakeBrowser(&events{}), newPage())
		checks := 0
		l.inUse = func(string) bool { checks++; return checks > 1 }
		l.launch = func(context.Context, Options) (browser, error) {
			return nil, errors.New("webplayer: chrome did not answer on the DevTools pipe")
		}
		_, err := l.open(context.Background(), OpenOptions{})
		if !errors.Is(err, ErrProfileInUse) {
			t.Fatalf("open = %v; want ErrProfileInUse", err)
		}
		if !strings.Contains(err.Error(), "close the other nu11signal or login window") {
			t.Errorf("error %q does not say what to do", err)
		}
	})
	t.Run("an attach failure on a lock held elsewhere", func(t *testing.T) {
		b := newFakeBrowser(&events{})
		l := newLauncher(t, b, newPage())
		closedFirst := false
		l.inUse = func(string) bool {
			select {
			case <-b.Done():
				closedFirst = true
				return true
			default:
				return false
			}
		}
		l.attach = func(context.Context, *Client) (Evaluator, error) { return nil, ErrBrowserGone }
		if _, err := l.open(context.Background(), OpenOptions{}); !errors.Is(err, ErrProfileInUse) {
			t.Fatalf("open = %v; want ErrProfileInUse", err)
		}
		if !closedFirst || b.closed() != 1 {
			t.Errorf("the browser was not closed before the lock was checked (closes %d)", b.closed())
		}
	})
}

func TestOpenGivesUpOnAPageThatNeverLoads(t *testing.T) {
	b := newFakeBrowser(&events{})
	l := newLauncher(t, b, statusPage(&events{}, [2]bool{false, false}))
	l.readyTimeout = 20 * time.Millisecond
	_, err := l.open(context.Background(), OpenOptions{})
	if !errors.Is(err, ErrNotLoaded) || !strings.Contains(err.Error(), "music.apple.com did not load within") {
		t.Fatalf("open = %v; want ErrNotLoaded", err)
	}
	if b.closed() != 1 {
		t.Fatalf("browser closed %d times; want 1", b.closed())
	}
}

func TestOpenWrapsABrowserThatDoesNotStart(t *testing.T) {
	t.Run("launch", func(t *testing.T) {
		l := newLauncher(t, newFakeBrowser(&events{}), newPage())
		l.launch = func(context.Context, Options) (browser, error) {
			return nil, errors.New("webplayer: start /usr/bin/google-chrome: exec format error")
		}
		_, err := l.open(context.Background(), OpenOptions{})
		if !errors.Is(err, ErrLaunchFailed) || errors.Is(err, ErrProfileInUse) || !strings.Contains(err.Error(), "exec format error") {
			t.Fatalf("open = %v; want ErrLaunchFailed with the reason", err)
		}
	})
	t.Run("attach", func(t *testing.T) {
		b := newFakeBrowser(&events{})
		l := newLauncher(t, b, newPage())
		l.attach = func(context.Context, *Client) (Evaluator, error) { return nil, ErrBrowserGone }
		if _, err := l.open(context.Background(), OpenOptions{}); !errors.Is(err, ErrLaunchFailed) || !errors.Is(err, ErrBrowserGone) {
			t.Fatalf("open = %v; want ErrLaunchFailed wrapping ErrBrowserGone", err)
		}
		if b.closed() != 1 {
			t.Errorf("browser closed %d times; want 1", b.closed())
		}
	})
}

// flagsEnv answers BrowserFlagsEnv with v.
func flagsEnv(v string) func(string) string {
	return func(name string) string {
		if name == "DISPLAY" {
			return ":0"
		}
		if name == BrowserFlagsEnv {
			return v
		}
		return ""
	}
}

func TestOpenAndLoginLaunchWithTheBrowserFlags(t *testing.T) {
	want := []string{"--no-sandbox", "--ozone-platform=wayland"}
	l := newLauncher(t, newFakeBrowser(&events{}), statusPage(&events{}, [2]bool{true, true}))
	l.getenv = flagsEnv("--no-sandbox --ozone-platform=wayland")
	p, err := l.open(context.Background(), OpenOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = p.Close()
	if len(l.opts) != 1 || !slices.Equal(l.opts[0].ExtraFlags, want) {
		t.Fatalf("open launch options = %+v; want ExtraFlags %q", l.opts, want)
	}

	l = newLauncher(t, newFakeBrowser(&events{}), statusPage(&events{}, [2]bool{true, true}))
	l.getenv = flagsEnv("--no-sandbox --ozone-platform=wayland")
	if err := l.login(context.Background(), &strings.Builder{}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if len(l.opts) != 1 || !slices.Equal(l.opts[0].ExtraFlags, want) {
		t.Fatalf("login launch options = %+v; want ExtraFlags %q", l.opts, want)
	}
}

func TestOpenAndLoginRefuseInvalidBrowserFlags(t *testing.T) {
	l := newLauncher(t, newFakeBrowser(&events{}), newPage())
	l.getenv = flagsEnv("--no-sandbox --remote-debugging-port=9222")
	_, err := l.open(context.Background(), OpenOptions{})
	if err == nil || errors.Is(err, ErrLaunchFailed) || !strings.Contains(err.Error(), BrowserFlagsEnv) ||
		!strings.Contains(err.Error(), "--remote-debugging-port=9222") {
		t.Fatalf("open = %v; want an error naming %s and the flag", err, BrowserFlagsEnv)
	}
	l.getenv = flagsEnv("no-sandbox")
	if err := l.login(context.Background(), &strings.Builder{}); err == nil || !strings.Contains(err.Error(), BrowserFlagsEnv) {
		t.Fatalf("login = %v; want an error naming %s", err, BrowserFlagsEnv)
	}
	if l.launched != 0 {
		t.Fatal("launched a browser with invalid flags")
	}
}

func TestOpenReportsTheBrowserExitOnce(t *testing.T) {
	b := newFakeBrowser(&events{})
	l := newLauncher(t, b, statusPage(&events{}, [2]bool{true, true}))
	p, err := l.open(context.Background(), OpenOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	b.exit()
	select {
	case err := <-p.Errors():
		if !errors.Is(err, ErrBrowserGone) {
			t.Fatalf("Errors = %v; want ErrBrowserGone", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the browser's exit was not reported")
	}
	_ = p.Close()
	for err := range p.Errors() {
		t.Errorf("reported again: %v", err)
	}
}

func TestProfileLocked(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname:", err)
	}
	tests := []struct {
		name   string
		target string // "" leaves no lock
		want   bool
	}{
		{"no lock", "", false},
		{"held by a live process", fmt.Sprintf("%s-%d", host, os.Getpid()), true},
		{"left by a dead process", fmt.Sprintf("%s-%d", host, 1<<30), false},
		{"held on another computer", "elsewhere-1234", true},
		{"malformed", "nonsense", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.target != "" {
				if err := os.Symlink(tt.target, filepath.Join(dir, "SingletonLock")); err != nil {
					t.Fatal(err)
				}
			}
			if got := profileLocked(dir); got != tt.want {
				t.Fatalf("profileLocked(%s) = %v; want %v", tt.target, got, tt.want)
			}
		})
	}
}

func TestLoginPrintsNoBrowserHint(t *testing.T) {
	tr := newTree(t)
	tr.exe("/snap/bin/chromium")
	_, missing := tr.discover(discoverCase{goarch: "amd64"})
	l := newLauncher(t, newFakeBrowser(&events{}), newPage())
	l.discover = func() (Installation, error) { return Installation{}, missing }
	var out strings.Builder
	if err := l.login(context.Background(), &out); !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("login = %v; want ErrNoBrowser", err)
	}
	if out.String() != missing.Error()+"\n" {
		t.Fatalf("output = %q; want %q", out.String(), missing.Error()+"\n")
	}
	out.Reset()
	if _, err := l.open(context.Background(), OpenOptions{Stderr: &out}); !errors.Is(err, ErrNoBrowser) {
		t.Fatalf("open = %v; want ErrNoBrowser", err)
	}
	if out.Len() != 0 || l.launched != 0 {
		t.Fatal("normal startup must remain silent and neither path may launch")
	}
}

func TestLoginWaitsForTheSignInThenCloses(t *testing.T) {
	ev := &events{}
	b := newFakeBrowser(ev)
	page := statusPage(ev, [2]bool{false, false}, [2]bool{true, false}, [2]bool{true, false}, [2]bool{true, true})
	l := newLauncher(t, b, page)
	var out strings.Builder
	if err := l.login(context.Background(), &out); err != nil {
		t.Fatalf("login: %v", err)
	}
	if len(l.opts) != 1 || l.opts[0].Headless || l.opts[0].URL != URL {
		t.Fatalf("launch options = %+v; want one visible window at %s", l.opts, URL)
	}
	want := "Sign in to Apple Music in the window that opened; it closes by itself when you are signed in.\n" +
		"Signed in. Run nu11signal to play.\n"
	if out.String() != want {
		t.Errorf("output = %q; want %q", out.String(), want)
	}
	if b.closed() != 1 {
		t.Errorf("browser closed %d times; want 1", b.closed())
	}
	if got := ev.all(); slices.Contains(got, "hide()") {
		t.Errorf("the sign-in page was hidden: %v", got)
	}
}

func TestLoginWhenAlreadySignedIn(t *testing.T) {
	b := newFakeBrowser(&events{})
	l := newLauncher(t, b, statusPage(&events{}, [2]bool{true, true}))
	var out strings.Builder
	if err := l.login(context.Background(), &out); err != nil {
		t.Fatalf("login: %v", err)
	}
	if got, want := out.String(), "Already signed in to Apple Music. Run nu11signal to play.\n"; got != want {
		t.Errorf("output = %q; want %q", got, want)
	}
	if b.closed() != 1 {
		t.Errorf("browser closed %d times; want 1", b.closed())
	}
}

func TestLoginCancelledClosesTheWindow(t *testing.T) {
	b := newFakeBrowser(&events{})
	l := newLauncher(t, b, statusPage(&events{}, [2]bool{true, false}))
	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	errc := make(chan error, 1)
	go func() { errc <- l.login(ctx, &out) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("login = %v; want cancelled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("login did not end with its context")
	}
	if b.closed() != 1 {
		t.Errorf("browser closed %d times; want 1", b.closed())
	}
	if strings.Contains(out.String(), "Signed in") {
		t.Errorf("output = %q; a cancelled sign-in reported success", out.String())
	}
}

func TestLoginGivesUpAfterItsTimeout(t *testing.T) {
	b := newFakeBrowser(&events{})
	l := newLauncher(t, b, statusPage(&events{}, [2]bool{true, false}))
	l.loginTimeout = 20 * time.Millisecond
	err := l.login(context.Background(), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("login = %v; want a timeout", err)
	}
	if b.closed() != 1 {
		t.Errorf("browser closed %d times; want 1", b.closed())
	}
}

func TestLoginRefusesAProfileInUse(t *testing.T) {
	l := newLauncher(t, newFakeBrowser(&events{}), newPage())
	l.inUse = func(string) bool { return true }
	if err := l.login(context.Background(), &strings.Builder{}); !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("login = %v; want ErrProfileInUse", err)
	}
}

func TestPlayerTellsHowToSignIn(t *testing.T) {
	p := New(newPage())
	defer p.Close()
	if got, want := p.AuthorizationHint(), "quit and run nu11signal --apple-music-login"; got != want {
		t.Fatalf("AuthorizationHint = %q; want %q", got, want)
	}
}
