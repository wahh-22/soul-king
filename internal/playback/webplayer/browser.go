package webplayer

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// BrowserEnv names the environment variable that overrides discovery with
// an absolute path to a Chrome or Chromium executable.
const BrowserEnv = "NU11SIGNAL_BROWSER"

// BrowserFlagsEnv names the environment variable whose space-separated
// flags Open and Login add to the browser's command line, for containers
// and unusual setups (for example --no-sandbox --ozone-platform=wayland).
const BrowserFlagsEnv = "NU11SIGNAL_BROWSER_FLAGS"

// browserFlags returns BrowserFlagsEnv's flags, each checked as
// Options.ExtraFlags are (see checkFlag); none when it is unset or blank.
func browserFlags(getenv func(string) string) ([]string, error) {
	flags := strings.Fields(getenv(BrowserFlagsEnv))
	for _, f := range flags {
		if err := checkFlag(f); err != nil {
			return nil, fmt.Errorf("webplayer: %s: %w", BrowserFlagsEnv, err)
		}
	}
	return flags, nil
}

// ErrNoBrowser is wrapped by NoBrowserError.
var ErrNoBrowser = errors.New("webplayer: no usable browser")

// Installation is the browser Discover chose.
type Installation struct {
	// Path is the executable to launch: the command found on PATH (often a
	// distribution wrapper script), an install-directory binary, or the
	// BrowserEnv override.
	Path string
	// Binary is the real browser binary Widevine was found for, after
	// symlinks and wrapper scripts.
	Binary string
	// Widevine is the libwidevinecdm.so that makes the browser usable.
	Widevine string
	// Reason explains the choice in one line, for diagnostics.
	Reason string
}

// NoBrowserError reports that no Chrome or Chromium with Widevine for this
// platform was found. It wraps ErrNoBrowser.
type NoBrowserError struct {
	// Arch is the Widevine platform needed, such as "linux_x64".
	Arch string
	// WithoutWidevine lists the browsers found that lack it, in preference
	// order, so the message can say what to fix.
	WithoutWidevine []string
}

func (e *NoBrowserError) Error() string {
	if len(e.WithoutWidevine) == 0 {
		return fmt.Sprintf("webplayer: no Google Chrome or Chromium found; install one with Widevine (%s) or set %s", e.Arch, BrowserEnv)
	}
	return fmt.Sprintf("webplayer: no Google Chrome or Chromium with Widevine for %s; found without Widevine: %s",
		e.Arch, strings.Join(e.WithoutWidevine, ", "))
}

func (e *NoBrowserError) Unwrap() error { return ErrNoBrowser }

// flavor is one browser family: the commands it installs on PATH, the
// binaries its packages put in install directories (which wrapper scripts
// on PATH exec), and its directory under the user's config home, where the
// component updater keeps a downloaded Widevine.
type flavor struct {
	name      string
	commands  []string
	installs  []string
	configDir string
}

// browserFlavors lists the supported browsers in preference order. Snap
// and Flatpak installs are not covered: their browsers run sandboxed with
// their own home and cannot take our pipe and profile as they are.
func browserFlavors() []flavor {
	return []flavor{
		{
			name:      "Google Chrome",
			commands:  []string{"google-chrome", "google-chrome-stable"},
			installs:  []string{"/opt/google/chrome/chrome"},
			configDir: "google-chrome",
		},
		{
			name:     "Chromium",
			commands: []string{"chromium", "chromium-browser"},
			installs: []string{
				"/usr/lib/chromium/chromium",                   // Debian
				"/usr/lib/chromium-browser/chromium-browser",   // Ubuntu (deb)
				"/usr/lib64/chromium-browser/chromium-browser", // Fedora
			},
			configDir: "chromium",
		},
	}
}

// fileSystem is the part of the filesystem Discover reads, so tests can
// root it in a temporary directory.
type fileSystem interface {
	Stat(name string) (fs.FileInfo, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	EvalSymlinks(name string) (string, error)
}

type osFileSystem struct{}

func (osFileSystem) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }
func (osFileSystem) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (osFileSystem) EvalSymlinks(name string) (string, error)   { return filepath.EvalSymlinks(name) }

