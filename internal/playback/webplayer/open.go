package webplayer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// URL is Apple's web player, which Open and Login load.
const URL = "https://music.apple.com/"

// ErrProfileInUse is returned by Open and Login when another browser
// holds the profile: another nu11signal, or a sign-in window still open.
var ErrProfileInUse = errors.New("webplayer: the Apple Music profile is in use; close the other nu11signal or login window")

// ErrLaunchFailed is wrapped by Open's and Login's error when the browser
// was found but did not start or answer, for another reason than a
// profile in use.
var ErrLaunchFailed = errors.New("webplayer: the browser did not start")

// ErrNotLoaded is wrapped by Open's and Login's error when the browser
// started but music.apple.com did not become ready (offline, for example).
var ErrNotLoaded = errors.New("webplayer: music.apple.com did not load")

const (
	// DefaultLoadTimeout bounds Open's and Login's wait for the page's
	// MusicKit to be ready, within their context.
	DefaultLoadTimeout = 30 * time.Second
	// LoginTimeout bounds how long Login waits for the user to sign in.
	LoginTimeout = 15 * time.Minute
	// loadPollInterval paces the questions while the page loads.
	loadPollInterval = 250 * time.Millisecond
	// loginPollInterval paces the questions while the user signs in.
	loginPollInterval = time.Second
	// singletonLock is the link Chrome keeps in a profile it runs on,
	// pointing at "<hostname>-<pid>" of the process holding it.
	singletonLock = "SingletonLock"
)

// OpenOptions configures Open.
type OpenOptions struct {
	// Stderr, when set, receives the browser's output, which is otherwise
	// discarded.
	Stderr io.Writer
	// Player configures the returned Player (see New).
	Player []Option
}

// launcher is what Open and Login depend on, so tests can stand in for
// the browser.
type launcher struct {
	discover   func() (Installation, error)
	profileDir func() (string, error)
	// inUse reports whether another live browser holds the profile.
	inUse func(profile string) bool
	// getenv reads BrowserFlagsEnv.
	getenv func(string) string
	launch func(ctx context.Context, opts Options) (browser, error)
	attach func(ctx context.Context, c *Client) (Evaluator, error)
	// readyTimeout bounds the wait for MusicKit; readyEvery and
	// loginEvery pace the questions; loginTimeout bounds the sign-in.
	readyTimeout, readyEvery time.Duration
	loginEvery, loginTimeout time.Duration
}

func defaultLauncher() launcher {
	return launcher{
		discover:   Discover,
		profileDir: ProfileDir,
		inUse:      profileLocked,
		getenv:     os.Getenv,
		launch: func(ctx context.Context, opts Options) (browser, error) {
			b, err := Launch(ctx, opts)
			if err != nil {
				return nil, err // a nil interface, not a typed nil *Browser
			}
			return b, nil
		},
		attach: func(ctx context.Context, c *Client) (Evaluator, error) {
			page, err := Attach(ctx, c)
			if err != nil {
				return nil, err
			}
			return page, nil
		},
		readyTimeout: DefaultLoadTimeout,
		readyEvery:   loadPollInterval,
		loginEvery:   loginPollInterval,
		loginTimeout: LoginTimeout,
	}
}

// Open starts the web player: it finds a browser (Discover; without one
// the error wraps ErrNoBrowser), launches it headless in the profile
// (ProfileDir) at URL, waits until the page's MusicKit is ready, and
// hides the page. ctx bounds only this startup. The Player owns the
// browser: its Close stops what it played, then closes the browser.
// Whether the profile is signed in is Authorize's answer (Login signs in).
// A profile another browser holds is ErrProfileInUse; a browser that does
// not start wraps ErrLaunchFailed, a page that does not load ErrNotLoaded.
// BrowserFlagsEnv adds flags to the browser's command line.
func Open(ctx context.Context, opts OpenOptions) (*Player, error) {
	return defaultLauncher().open(ctx, opts)
}

func (l launcher) open(ctx context.Context, opts OpenOptions) (*Player, error) {
	b, page, err := l.start(ctx, true, opts.Stderr)
	if err != nil {
		return nil, err
	}
	p := New(page, opts.Player...)
	p.own(b)
	if _, err := l.waitReady(ctx, p); err != nil {
		_ = p.Close()
		return nil, err
	}
	if err := p.Hide(ctx); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("%w: %w", ErrNotLoaded, err)
	}
	return p, nil
}

// Login opens a visible browser window at URL in the profile for the user
// to sign in to Apple Music, writes what to do to out, and asks the page
// every second whether it is signed in, until it is (then the window
// closes), ctx ends, or LoginTimeout passes; the window is closed in
// every case. Signing in again after a session ended works the same way.
// Like Open, it never reads tokens or cookies: the session stays in the
// profile, where the next Open finds it.
func Login(ctx context.Context, out io.Writer) error {
	return defaultLauncher().login(ctx, out)
}

