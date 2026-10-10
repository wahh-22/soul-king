package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/wahh-22/nu11signal/internal/config"
	"github.com/wahh-22/nu11signal/internal/helper"
	"github.com/wahh-22/nu11signal/internal/history"
	"github.com/wahh-22/nu11signal/internal/playback"
	"github.com/wahh-22/nu11signal/internal/playback/composite"
	"github.com/wahh-22/nu11signal/internal/playback/demo"
	"github.com/wahh-22/nu11signal/internal/playback/playbacktest"
	"github.com/wahh-22/nu11signal/internal/playback/webplayer"
	"github.com/wahh-22/nu11signal/internal/update"
)

// fakePlayer is a helper-backed player stand-in that records Close calls;
// its States and Errors deliver nothing until closed. Calling any other
// Player method panics (nil embedded interface).
type fakePlayer struct {
	playback.Player
	closed int
	states chan playback.State
	errs   chan error
}

func newFakePlayer() *fakePlayer {
	return &fakePlayer{states: make(chan playback.State), errs: make(chan error)}
}

func (p *fakePlayer) States() <-chan playback.State { return p.states }
func (p *fakePlayer) Errors() <-chan error          { return p.errs }

func (p *fakePlayer) Close() error {
	if p.closed == 0 {
		close(p.states)
		close(p.errs)
	}
	p.closed++
	return nil
}

// testEnv wires run to fakes; each test overrides only what it exercises.
type testEnv struct {
	stdout, stderr bytes.Buffer
	d              deps
	uiPlayer       playback.Player
	uiRecents      history.Recents
	uiConfig       config.Source
	uiCalm         bool
	uiUpdates      update.Checker
	uiRuns         int
	// local is the local backend openLocal returned, and localDirs the
	// folders it was given.
	local     *playbacktest.Fake
	localDirs []string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	e := &testEnv{}
	e.d = deps{
		stdout: &e.stdout,
		stderr: &e.stderr,
		goos:   "darwin",
		openLocal: func(dirs []string) playback.Player {
			e.local, e.localDirs = playbacktest.New(), dirs
			return e.local
		},
		locateHelper: func() (string, error) {
			t.Error("locateHelper called unexpectedly")
			return "", errors.New("unexpected locate")
		},
		startHelper: func(context.Context, string) (playback.Player, error) {
			t.Error("startHelper called unexpectedly")
			return nil, errors.New("unexpected start")
		},
		openWebPlayer: func(context.Context) (playback.Player, error) {
			t.Error("openWebPlayer called unexpectedly")
			return nil, errors.New("unexpected web player")
		},
		loginAppleMusic: func(context.Context, io.Writer) error {
			t.Error("loginAppleMusic called unexpectedly")
			return errors.New("unexpected login")
		},
		runUI: func(p playback.Player, r history.Recents, cfg config.Source, calm bool, updates update.Checker) error {
			e.uiRuns++
			e.uiPlayer, e.uiRecents, e.uiConfig, e.uiCalm, e.uiUpdates = p, r, cfg, calm, updates
			return nil
		},
	}
	return e
}

func TestRunVersionPrintsVersionAndExitsZero(t *testing.T) {
	e := newTestEnv(t)
	if code := run([]string{"--version"}, e.d); code != 0 {
		t.Fatalf("exit code = %d; want 0", code)
	}
	if got, want := e.stdout.String(), version+"\n"; got != want {
		t.Fatalf("stdout = %q; want %q", got, want)
	}
	if e.uiRuns != 0 {
		t.Fatal("--version started the UI")
	}
}

func TestRunHelpExitsZeroWithUsage(t *testing.T) {
	e := newTestEnv(t)
	if code := run([]string{"-h"}, e.d); code != 0 {
		t.Fatalf("exit code = %d; want 0", code)
	}
	if !strings.Contains(e.stderr.String(), "-demo") {
		t.Fatalf("stderr = %q; want usage listing -demo", e.stderr.String())
	}
}

func TestRunUnknownFlagExitsTwo(t *testing.T) {
	e := newTestEnv(t)
	if code := run([]string{"--nope"}, e.d); code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if !strings.Contains(e.stderr.String(), "flag provided but not defined: -nope") {
		t.Fatalf("stderr = %q; want the flag error", e.stderr.String())
	}
	if e.uiRuns != 0 {
		t.Fatal("a flag error started the UI")
	}
}