// discoverEnv is everything Discover depends on.
type discoverEnv struct {
	getenv   func(string) string
	lookPath func(string) (string, error)
	fs       fileSystem
	goarch   string
	home     string
}

// Discover finds a Chrome or Chromium that can play Apple Music: one with
// a Widevine CDM for this platform, bundled next to the real binary
// (WidevineCdm/_platform_specific/linux_<arch>/libwidevinecdm.so) or
// downloaded by the component updater into the browser's config directory
// (~/.config/google-chrome/WidevineCdm/<version>/..., likewise chromium).
//
// BrowserEnv, when set, is the only candidate. Otherwise google-chrome,
// google-chrome-stable, chromium and chromium-browser are looked up on
// PATH, in that order, then the known install directories. Without a
// usable browser the error is a *NoBrowserError.
func Discover() (Installation, error) {
	home, _ := os.UserHomeDir() // without one, only bundled Widevine counts
	return discover(discoverEnv{
		getenv:   os.Getenv,
		lookPath: exec.LookPath,
		fs:       osFileSystem{},
		goarch:   runtime.GOARCH,
		home:     home,
	})
}

func discover(env discoverEnv) (Installation, error) {
	d := discoverer{env: env, platform: widevinePlatform(env.goarch), configHome: configHome(env.getenv, env.home)}
	missing := &NoBrowserError{Arch: d.platform}

	if p := env.getenv(BrowserEnv); p != "" {
		if !filepath.IsAbs(p) {
			return Installation{}, fmt.Errorf("webplayer: %s=%s: not an absolute path", BrowserEnv, p)
		}
		if !d.executable(p) {
			return Installation{}, fmt.Errorf("webplayer: %s=%s: not an executable file", BrowserEnv, p)
		}
		inst, _, ok := d.check(p, flavorOf(p))
		if !ok {
			missing.WithoutWidevine = []string{p}
			return Installation{}, missing
		}
		inst.Reason = "from " + BrowserEnv + ": " + inst.Reason
		return inst, nil
	}

	seen := make(map[string]bool) // binaries already judged
	consider := func(launcher string, fl *flavor) (Installation, bool) {
		inst, checked, ok := d.check(launcher, fl)
		if slices.ContainsFunc(checked, func(b string) bool { return seen[b] }) {
			return Installation{}, false
		}
		for _, b := range checked {
			seen[b] = true
		}
		if !ok {
			missing.WithoutWidevine = append(missing.WithoutWidevine, launcher)
		}
		return inst, ok
	}
	flavors := browserFlavors()
	for i := range flavors {
		for _, name := range flavors[i].commands {
			if p, err := env.lookPath(name); err == nil {
				if inst, ok := consider(p, &flavors[i]); ok {
					return inst, nil
				}
			}
		}
	}
	for i := range flavors {
		for _, bin := range flavors[i].installs {
			if d.executable(bin) {
				if inst, ok := consider(bin, &flavors[i]); ok {
					return inst, nil
				}
			}
		}
	}
	return Installation{}, missing
}

type discoverer struct {
	env        discoverEnv
	platform   string
	configHome string
}

// check looks for Widevine for launcher: next to its real binary (after
// symlinks), next to its flavor's install-directory binaries (which a
// wrapper script would exec), then in the flavor's config directory. It
// returns the binaries it judged, so one install reached through several
// commands is reported once.
func (d discoverer) check(launcher string, fl *flavor) (Installation, []string, bool) {
	real, err := d.env.fs.EvalSymlinks(launcher)
	if err != nil {
		real = launcher
	}
	binaries := []string{real}
	if fl != nil {
		for _, bin := range fl.installs {
			if bin != real && d.executable(bin) {
				binaries = append(binaries, bin)
			}
		}
	}
	name := "browser"
	if fl != nil {
		name = fl.name
	}
	found := func(bin, cdm string) (Installation, []string, bool) {
		reason := fmt.Sprintf("%s at %s", name, launcher)
		if bin != launcher {
			reason += " (binary " + bin + ")"
		}
		reason += " with Widevine " + cdm
		return Installation{Path: launcher, Binary: bin, Widevine: cdm, Reason: reason}, binaries, true
	}
	if d.platform == "" {
		return Installation{}, binaries, false
	}
	for _, bin := range binaries {
		if cdm := d.cdmIn(filepath.Join(filepath.Dir(bin), "WidevineCdm")); cdm != "" {
			return found(bin, cdm)
		}
	}
	if fl != nil && d.configHome != "" {
		if cdm := d.componentCDM(filepath.Join(d.configHome, fl.configDir, "WidevineCdm")); cdm != "" {
			return found(real, cdm)
		}
	}
	return Installation{}, binaries, false
}