func (l launcher) login(ctx context.Context, out io.Writer) error {
	b, page, err := l.start(ctx, false, nil)
	if err != nil {
		var missing *NoBrowserError
		if errors.As(err, &missing) {
			fmt.Fprintln(out, missing.Error())
		}
		return err
	}
	// Browser.close shuts the browser down cleanly, so it saves the
	// session to the profile first.
	defer b.Close(context.Background())
	p := New(page)
	defer p.Close()

	authorized, err := l.waitReady(ctx, p)
	if err != nil {
		return err
	}
	if authorized {
		fmt.Fprintln(out, "Already signed in to Apple Music. Run nu11signal to play.")
		return nil
	}
	fmt.Fprintln(out, "Sign in to Apple Music in the window that opened; it closes by itself when you are signed in.")
	wait, cancel := context.WithTimeout(ctx, l.loginTimeout)
	defer cancel()
	for {
		if err := sleep(wait, l.loginEvery); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("webplayer: sign-in cancelled: %w", ctx.Err())
			}
			return fmt.Errorf("webplayer: not signed in within %s", l.loginTimeout)
		}
		_, authorized, err := p.status(wait)
		if err != nil && wait.Err() == nil {
			return fmt.Errorf("webplayer: the sign-in window closed: %w", err)
		}
		if authorized {
			fmt.Fprintln(out, "Signed in. Run nu11signal to play.")
			return nil
		}
	}
}

// start launches the browser, headless or not, in the profile at URL and
// attaches to its page, with BrowserFlagsEnv's flags. A profile another
// live browser holds, before the launch or after a browser that failed to
// start was closed, is ErrProfileInUse; any other launch or attach
// failure wraps ErrLaunchFailed.
func (l *launcher) start(ctx context.Context, headless bool, stderr io.Writer) (browser, Evaluator, error) {
	inst, err := l.discover()
	if err != nil {
		return nil, nil, err
	}
	flags, err := browserFlags(l.getenv)
	if err != nil {
		return nil, nil, err
	}
	var profile string
	if inst.Flatpak != "" {
		profile, err = ensureProfileDir(inst.Profile)
		l.readyTimeout = max(l.readyTimeout, FlatpakReadyTimeout)
	} else {
		profile, err = l.profileDir()
	}
	if err != nil {
		return nil, nil, err
	}
	if l.inUse(profile) {
		return nil, nil, ErrProfileInUse
	}
	readyTimeout := DefaultReadyTimeout
	if inst.Flatpak != "" {
		readyTimeout = FlatpakReadyTimeout
	}
	b, err := l.launch(ctx, Options{Browser: inst.Path, Flatpak: inst.Flatpak, Profile: profile, URL: URL, Headless: headless, ExtraFlags: flags, Stderr: stderr, ReadyTimeout: readyTimeout})
	if err != nil {
		// A browser started on a profile held elsewhere hands over to
		// that one and exits, which Launch reports as no answer.
		if l.inUse(profile) {
			return nil, nil, ErrProfileInUse
		}
		return nil, nil, fmt.Errorf("%w: %w", ErrLaunchFailed, err)
	}
	page, err := l.attach(ctx, b.Client())
	if err != nil {
		_ = b.Close(context.Background())
		if l.inUse(profile) {
			return nil, nil, ErrProfileInUse
		}
		return nil, nil, fmt.Errorf("%w: %w", ErrLaunchFailed, err)
	}
	return b, page, nil
}

// waitReady asks the page until its MusicKit is ready, and returns
// whether it is signed in. It gives up, with ErrNotLoaded, when ctx ends,
// after readyTimeout, or when the browser is gone.
func (l launcher) waitReady(ctx context.Context, p *Player) (bool, error) {
	wait, cancel := context.WithTimeout(ctx, l.readyTimeout)
	defer cancel()
	for {
		ready, authorized, err := p.status(wait)
		switch {
		case ready:
			return authorized, nil
		case err != nil && wait.Err() == nil:
			return false, fmt.Errorf("%w: %w", ErrNotLoaded, err)
		}
		if err := sleep(wait, l.readyEvery); err != nil {
			if ctx.Err() != nil {
				return false, fmt.Errorf("%w: %w", ErrNotLoaded, ctx.Err())
			}
			return false, fmt.Errorf("%w within %s", ErrNotLoaded, l.readyTimeout)
		}
	}
}

// profileLocked reports whether the profile's SingletonLock names a
// browser that still holds it: a live process on this computer, or any
// process on another one sharing the directory (which Chrome refuses
// too). A lock a dead process left behind is not held: Chrome takes it
// over.
func profileLocked(profile string) bool {
	target, err := os.Readlink(filepath.Join(profile, singletonLock))
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(target, '-')
	if i <= 0 {
		return false
	}
	pid, err := strconv.Atoi(target[i+1:])
	if err != nil || pid <= 0 {
		return false
	}
	if host, err := os.Hostname(); err == nil && target[:i] != host {
		return true
	}
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