// tempHome points the user's config directory at a temporary home.
func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	return home
}

// assertLocalOnly checks the UI got a player of the local files alone,
// closed on exit.
func assertLocalOnly(t *testing.T, e *testEnv) {
	t.Helper()
	c, ok := e.uiPlayer.(*composite.Player)
	if !ok {
		t.Fatalf("UI got player %T; want *composite.Player", e.uiPlayer)
	}
	if c.Supports(playback.CapCatalogSearch, "") {
		t.Error("a local-only player offers the catalog")
	}
	if e.local == nil || !e.local.Closed() {
		t.Error("the local backend was not opened and closed")
	}
}

func TestRunHelperNotFoundPlaysLocalFiles(t *testing.T) {
	tempHome(t)
	e := newTestEnv(t)
	e.d.locateHelper = func() (string, error) {
		return "", fmt.Errorf("%w: set NU11SIGNAL_HELPER or build the helper", helper.ErrHelperNotFound)
	}
	if code := run(nil, e.d); code != 0 {
		t.Fatalf("exit code = %d (stderr %q); want 0, local files only", code, e.stderr.String())
	}
	assertLocalOnly(t, e)
}

// onLinux makes e a Linux run whose web player finds no usable browser.
func onLinux(e *testEnv) {
	e.d.goos = "linux"
	e.d.openWebPlayer = func(context.Context) (playback.Player, error) {
		return nil, &webplayer.NoBrowserError{Arch: "linux_x64"}
	}
}

// Without a usable browser Linux plays the local files alone, quietly:
// nothing on stderr, which the UI is about to take over.
func TestRunOnLinuxWithoutABrowserPlaysLocalFiles(t *testing.T) {
	tempHome(t)
	e := newTestEnv(t) // locateHelper fails the test if called
	onLinux(e)
	if code := run(nil, e.d); code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, e.stderr.String())
	}
	assertLocalOnly(t, e)
	if e.stderr.Len() != 0 {
		t.Errorf("stderr = %q; want nothing", e.stderr.String())
	}
}

func TestRunOnLinuxJoinsTheWebPlayerWithTheLocalFiles(t *testing.T) {
	tempHome(t)
	e := newTestEnv(t) // locateHelper and startHelper fail the test if called
	e.d.goos = "linux"
	player := newFakePlayer()
	var bounded bool
	e.d.openWebPlayer = func(ctx context.Context) (playback.Player, error) {
		_, bounded = ctx.Deadline()
		return player, nil
	}
	if code := run(nil, e.d); code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, e.stderr.String())
	}
	if !bounded {
		t.Error("the web player's startup has no deadline")
	}
	c, ok := e.uiPlayer.(*composite.Player)
	if !ok || !c.Supports(playback.CapCatalogSearch, "") {
		t.Fatalf("UI got player %T; want the web player joined with the local files", e.uiPlayer)
	}
	if player.closed != 1 {
		t.Fatalf("web player closed %d times; want 1", player.closed)
	}
	if e.local == nil || !e.local.Closed() {
		t.Fatal("the local backend was not opened and closed")
	}
}

// A browser that is there but cannot start the web player stops startup,
// as a broken helper does on macOS.
func TestRunOnLinuxWebPlayerFailureExitsOne(t *testing.T) {
	tempHome(t)
	e := newTestEnv(t)
	e.d.goos = "linux"
	e.d.openWebPlayer = func(context.Context) (playback.Player, error) {
		return nil, webplayer.ErrProfileInUse
	}
	if code := run(nil, e.d); code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	want := "nu11signal: start web player: " + webplayer.ErrProfileInUse.Error() + "\n"
	if got := e.stderr.String(); got != want {
		t.Fatalf("stderr = %q; want %q", got, want)
	}
	if e.uiRuns != 0 || e.local != nil {
		t.Fatal("a failed web player opened the local files or started the UI")
	}
}