// cdmIn returns the platform's libwidevinecdm.so under a WidevineCdm
// directory (or one version of it), or "".
func (d discoverer) cdmIn(dir string) string {
	p := filepath.Join(dir, "_platform_specific", d.platform, "libwidevinecdm.so")
	if fi, err := d.env.fs.Stat(p); err == nil && fi.Mode().IsRegular() {
		return p
	}
	return ""
}

// componentCDM returns the platform's library from the newest version
// directory under a component-updated WidevineCdm directory, or "".
func (d discoverer) componentCDM(dir string) string {
	entries, err := d.env.fs.ReadDir(dir)
	if err != nil {
		return ""
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && isVersion(e.Name()) {
			versions = append(versions, e.Name())
		}
	}
	slices.SortFunc(versions, func(a, b string) int { return compareVersions(b, a) })
	for _, v := range versions {
		if cdm := d.cdmIn(filepath.Join(dir, v)); cdm != "" {
			return cdm
		}
	}
	return ""
}

func (d discoverer) executable(p string) bool {
	fi, err := d.env.fs.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// flavorOf is the family whose command an override is named after (such
// as /usr/bin/google-chrome), so that wrapper script still leads to its
// install and config directories. Any other override only counts with
// Widevine next to its own binary.
func flavorOf(p string) *flavor {
	base := filepath.Base(p)
	flavors := browserFlavors()
	for i := range flavors {
		if slices.Contains(flavors[i].commands, base) {
			return &flavors[i]
		}
	}
	return nil
}

// widevinePlatform is the CDM's platform directory for a GOARCH, or "" when
// Google ships no Linux Widevine for it.
func widevinePlatform(goarch string) string {
	switch goarch {
	case "amd64":
		return "linux_x64"
	case "arm64":
		return "linux_arm64"
	}
	return ""
}

func isVersion(s string) bool {
	if s == "" {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

// compareVersions compares dotted numeric versions part by part.
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if c := cmp.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// configHome is $XDG_CONFIG_HOME when absolute, else ~/.config, else "".
func configHome(getenv func(string) string, home string) string {
	if x := getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return x
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config")
}

// ProfileDir returns the browser profile directory,
// $XDG_CONFIG_HOME/nu11signal/webplayer (or ~/.config/nu11signal/webplayer),
// creating it owner-only (0700). The profile holds the Apple Music session,
// so an existing directory that is a symlink or open to group or others is
// refused rather than repaired: nu11signal does not change permissions it
// did not set; the error says how to fix them.
func ProfileDir() (string, error) {
	home, _ := os.UserHomeDir()
	return profileDir(os.Getenv, home)
}

func profileDir(getenv func(string) string, home string) (string, error) {
	base := configHome(getenv, home)
	if base == "" {
		return "", errors.New("webplayer: no config directory (neither XDG_CONFIG_HOME nor a home directory is set)")
	}
	dir := filepath.Join(base, "nu11signal", "webplayer")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("webplayer: profile directory: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", fmt.Errorf("webplayer: profile directory: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("webplayer: profile directory %s is not a directory (symlinks are refused)", dir)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("webplayer: profile directory %s is open to other users (mode %04o); run chmod 700 %s", dir, perm, dir)
	}
	return dir, nil
}

const (
	// DefaultReadyTimeout bounds Launch's wait for the browser to answer.
	DefaultReadyTimeout = 30 * time.Second
	// DefaultCloseTimeout is Close's grace period after Browser.close.
	DefaultCloseTimeout = 5 * time.Second
	// closeCallTimeout bounds the Browser.close command itself, for a
	// browser that no longer reads its pipe.
	closeCallTimeout = 2 * time.Second
	// outputWaitDelay bounds the wait for Stderr copying after the browser
	// exits, as its children may keep the output open.
	outputWaitDelay = time.Second
)

// Options configures Launch.
type Options struct {
	// Browser is the executable to start (see Discover).
	Browser string
	// Profile is the absolute --user-data-dir (see ProfileDir).
	Profile string
	// URL is the page to open; empty leaves the browser's default.
	URL string
	// Headless runs the browser without a window (--headless=new).
	Headless bool
	// ExtraFlags are added before URL, for example --no-sandbox in a
	// container: each a --flag or --flag=value. Flags that would open a
	// debugging endpoint or change the profile are refused.
	ExtraFlags []string
	// Env is the browser's environment; nil inherits the current one.
	Env []string
	// Stderr, when set, receives the browser's stdout and stderr, which
	// are otherwise discarded.
	Stderr io.Writer
	// OnEvent receives DevTools events (see NewClient); nil drops them.
	OnEvent func(Event)
	// ReadyTimeout bounds the wait for the browser's first answer
	// (default DefaultReadyTimeout).
	ReadyTimeout time.Duration
	// CloseTimeout is Close's grace period between Browser.close and
	// killing the browser (default DefaultCloseTimeout).
	CloseTimeout time.Duration
}

// browserArgs is the browser's command line: DevTools on the pipe only
// (never a port), the given profile, no first-run or keyring prompts, and
// media that keeps playing without a gesture while the page is hidden.
func browserArgs(opts Options) []string {
	args := []string{
		"--remote-debugging-pipe",
		"--user-data-dir=" + opts.Profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--password-store=basic",
		"--autoplay-policy=no-user-gesture-required",
		"--disable-background-media-suspend",
		"--disable-renderer-backgrounding",
		"--disable-background-timer-throttling",
		"--hide-crash-restore-bubble",
	}
	if opts.Headless {
		args = append(args, "--headless=new")
	}
	args = append(args, opts.ExtraFlags...)
	if opts.URL != "" {
		args = append(args, opts.URL)
	}
	return args
}

func validateOptions(opts Options) error {
	switch {
	case opts.Browser == "":
		return errors.New("webplayer: no browser executable")
	case !filepath.IsAbs(opts.Profile):
		return fmt.Errorf("webplayer: profile %q is not an absolute directory", opts.Profile)
	case strings.HasPrefix(opts.URL, "-"):
		return fmt.Errorf("webplayer: URL %q looks like a flag", opts.URL)
	}
	for _, f := range opts.ExtraFlags {
		if err := checkFlag(f); err != nil {
			return fmt.Errorf("webplayer: %w", err)
		}
	}
	return nil
}

// checkFlag accepts an extra flag: a --flag or --flag=value that neither
// opens a debugging endpoint nor changes the profile. Anything else
// could be taken for the URL to open.
func checkFlag(f string) error {
	// Chrome accepts both "-" and "--" before a switch.
	name := strings.TrimLeft(f, "-")
	if strings.HasPrefix(name, "remote-debugging") || strings.HasPrefix(name, "user-data-dir") {
		return fmt.Errorf("extra flag %q is not allowed", f)
	}
	name, _, _ = strings.Cut(name, "=")
	if !strings.HasPrefix(f, "--") || strings.HasPrefix(f, "---") || name == "" {
		return fmt.Errorf("extra flag %q is not a --flag or --flag=value", f)
	}
	return nil
}

// Browser is a running browser driven over its DevTools pipe. It leads its
// own process group, so terminal signals reach only nu11signal and Close
// can stop its helper processes too.
type Browser struct {
	cmd          *exec.Cmd
	pid          int
	client       *Client
	toBrowser    *os.File // our end of the browser's fd 3
	fromBrowser  *os.File // our end of the browser's fd 4
	closeTimeout time.Duration

	done    chan struct{} // closed after the process is reaped and the pipe torn down
	waitErr error         // written before done is closed

	closeOnce sync.Once
	closeErr  error
}

// Launch starts the browser with DevTools on a pipe (fd 3 carries
// commands, fd 4 responses and events; no port is opened) and waits until
// it answers. ctx bounds only the startup; the browser lives until Close
// or until it exits.
func Launch(ctx context.Context, opts Options) (*Browser, error) {
	if err := validateOptions(opts); err != nil {
		return nil, err
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultReadyTimeout
	}
	if opts.CloseTimeout <= 0 {
		opts.CloseTimeout = DefaultCloseTimeout
	}

	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("webplayer: %w", err)
	}
	respR, respW, err := os.Pipe()
	if err != nil {
		cmdR.Close()
		cmdW.Close()
		return nil, fmt.Errorf("webplayer: %w", err)
	}
	cmd := exec.Command(opts.Browser, browserArgs(opts)...)
	cmd.Env = opts.Env
	if opts.Stderr != nil {
		cmd.Stdout = opts.Stderr
		cmd.Stderr = opts.Stderr
		cmd.WaitDelay = outputWaitDelay
	}
	cmd.ExtraFiles = []*os.File{cmdR, respW} // fd 3, fd 4
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	// The browser holds its own copies; ours would keep the pipe open.
	cmdR.Close()
	respW.Close()
	if err != nil {
		cmdW.Close()
		respR.Close()
		return nil, fmt.Errorf("webplayer: start %s: %w", opts.Browser, err)
	}

	b := &Browser{
		cmd:          cmd,
		pid:          cmd.Process.Pid,
		client:       NewClient(respR, cmdW, opts.OnEvent),
		toBrowser:    cmdW,
		fromBrowser:  respR,
		closeTimeout: opts.CloseTimeout,
		done:         make(chan struct{}),
	}
	go b.wait()

	rctx, cancel := context.WithTimeout(ctx, opts.ReadyTimeout)
	defer cancel()
	if _, err := b.client.Call(rctx, "", "Browser.getVersion", nil); err != nil {
		b.kill()
		status := "killed"
		if b.waitErr != nil {
			status = b.waitErr.Error()
		}
		return nil, fmt.Errorf("webplayer: %s did not answer on the DevTools pipe (%s): %w", opts.Browser, status, err)
	}
	return b, nil
}

// Client is the DevTools connection to the browser target.
func (b *Browser) Client() *Client { return b.client }

// PID is the browser's process id, which is also its process group id.
func (b *Browser) PID() int { return b.pid }

// Done is closed once the browser has exited, been reaped, and its pipe is
// closed (Client calls then fail with ErrBrowserGone).
func (b *Browser) Done() <-chan struct{} { return b.done }

// Err is the browser's exit error once Done is closed (nil for a clean
// exit), and nil before.
func (b *Browser) Err() error {
	select {
	case <-b.done:
		return b.waitErr
	default:
		return nil
	}
}

// Close asks the browser to exit with Browser.close and waits up to
// CloseTimeout, or until ctx ends, before killing its process group. It
// then waits for the browser to be reaped. It is idempotent, and returns
// nil when the browser exited on its own (see Err).
func (b *Browser) Close(ctx context.Context) error {
	b.closeOnce.Do(func() { b.closeErr = b.shutdown(ctx) })
	return b.closeErr
}

func (b *Browser) shutdown(ctx context.Context) error {
	select {
	case <-b.done:
		return nil
	default:
	}
	cctx, cancel := context.WithTimeout(ctx, closeCallTimeout)
	_, _ = b.client.Call(cctx, "", "Browser.close", nil) // the pipe may close before the reply
	cancel()

	timer := time.NewTimer(b.closeTimeout)
	defer timer.Stop()
	select {
	case <-b.done:
		return nil
	case <-timer.C:
		b.kill()
		return fmt.Errorf("webplayer: browser did not exit within %s of Browser.close; killed", b.closeTimeout)
	case <-ctx.Done():
		b.kill()
		return fmt.Errorf("webplayer: browser still running when Close was cut short (%w); killed", ctx.Err())
	}
}

// kill stops the browser's whole process group and waits until it is
// reaped.
func (b *Browser) kill() {
	_ = killGroup(b.pid)
	<-b.done
}

// wait reaps the browser, sweeps what is left of its process group (helper
// processes that outlived it), closes our pipe ends so the reader and any
// stuck write end, and only then closes done.
func (b *Browser) wait() {
	err := b.cmd.Wait()
	_ = killGroup(b.pid)
	b.toBrowser.Close()
	b.fromBrowser.Close()
	<-b.client.Done()
	b.waitErr = err
	close(b.done)
}

func killGroup(pgid int) error {
	return syscall.Kill(-pgid, syscall.SIGKILL)
}