// A web player that does not load (offline) or whose browser does not
// start falls back to the local files, with one notice for the UI; the
// interrupt that cancels startup still stops it.
func TestRunOnLinuxWebPlayerThatDoesNotStartFallsBackToLocalFiles(t *testing.T) {
	tests := []struct {
		name, notice string
		err          error
	}{
		{"page did not load", "apple music unavailable (web player did not load) // local files only",
			fmt.Errorf("%w within 30s", webplayer.ErrNotLoaded)},
		{"browser did not start", "apple music unavailable (browser did not start) // local files only",
			fmt.Errorf("%w: %w", webplayer.ErrLaunchFailed, errors.New("exec format error"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempHome(t)
			e := newTestEnv(t)
			e.d.goos = "linux"
			e.d.openWebPlayer = func(context.Context) (playback.Player, error) { return nil, tt.err }
			if code := run(nil, e.d); code != 0 {
				t.Fatalf("exit code = %d (stderr %q); want 0, local files only", code, e.stderr.String())
			}
			assertLocalOnly(t, e)
			if e.stderr.Len() != 0 {
				t.Errorf("stderr = %q; want nothing", e.stderr.String())
			}
			// The notices the UI did not read are still buffered.
			var notices []string
			for err := range e.uiPlayer.Errors() {
				notices = append(notices, err.Error())
			}
			if want := []string{tt.notice}; !reflect.DeepEqual(notices, want) {
				t.Fatalf("notices = %q; want %q", notices, want)
			}
		})
	}
}

// Without a browser the fallback is silent: no notice either.
func TestRunOnLinuxWithoutABrowserHasNoNotice(t *testing.T) {
	tempHome(t)
	e := newTestEnv(t)
	onLinux(e)
	if code := run(nil, e.d); code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, e.stderr.String())
	}
	for err := range e.uiPlayer.Errors() {
		t.Errorf("notice %q; want none", err)
	}
}

// --local and --demo do not start the web player.
func TestRunOnLinuxLocalAndDemoSkipTheWebPlayer(t *testing.T) {
	tempHome(t)
	e := newTestEnv(t) // openWebPlayer fails the test if called
	e.d.goos = "linux"
	if code := run([]string{"--local"}, e.d); code != 0 {
		t.Fatalf("--local: exit code = %d (stderr %q)", code, e.stderr.String())
	}
	assertLocalOnly(t, e)

	e = newTestEnv(t)
	e.d.goos = "linux"
	if code := run([]string{"--demo"}, e.d); code != 0 {
		t.Fatalf("--demo: exit code = %d (stderr %q)", code, e.stderr.String())
	}
	if _, ok := e.uiPlayer.(*demo.Player); !ok {
		t.Fatalf("--demo: UI got player %T; want *demo.Player", e.uiPlayer)
	}
}

func TestRunAppleMusicLoginOnLinux(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantCode   int
		wantStderr string
	}{
		{"signed in", nil, 0, ""},
		{"failed", errors.New("webplayer: sign-in cancelled"), 1, "nu11signal: webplayer: sign-in cancelled\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempHome(t)
			e := newTestEnv(t) // the helper and the web player fail the test if called
			e.d.goos = "linux"
			calls := 0
			e.d.loginAppleMusic = func(ctx context.Context, out io.Writer) error {
				calls++
				io.WriteString(out, "Signed in.\n")
				return tt.err
			}
			if code := run([]string{"--apple-music-login"}, e.d); code != tt.wantCode {
				t.Fatalf("exit code = %d; want %d", code, tt.wantCode)
			}
			if calls != 1 {
				t.Fatalf("login ran %d times; want 1", calls)
			}
			if got := e.stdout.String(); got != "Signed in.\n" {
				t.Errorf("stdout = %q; want the login's output", got)
			}
			if got := e.stderr.String(); got != tt.wantStderr {
				t.Errorf("stderr = %q; want %q", got, tt.wantStderr)
			}
			if e.uiRuns != 0 || e.local != nil {
				t.Fatal("--apple-music-login opened the local files or started the UI")
			}
		})
	}
}

func TestRunAppleMusicLoginOnMacOSIsAnError(t *testing.T) {
	tempHome(t)
	e := newTestEnv(t) // login, the helper and the web player fail the test if called
	if code := run([]string{"--apple-music-login"}, e.d); code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	if got, want := e.stderr.String(), "nu11signal: --apple-music-login is for Linux; on macOS Apple Music signs in through the helper\n"; got != want {
		t.Fatalf("stderr = %q; want %q", got, want)
	}
	if e.uiRuns != 0 || e.local != nil {
		t.Fatal("a rejected login opened the local files or started the UI")
	}
}

func TestRunLocalFlagSkipsTheHelper(t *testing.T) {
	tempHome(t)
	e := newTestEnv(t) // locateHelper fails the test if called
	if code := run([]string{"--local"}, e.d); code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, e.stderr.String())
	}
	assertLocalOnly(t, e)
}

func TestRunScansMusicDirs(t *testing.T) {
	home := tempHome(t)
	e := newTestEnv(t)
	onLinux(e)
	if run(nil, e.d); !reflect.DeepEqual(e.localDirs, []string{filepath.Join(home, "Music")}) {
		t.Errorf("default dirs = %q; want ~/Music", e.localDirs)
	}
	path, err := config.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"music_dirs": ["~/tapes", "/srv/music", "~"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e = newTestEnv(t)
	onLinux(e)
	want := []string{filepath.Join(home, "tapes"), "/srv/music", home}
	if run(nil, e.d); !reflect.DeepEqual(e.localDirs, want) {
		t.Errorf("configured dirs = %q; want %q", e.localDirs, want)
	}
}

func TestRunHelperStartFailureExitsOne(t *testing.T) {
	tempHome(t)
	e := newTestEnv(t)
	e.d.locateHelper = func() (string, error) { return "/opt/helper", nil }
	var gotPath string
	e.d.startHelper = func(_ context.Context, path string) (playback.Player, error) {
		gotPath = path
		return nil, errors.New("helper exited")
	}
	if code := run(nil, e.d); code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	if gotPath != "/opt/helper" {
		t.Fatalf("startHelper path = %q; want the located one", gotPath)
	}
	if got, want := e.stderr.String(), "nu11signal: start helper: helper exited\n"; got != want {
		t.Fatalf("stderr = %q; want %q", got, want)
	}
}

func TestRunPlaysThroughHelperAndClosesIt(t *testing.T) {
	// The recent-searches file lives in the user's config directory; point
	// it at a temporary home so the test never depends on the host's.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	e := newTestEnv(t)
	player := newFakePlayer()
	e.d.locateHelper = func() (string, error) { return "/opt/helper", nil }
	e.d.startHelper = func(context.Context, string) (playback.Player, error) { return player, nil }
	if code := run(nil, e.d); code != 0 {
		t.Fatalf("exit code = %d; want 0 (stderr %q)", code, e.stderr.String())
	}
	// Apple Music and the local files, joined.
	c, ok := e.uiPlayer.(*composite.Player)
	if !ok || !c.Supports(playback.CapCatalogSearch, "") {
		t.Fatalf("UI got player %T; want the helper joined with the local files", e.uiPlayer)
	}
	if player.closed != 1 {
		t.Fatalf("player closed %d times; want 1", player.closed)
	}
	if e.local == nil || !e.local.Closed() {
		t.Fatal("the local backend was not opened and closed")
	}
	if _, ok := e.uiRecents.(*history.File); !ok {
		t.Fatalf("UI got recents %T; want the recent-searches file", e.uiRecents)
	}
	if _, ok := e.uiConfig.(*config.File); !ok {
		t.Fatalf("UI got settings %T; want the settings file", e.uiConfig)
	}
}

func TestRunDemoUsesSimulatedPlayerWithoutHelper(t *testing.T) {
	e := newTestEnv(t) // locateHelper and startHelper fail the test if called
	if code := run([]string{"--demo"}, e.d); code != 0 {
		t.Fatalf("exit code = %d; want 0 (stderr %q)", code, e.stderr.String())
	}
	if _, ok := e.uiPlayer.(*demo.Player); !ok {
		t.Fatalf("UI got player %T; want *demo.Player", e.uiPlayer)
	}
	// The demo keeps recent searches in memory, off the user's config.
	if e.uiRecents != nil {
		t.Fatalf("UI got recents %T; want none (in-memory fallback)", e.uiRecents)
	}
	// It reads the settings, though: they only choose how it looks.
	if _, ok := e.uiConfig.(*config.File); !ok {
		t.Fatalf("UI got settings %T; want the settings file", e.uiConfig)
	}
}

func TestRunUIExitPaths(t *testing.T) {
	tests := []struct {
		name       string
		uiErr      error
		wantCode   int
		wantStderr string
	}{
		{"interrupt is a clean exit", tea.ErrInterrupted, 0, ""},
		{"UI failure exits one", errors.New("no tty"), 1, "nu11signal: no tty\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.d.runUI = func(playback.Player, history.Recents, config.Source, bool, update.Checker) error { return tt.uiErr }
			if code := run([]string{"--demo"}, e.d); code != tt.wantCode {
				t.Fatalf("exit code = %d; want %d", code, tt.wantCode)
			}
			if got := e.stderr.String(); got != tt.wantStderr {
				t.Fatalf("stderr = %q; want %q", got, tt.wantStderr)
			}
		})
	}
}

func TestRunCalmStartsTheEffectsOff(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  string
		want bool
	}{
		{"effects on by default", []string{"--demo"}, "", false},
		{"calm flag", []string{"--demo", "--calm"}, "", true},
		{"calm environment", []string{"--demo"}, "1", true},
		{"environment other than 1", []string{"--demo"}, "0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NU11SIGNAL_CALM", tt.env)
			e := newTestEnv(t)
			if code := run(tt.args, e.d); code != 0 {
				t.Fatalf("exit code = %d; want 0 (stderr %q)", code, e.stderr.String())
			}
			if e.uiCalm != tt.want {
				t.Fatalf("UI calm = %v; want %v", e.uiCalm, tt.want)
			}
		})
	}
}

// updateEnv is a helper-backed run with version v, a temporary home and
// NU11SIGNAL_NO_UPDATE_CHECK set to noCheck; settings, when not empty,
// is written as the settings file first.
func updateEnv(t *testing.T, v, noCheck, settings string) *testEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv(noUpdateCheckEnv, noCheck)
	old := version
	version = v
	t.Cleanup(func() { version = old })
	if settings != "" {
		path, err := config.DefaultPath()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := newTestEnv(t)
	e.d.locateHelper = func() (string, error) { return "/opt/helper", nil }
	e.d.startHelper = func(context.Context, string) (playback.Player, error) { return newFakePlayer(), nil }
	return e
}

func TestRunPassesACachedGitHubChecker(t *testing.T) {
	e := updateEnv(t, "0.3.0", "", "")
	if code := run(nil, e.d); code != 0 {
		t.Fatalf("exit code = %d (stderr %q)", code, e.stderr.String())
	}
	c, ok := e.uiUpdates.(*update.Cached)
	if !ok {
		t.Fatalf("UI got updates %T; want *update.Cached", e.uiUpdates)
	}
	cfg, _ := config.DefaultPath()
	if want := filepath.Join(filepath.Dir(cfg), "update.json"); c.Path != want {
		t.Fatalf("cache path = %q; want %q", c.Path, want)
	}
	g, ok := c.Inner.(*update.GitHub)
	if !ok || g.UserAgent != "nu11signal/0.3.0" {
		t.Fatalf("inner checker = %#v; want GitHub for nu11signal/0.3.0", c.Inner)
	}
}

func TestRunUpdateCheckOptOuts(t *testing.T) {
	tests := []struct {
		name, version, env, settings string
		args                         []string
	}{
		{"dev build", "dev", "", "", nil},
		{"unparsable version", "nightly", "", "", nil},
		{"environment", "0.3.0", "1", "", nil},
		{"settings file", "0.3.0", "", `{"update_check": false}`, nil},
		{"demo", "0.3.0", "", "", []string{"--demo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := updateEnv(t, tt.version, tt.env, tt.settings)
			if code := run(tt.args, e.d); code != 0 {
				t.Fatalf("exit code = %d (stderr %q)", code, e.stderr.String())
			}
			if e.uiUpdates != nil {
				t.Fatalf("UI got updates %T; want none", e.uiUpdates)
			}
		})
	}
	// An environment value other than 1 leaves the check on.
	e := updateEnv(t, "0.3.0", "0", `{"update_check": true}`)
	if run(nil, e.d); e.uiUpdates == nil {
		t.Fatal("NU11SIGNAL_NO_UPDATE_CHECK=0 with update_check true turned the check off")
	}
}

// A cold Flatpak Chrome may take FlatpakReadyTimeout to answer and as long
// again for music.apple.com's MusicKit, so startup must allow both.
func TestWebPlayerStartTimeoutCoversAFlatpakColdStart(t *testing.T) {
	if want := 2 * webplayer.FlatpakReadyTimeout; webPlayerStartTimeout < want {
		t.Fatalf("webPlayerStartTimeout = %v; want at least %v", webPlayerStartTimeout, want)
	}
}
