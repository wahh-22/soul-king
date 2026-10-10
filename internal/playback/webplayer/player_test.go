package webplayer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wahh-22/nu11signal/internal/playback"
)

// call is one namespace call the fake page received.
type call struct{ fn, args string }

func (c call) String() string { return c.fn + "(" + c.args + ")" }

// callPattern finds the namespace call inside a call expression. Greedy,
// so arguments holding the same text cannot end it early.
var callPattern = regexp.MustCompile(`(?s)` + regexp.QuoteMeta(namespace) + `\.(\w+)\((.*)\)\)\.then\(`)

// fakePage stands in for the page: it records every expression and
// answers the bootstrap, and each call through its handler.
type fakePage struct {
	mu       sync.Mutex
	exprs    []string
	calls    []call
	installs int
	// missing answers that many calls "missing", as a reloaded page.
	missing int
	// handle answers a call; nil answers null.
	handle func(c call) (any, error)
	// gate, when set, holds every call until a value is sent on it;
	// waiting gets a value as each call starts to wait.
	gate    chan struct{}
	waiting chan struct{}
}

func newPage() *fakePage { return &fakePage{waiting: make(chan struct{}, 64)} }

func (f *fakePage) Evaluate(ctx context.Context, expr string) (json.RawMessage, error) {
	f.mu.Lock()
	f.exprs = append(f.exprs, expr)
	if expr == bootstrap {
		f.installs++
		f.mu.Unlock()
		return json.RawMessage(`{"installed":true}`), nil
	}
	m := callPattern.FindStringSubmatch(expr)
	if m == nil {
		f.mu.Unlock()
		return nil, errors.New("fake page: unknown expression")
	}
	c := call{fn: m[1], args: m[2]}
	if f.missing > 0 {
		f.missing--
		f.mu.Unlock()
		return json.RawMessage(`{"missing":true}`), nil
	}
	f.calls = append(f.calls, c)
	handle, gate := f.handle, f.gate
	f.mu.Unlock()
	if gate != nil && c.fn != "now" && c.fn != "status" {
		f.waiting <- struct{}{}
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var v any
	if handle != nil {
		var err error
		if v, err = handle(c); err != nil {
			return nil, err
		}
	}
	b, err := json.Marshal(map[string]any{"value": v})
	return b, err
}

// Calls lists the calls received, but now and status polls.
func (f *fakePage) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.fn != "now" && c.fn != "status" {
			out = append(out, c.String())
		}
	}
	return out
}

func (f *fakePage) setHandle(h func(c call) (any, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handle = h
}

// nowReply is what now() answers for a playing song.
func nowReply(status string) map[string]any {
	return map[string]any{
		"status": status, "title": "Song", "artist": "Artist", "album": "Album", "songID": "222",
		"positionS": 61.5, "durationS": 215.0, "repeat": "off", "volume": 0.5,
	}
}

// replying answers now() with np and every other call with null.
func replying(np map[string]any) func(c call) (any, error) {
	return func(c call) (any, error) {
		if c.fn == "now" {
			return np, nil
		}
		return nil, nil
	}
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ticker drives the poller one poll per tick. Each tick carries a
// channel the poller closes when it waits again, its poll handled.
type ticker struct{ ticks chan chan struct{} }

func newTicker() *ticker { return &ticker{ticks: make(chan chan struct{})} }

// option makes the player poll on t's ticks, on clock's time.
func (t *ticker) option(clock *fakeClock) Option {
	return func(p *Player) {
		p.now = clock.Now
		var handled chan struct{} // only the poller uses it
		p.pause = func(ctx context.Context, _ time.Duration) error {
			if handled != nil {
				close(handled)
			}
			select {
			case handled = <-t.ticks:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// tick lets the poller run one poll and waits until it was handled.
func (t *ticker) tick(tb testing.TB) {
	tb.Helper()
	handled := make(chan struct{})
	select {
	case t.ticks <- handled:
	case <-time.After(2 * time.Second):
		tb.Fatal("the poller is not waiting")
	}
	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		tb.Fatal("the poll did not finish")
	}
}

func nextState(t *testing.T, ch <-chan playback.State) playback.State {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(2 * time.Second):
		t.Fatal("no state")
		return playback.State{}
	}
}

// drainStates returns the states waiting on p's States.
func drainStates(p *Player) []playback.State {
	var out []playback.State
	for {
		select {
		case s := <-p.States():
			out = append(out, s)
		default:
			return out
		}
	}
}

func TestPlaySongsPlaysTheQueueAndReportsThePage(t *testing.T) {
	page, clock, tk := newPage(), newClock(), newTicker()
	page.setHandle(replying(nowReply("playing")))
	p := New(page, tk.option(clock))
	defer p.Close()
	report, err := p.PlaySongs(context.Background(), []string{"111", "222", "333"}, 1)
	if err != nil || !report.Clean() {
		t.Fatalf("PlaySongs = %+v, %v; want a clean play", report, err)
	}
	if got, want := page.Calls(), []string{`play(["111","222","333"],1)`}; !slices.Equal(got, want) {
		t.Fatalf("page calls = %v, want %v", got, want)
	}
	if s := nextState(t, p.States()); s.Status != playback.StatusPlaying || s.SongID != "222" {
		t.Fatalf("state = %+v, want playing 222", s)
	}
	clock.Add(5 * time.Second)
	tk.tick(t)
	want := playback.State{
		Status: playback.StatusPlaying, Title: "Song", Artist: "Artist", Album: "Album", SongID: "222",
		Duration: 215 * time.Second, Position: 61500 * time.Millisecond, Repeat: playback.RepeatOff, VolumeMode: playback.VolumeApp,
	}
	if got := nextState(t, p.States()); got != want {
		t.Errorf("state = %+v\nwant %+v", got, want)
	}
}

// playedPlayer is a player over a fake page that played song 111 (its
// state drained) and polls on the ticker's ticks, 10s later on the clock.
func playedPlayer(t *testing.T) (*Player, *fakePage, *fakeClock, *ticker) {
	t.Helper()
	page, clock, tk := newPage(), newClock(), newTicker()
	p := New(page, tk.option(clock))
	t.Cleanup(func() { _ = p.Close() })
	if _, err := p.PlaySongs(context.Background(), []string{"111"}, 0); err != nil {
		t.Fatalf("PlaySongs: %v", err)
	}
	if s := drainStates(p); len(s) != 1 {
		t.Fatalf("states = %+v, want the play's", s)
	}
	clock.Add(10 * time.Second)
	return p, page, clock, tk
}

// song is a now() report of song 111 at 30s of 200s.
func song(status string) map[string]any {
	return map[string]any{
		"status": status, "title": "Song", "artist": "Artist", "album": "Album", "songID": "111",
		"positionS": 30.0, "durationS": 200.0, "repeat": "off", "volume": nil,
	}
}

func TestBootstrapIsInstalledOnceAndAfterAReload(t *testing.T) {
	page := newPage()
	p := New(page, WithPollInterval(time.Hour))
	defer p.Close()
	ctx := context.Background()
	for range 3 {
		if err := p.Pause(ctx); err != nil {
			t.Fatalf("Pause: %v", err)
		}
	}
	if page.installs != 1 {
		t.Fatalf("installs = %d, want 1", page.installs)
	}
	if page.exprs[0] != bootstrap {
		t.Errorf("first expression is not the bootstrap")
	}
	// The page reloaded: the namespace is gone.
	page.missing = 1
	if err := p.Pause(ctx); err != nil {
		t.Fatalf("Pause after a reload: %v", err)
	}
	if page.installs != 2 {
		t.Errorf("installs = %d, want 2", page.installs)
	}
	if got, want := page.Calls(), []string{"pause()", "pause()", "pause()", "pause()"}; !slices.Equal(got, want) {
		t.Errorf("page calls = %v, want %v", got, want)
	}
	// A page that does not keep it is an error, not a loop.
	page.missing = 2
	if err := p.Pause(ctx); !errors.Is(err, errNotInstalled) {
		t.Errorf("Pause on a page that drops the script = %v, want errNotInstalled", err)
	}
	if page.installs != 3 {
		t.Errorf("installs = %d, want 3", page.installs)
	}
}

func TestHideIsAppliedAgainAfterAReload(t *testing.T) {
	page := newPage()
	p := New(page, WithPollInterval(time.Hour))
	defer p.Close()
	ctx := context.Background()
	if err := p.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := p.Hide(ctx); err != nil {
		t.Fatalf("Hide: %v", err)
	}
	page.missing = 1
	if err := p.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if got, want := page.Calls(), []string{"pause()", "hide()", "hide()", "pause()"}; !slices.Equal(got, want) {
		t.Errorf("page calls = %v, want %v", got, want)
	}
}

func TestAuthorizeReportsThePage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply any
		err   error
		want  playback.AuthStatus
		fails bool
	}{
		{name: "signed in", reply: map[string]bool{"ready": true, "authorized": true}, want: playback.AuthAuthorized},
		{name: "not signed in", reply: map[string]bool{"ready": true, "authorized": false}, want: playback.AuthNotDetermined},
		{name: "not ready", reply: map[string]bool{"ready": false, "authorized": false}, want: playback.AuthNotDetermined},
		{name: "malformed", reply: "yes", want: playback.AuthNotDetermined},
		{name: "between documents", err: &CDPError{Method: "Runtime.evaluate", Message: "Execution context was destroyed."}, want: playback.AuthNotDetermined},
		{name: "script error", err: &ScriptError{Description: "TypeError"}, want: playback.AuthNotDetermined},
		{name: "browser gone", err: ErrBrowserGone, fails: true},
	} {
		page := newPage()
		page.setHandle(func(call) (any, error) { return tc.reply, tc.err })
		p := New(page)
		got, err := p.Authorize(context.Background())
		switch {
		case tc.fails && !errors.Is(err, ErrBrowserGone):
			t.Errorf("%s: Authorize = %q, %v; want ErrBrowserGone", tc.name, got, err)
		case !tc.fails && (err != nil || got != tc.want):
			t.Errorf("%s: Authorize = %q, %v; want %q", tc.name, got, err, tc.want)
		}
		if got := page.calls; len(got) != 1 || got[0].fn != "status" || got[0].args != "" {
			t.Errorf("%s: page calls = %v, want one status()", tc.name, got)
		}
		_ = p.Close()
	}
	if !New(newPage()).AuthorizesLate() {
		t.Error("AuthorizesLate = false, want true")
	}
}

func TestControlsCallThePage(t *testing.T) {
	page := newPage()
	p := New(page, WithPollInterval(time.Hour))
	defer p.Close()
	ctx := context.Background()
	for name, err := range map[string]error{
		"Stop before a play": p.Stop(ctx),
	} {
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	steps := []struct {
		name string
		do   func() error
	}{
		{"PlaySongs", func() error { _, err := p.PlaySongs(ctx, []string{"111"}, 0); return err }},
		{"Pause", func() error { return p.Pause(ctx) }},
		{"Resume", func() error { return p.Resume(ctx) }},
		{"Next", func() error { return p.Next(ctx) }},
		{"Previous", func() error { return p.Previous(ctx) }},
		{"Seek", func() error { return p.Seek(ctx, 90500*time.Millisecond) }},
		{"Seek before the start", func() error { return p.Seek(ctx, -time.Second) }},
		{"SetRepeat", func() error { return p.SetRepeat(ctx, playback.RepeatOne) }},
		{"SetVolume", func() error { return p.SetVolume(ctx, 0.256) }},
		{"SetVolume above full", func() error { return p.SetVolume(ctx, 7) }},
		{"Stop", func() error { return p.Stop(ctx) }},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
	}
	want := []string{
		`play(["111"],0)`, "pause()", "resume()", "next()", "previous()", "seek(90.5)", "seek(0)",
		`repeat("one")`, "volume(0.26)", "volume(1)", "stop()",
	}
	if got := page.Calls(); !slices.Equal(got, want) {
		t.Errorf("page calls = %v\nwant %v", got, want)
	}
	if err := p.SetRepeat(ctx, "shuffle"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("SetRepeat(shuffle) = %v, want ErrInvalidArgument", err)
	}
	if got := page.Calls(); len(got) != len(want) {
		t.Errorf("an invalid repeat mode reached the page: %v", got[len(want):])
	}
}

func TestFailedCommandIsAnErrorAndEmitsNothing(t *testing.T) {
	page := newPage()
	page.setHandle(func(call) (any, error) { return nil, &ScriptError{Description: "Error: pause failed: NOT_READY"} })
	p := New(page, WithPollInterval(time.Hour))
	defer p.Close()
	err := p.Pause(context.Background())
	var se *ScriptError
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "webplayer pause") {
		t.Errorf("Pause = %v, want a wrapped ScriptError", err)
	}
	if s := drainStates(p); len(s) != 0 {
		t.Errorf("states = %+v, want none", s)
	}
}

func TestPlaySongsQueuesOnlyCatalogSongs(t *testing.T) {
	page := newPage()
	p := New(page, WithPollInterval(time.Hour))
	defer p.Close()
	ctx := context.Background()
	ids := []string{"i.Library", "111", "local:/a.mp3", "222", "333"}
	report, err := p.PlaySongs(ctx, ids, 3)
	if err != nil {
		t.Fatalf("PlaySongs: %v", err)
	}
	if want := []string{"i.Library", "local:/a.mp3"}; !slices.Equal(report.Skipped, want) || report.StartedAlone || len(report.Missing) != 0 {
		t.Errorf("report = %+v, want Skipped %v", report, want)
	}
	if got, want := page.Calls(), []string{`play(["111","222","333"],1)`}; !slices.Equal(got, want) {
		t.Errorf("page calls = %v, want %v", got, want)
	}
	if s := drainStates(p); len(s) != 1 || s[0].SongID != "222" || s[0].Status != playback.StatusPlaying {
		t.Errorf("states = %+v, want playing 222", s)
	}
	// No queue limit: the page takes the whole queue.
	long := make([]string, 200)
	for i := range long {
		long[i] = strconv.Itoa(1000 + i)
	}
	if report, err := p.PlaySongs(ctx, long, 150); err != nil || !report.Clean() {
		t.Fatalf("PlaySongs(200 songs) = %+v, %v", report, err)
	}
	last := page.calls[len(page.calls)-1]
	var queue []string
	if err := json.Unmarshal([]byte("["+last.args+"]"), &[]any{&queue, new(int)}); err != nil || !slices.Equal(queue, long) {
		t.Errorf("play args = %.80s…, want all 200 songs (%v)", last.args, err)
	}
	for _, tc := range []struct {
		ids   []string
		start int
	}{{nil, 0}, {[]string{"111"}, 1}, {[]string{"111"}, -1}, {[]string{"i.Library", "111"}, 0}, {[]string{`1"); stop(); ("`}, 0}} {
		before := len(page.Calls())
		if _, err := p.PlaySongs(ctx, tc.ids, tc.start); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("PlaySongs(%q, %d) = %v, want ErrInvalidArgument", tc.ids, tc.start, err)
		}
		if got := page.Calls(); len(got) != before {
			t.Errorf("PlaySongs(%q, %d) reached the page: %v", tc.ids, tc.start, got[before:])
		}
	}
}

func TestTransportStates(t *testing.T) {
	p, page, clock, tk := playedPlayer(t)
	page.setHandle(replying(song("playing")))
	tk.tick(t)
	drainStates(p)
	ctx := context.Background()
	clock.Add(2 * time.Second)
	if err := p.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if s := drainStates(p); len(s) != 1 || s[0].Status != playback.StatusPaused || s[0].Title != "Song" || s[0].Position != 32*time.Second {
		t.Fatalf("states after Pause = %+v, want paused Song at 32s", s)
	}
	if err := p.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if s := drainStates(p); len(s) != 1 || s[0].Status != playback.StatusPlaying || s[0].Title != "Song" {
		t.Fatalf("states after Resume = %+v, want playing Song", s)
	}
	if err := p.Seek(ctx, 500*time.Second); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if s := drainStates(p); len(s) != 1 || s[0].Position != 200*time.Second {
		t.Fatalf("states after Seek = %+v, want the position at the song's end", s)
	}
	if err := p.SetRepeat(ctx, playback.RepeatAll); err != nil {
		t.Fatalf("SetRepeat: %v", err)
	}
	if s := drainStates(p); len(s) != 1 || s[0].Repeat != playback.RepeatAll {
		t.Fatalf("states after SetRepeat = %+v, want repeat all", s)
	}
	if err := p.Next(ctx); err != nil {
		t.Fatalf("Next: %v", err)
	}
	if s := drainStates(p); len(s) != 0 {
		t.Fatalf("states after Next = %+v, want none until the page reports", s)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if s := drainStates(p); len(s) != 1 || s[0].Status != playback.StatusStopped {
		t.Fatalf("states after Stop = %+v, want stopped", s)
	}
}

func TestNowPlayingStatuses(t *testing.T) {
	p, page, _, tk := playedPlayer(t)
	for _, tc := range []struct {
		report string
		want   playback.Status
	}{
		{"paused", playback.StatusPaused},
		{"playing", playback.StatusPlaying},
		{"seeking", playback.StatusSeeking},
		{"loading", playback.StatusSeeking}, // keeps the last one
		{"unknown", playback.StatusSeeking},
		{"stopped", playback.StatusStopped},
		{"bogus", playback.StatusStopped},
	} {
		page.setHandle(replying(song(tc.report)))
		tk.tick(t)
		if got := p.lastState().Status; got != tc.want {
			t.Errorf("after %q: status %q, want %q", tc.report, got, tc.want)
		}
	}
}

// lastState is the last state p emitted.
func (p *Player) lastState() playback.State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func TestNowPlayingTextIsSanitized(t *testing.T) {
	p, page, _, tk := playedPlayer(t)
	np := song("playing")
	np["title"] = " \u202eEvil\x1b[2J\u200b Title "
	np["artist"] = "Art\x07ist"
	np["songID"] = "i.NotCatalog"
	page.setHandle(replying(np))
	tk.tick(t)
	s := nextState(t, p.States())
	// Without a catalog id the report names no other song: 111 stays.
	if s.Title != "Evil[2J Title" || s.Artist != "Artist" || s.SongID != "111" {
		t.Errorf("state = %+v, want sanitized text on song 111", s)
	}
}

func TestNowPlayingEmitsOnlyChanges(t *testing.T) {
	p, page, clock, tk := playedPlayer(t)
	np := song("playing")
	page.setHandle(replying(np))
	tk.tick(t)
	if s := drainStates(p); len(s) != 1 || s[0].Title != "Song" || s[0].Position != 30*time.Second {
		t.Fatalf("states = %+v, want Song at 30s", s)
	}
	// One second on, the page at 31.5s: within the drift of 31s.
	clock.Add(time.Second)
	np["positionS"] = 31.5
	tk.tick(t)
	if s := drainStates(p); len(s) != 0 {
		t.Fatalf("states = %+v, want none within the drift", s)
	}
	// The page jumped: 10s more than the UI estimates.
	clock.Add(time.Second)
	np["positionS"] = 42.0
	tk.tick(t)
	if s := drainStates(p); len(s) != 1 || s[0].Position != 42*time.Second {
		t.Fatalf("states = %+v, want one at 42s", s)
	}
	// Another song.
	np = song("playing")
	np["songID"], np["title"], np["album"] = "999", "Other", ""
	page.setHandle(replying(np))
	tk.tick(t)
	if s := drainStates(p); len(s) != 1 || s[0].SongID != "999" || s[0].Title != "Other" || s[0].Album != "" {
		t.Fatalf("states = %+v, want song 999 without the last album", s)
	}
}

func TestPollerReportsRepeatAndVolume(t *testing.T) {
	p, page, _, tk := playedPlayer(t)
	np := song("playing")
	np["repeat"], np["volume"] = "all", 0.4
	page.setHandle(replying(np))
	tk.tick(t)
	if s := nextState(t, p.States()); s.Repeat != playback.RepeatAll {
		t.Errorf("state = %+v, want repeat all", s)
	}
	np["repeat"], np["volume"] = "", 1.5 // unknown: kept
	tk.tick(t)
	if s := p.lastState(); s.Repeat != playback.RepeatAll {
		t.Errorf("state = %+v, want repeat all kept", s)
	}
	p.mu.Lock()
	v := p.volume
	p.mu.Unlock()
	if v != 0.4 {
		t.Errorf("volume = %v, want 0.4", v)
	}
}

func TestStaleReportsDoNotUndoACommand(t *testing.T) {
	p, page, clock, tk := playedPlayer(t)
	page.setHandle(replying(song("playing")))
	tk.tick(t)
	drainStates(p)
	// The report is read while a pause applies: it predates the pause.
	var once sync.Once
	page.setHandle(func(c call) (any, error) {
		if c.fn != "now" {
			return nil, nil
		}
		once.Do(func() {
			clock.Add(100 * time.Millisecond)
			if err := p.Pause(context.Background()); err != nil {
				t.Errorf("Pause: %v", err)
			}
		})
		return song("playing"), nil
	})
	tk.tick(t)
	if s := drainStates(p); len(s) != 1 || s[0].Status != playback.StatusPaused {
		t.Fatalf("states = %+v, want only the pause's", s)
	}
	// A later report: the page was resumed from its own controls.
	clock.Add(time.Second)
	tk.tick(t)
	if s := drainStates(p); len(s) != 1 || s[0].Status != playback.StatusPlaying {
		t.Errorf("states = %+v, want playing", s)
	}
}

// nextErr waits for an error on p's Errors.
func nextErr(t *testing.T, p *Player) error {
	t.Helper()
	select {
	case err := <-p.Errors():
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("no error")
		return nil
	}
}

func TestBrowserGoneWhilePlayingIsStopped(t *testing.T) {
	p, page, _, tk := playedPlayer(t)
	page.setHandle(func(call) (any, error) { return nil, fmt.Errorf("cdp: %w", ErrBrowserGone) })
	tk.tick(t)
	if s := drainStates(p); len(s) != 1 || s[0].Status != playback.StatusStopped || s[0].SongID != "111" {
		t.Errorf("states = %+v, want 111 stopped", s)
	}
	if err := nextErr(t, p); !errors.Is(err, ErrBrowserGone) {
		t.Errorf("error = %v, want ErrBrowserGone", err)
	}
	tk.tick(t)
	select {
	case err := <-p.Errors():
		t.Errorf("reported again: %v", err)
	default:
	}
}

func TestPollerIsQuietBetweenDocuments(t *testing.T) {
	p, page, _, tk := playedPlayer(t)
	page.setHandle(func(call) (any, error) {
		return nil, &CDPError{Method: "Runtime.evaluate", Message: "Execution context was destroyed."}
	})
	tk.tick(t)
	page.setHandle(func(call) (any, error) { return "garbage", nil })
	tk.tick(t)
	if err := nextErr(t, p); !errors.Is(err, errMalformed) {
		t.Errorf("error = %v, want errMalformed (and nothing for the reload)", err)
	}
	if s := drainStates(p); len(s) != 0 {
		t.Errorf("states = %+v, want none", s)
	}
}

func TestPollerStartsWithAPlay(t *testing.T) {
	page, clock, tk := newPage(), newClock(), newTicker()
	p := New(page, tk.option(clock))
	ctx := context.Background()
	_, _ = p.Authorize(ctx)
	_ = p.Pause(ctx)
	select {
	case tk.ticks <- make(chan struct{}):
		t.Fatal("the poller runs before a play")
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := p.PlaySongs(ctx, []string{"111"}, 0); err != nil {
		t.Fatalf("PlaySongs: %v", err)
	}
	tk.tick(t)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	p.mu.Lock()
	polled := p.polled
	p.mu.Unlock()
	select {
	case <-polled:
	default:
		t.Error("the poller outlived Close")
	}
}

func TestVolume(t *testing.T) {
	page := newPage()
	p := New(page, WithPollInterval(time.Hour))
	defer p.Close()
	ctx := context.Background()
	np := song("paused")
	page.setHandle(replying(np))
	if _, err := p.Volume(ctx); !errors.Is(err, ErrVolumeUnknown) {
		t.Errorf("Volume with none reported = %v, want ErrVolumeUnknown", err)
	}
	np["volume"] = 0.3
	if v, err := p.Volume(ctx); err != nil || v != 0.3 {
		t.Errorf("Volume = %v, %v; want 0.3", v, err)
	}
	if err := p.SetVolume(ctx, 0.8); err != nil {
		t.Fatalf("SetVolume: %v", err)
	}
	np["volume"] = nil
	if v, err := p.Volume(ctx); err != nil || v != 0.8 {
		t.Errorf("Volume after SetVolume = %v, %v; want 0.8", v, err)
	}
	page.setHandle(func(call) (any, error) { return nil, ErrBrowserGone })
	if v, err := p.Volume(ctx); err != nil || v != 0.8 {
		t.Errorf("Volume with the page gone = %v, %v; want the last 0.8", v, err)
	}
}

// gatedPlayer is a player over a page that holds each command until the
// test sends on its gate.
func gatedPlayer(t *testing.T) (*Player, *fakePage) {
	t.Helper()
	page := newPage()
	page.gate = make(chan struct{})
	p := New(page, WithPollInterval(time.Hour), WithStopTimeout(20*time.Millisecond))
	t.Cleanup(func() { _ = p.Close() })
	return p, page
}

// waiting waits for the page's next held command.
func waiting(t *testing.T, page *fakePage) {
	t.Helper()
	select {
	case <-page.waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("no command waiting")
	}
}

// result waits for a call's answer on ch.
func result(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("call never answered")
		return nil
	}
}

// async runs call in the background, its answer on the channel returned.
func async(call func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- call() }()
	return ch
}

// queuedIn waits until queue holds a call other than prev, and returns it.
func queuedIn(t *testing.T, p *Player, queue **queued, prev *queued) *queued {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		q := *queue
		p.mu.Unlock()
		if q != nil && q != prev {
			return q
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("nothing queued")
	return nil
}

func TestRapidSeeksSendTheFirstAndTheLast(t *testing.T) {
	p, page := gatedPlayer(t)
	ctx := context.Background()
	first := async(func() error { return p.Seek(ctx, 10*time.Second) })
	waiting(t, page)
	second := async(func() error { return p.Seek(ctx, 20*time.Second) })
	q := queuedIn(t, p, &p.seeks, nil)
	third := async(func() error { return p.Seek(ctx, 30*time.Second) })
	queuedIn(t, p, &p.seeks, q)
	if err := result(t, second); err != nil {
		t.Errorf("superseded Seek = %v, want nil", err)
	}
	page.gate <- struct{}{}
	if err := result(t, first); err != nil {
		t.Errorf("first Seek = %v", err)
	}
	waiting(t, page)
	page.gate <- struct{}{}
	if err := result(t, third); err != nil {
		t.Errorf("last Seek = %v", err)
	}
	if got, want := page.Calls(), []string{"seek(10)", "seek(30)"}; !slices.Equal(got, want) {
		t.Errorf("page calls = %v, want %v", got, want)
	}
	if s := drainStates(p); len(s) != 2 || s[1].Position != 30*time.Second {
		t.Errorf("states = %+v, want the first seek's, then the last one's", s)
	}
}

func TestRapidVolumeChangesSendTheFirstAndTheLast(t *testing.T) {
	p, page := gatedPlayer(t)
	ctx := context.Background()
	first := async(func() error { return p.SetVolume(ctx, 0.5) })
	waiting(t, page)
	second := async(func() error { return p.SetVolume(ctx, 0.6) })
	q := queuedIn(t, p, &p.levels, nil)
	third := async(func() error { return p.SetVolume(ctx, 0.7) })
	queuedIn(t, p, &p.levels, q)
	if err := result(t, second); err != nil {
		t.Errorf("superseded SetVolume = %v, want nil", err)
	}
	page.gate <- struct{}{}
	if err := result(t, first); err != nil {
		t.Errorf("first SetVolume = %v", err)
	}
	waiting(t, page)
	page.gate <- struct{}{}
	if err := result(t, third); err != nil {
		t.Errorf("last SetVolume = %v", err)
	}
	if got, want := page.Calls(), []string{"volume(0.5)", "volume(0.7)"}; !slices.Equal(got, want) {
		t.Errorf("page calls = %v, want %v", got, want)
	}
	p.mu.Lock()
	level := p.volume
	p.mu.Unlock()
	if level != 0.7 {
		t.Errorf("volume = %v, want 0.7", level)
	}
}

func TestCommandsWaitForAPendingCommand(t *testing.T) {
	p, page := gatedPlayer(t)
	ctx := context.Background()
	pause := async(func() error { return p.Pause(ctx) })
	waiting(t, page)
	resume := async(func() error { return p.Resume(ctx) })
	select {
	case err := <-resume:
		t.Fatalf("Resume while a pause is pending = %v, want it to wait", err)
	case <-time.After(50 * time.Millisecond):
	}
	page.gate <- struct{}{}
	if err := result(t, pause); err != nil {
		t.Errorf("Pause = %v", err)
	}
	waiting(t, page)
	page.gate <- struct{}{}
	if err := result(t, resume); err != nil {
		t.Errorf("Resume = %v", err)
	}
	if got, want := page.Calls(), []string{"pause()", "resume()"}; !slices.Equal(got, want) {
		t.Errorf("page calls = %v, want %v", got, want)
	}
}

func TestContextEndsAWaitForAPendingCommand(t *testing.T) {
	p, page := gatedPlayer(t)
	pause := async(func() error { return p.Pause(context.Background()) })
	waiting(t, page)
	ctx, cancel := context.WithCancel(context.Background())
	seek := async(func() error { return p.Seek(ctx, time.Second) })
	queuedIn(t, p, &p.seeks, nil)
	cancel()
	if err := result(t, seek); !errors.Is(err, context.Canceled) {
		t.Errorf("queued Seek after cancel = %v, want context.Canceled", err)
	}
	page.gate <- struct{}{}
	if err := result(t, pause); err != nil {
		t.Errorf("Pause = %v", err)
	}
	if got, want := page.Calls(), []string{"pause()"}; !slices.Equal(got, want) {
		t.Errorf("page calls = %v, want %v", got, want)
	}
}

func TestStopFailureIsReported(t *testing.T) {
	p, page, _, _ := playedPlayer(t)
	page.setHandle(func(c call) (any, error) { return nil, &ScriptError{Description: "Error: stop failed: x"} })
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop = %v, want nil (best effort)", err)
	}
	var se *ScriptError
	if err := nextErr(t, p); !errors.As(err, &se) {
		t.Errorf("error = %v, want the stop's ScriptError", err)
	}
}

func TestCloseStopsWhatItPlayed(t *testing.T) {
	page := newPage()
	p := New(page, WithPollInterval(time.Hour))
	if _, err := p.PlaySongs(context.Background(), []string{"111"}, 0); err != nil {
		t.Fatalf("PlaySongs: %v", err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := p.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
	}
	wg.Wait()
	if got, want := page.Calls(), []string{`play(["111"],0)`, "stop()"}; !slices.Equal(got, want) {
		t.Errorf("page calls = %v, want %v", got, want)
	}
	for range p.States() { // the play's state, then closed
	}
	if _, ok := <-p.Errors(); ok {
		t.Error("Errors still open")
	}
}

func TestCloseSendsNoStopWithoutAPlay(t *testing.T) {
	page := newPage()
	p := New(page)
	ctx := context.Background()
	_, _ = p.Authorize(ctx)
	_ = p.Pause(ctx)
	_ = p.SetVolume(ctx, 0.5)
	_, _ = p.PlaySongs(ctx, []string{"i.Library"}, 0) // refused before the page
	_ = p.Close()
	if slices.Contains(page.Calls(), "stop()") {
		t.Errorf("page calls = %v, want no stop", page.Calls())
	}
}

func TestCloseStopsPastACommandInFlightAndGivesUp(t *testing.T) {
	p, page := gatedPlayer(t)
	ctx := context.Background()
	play := async(func() error { _, err := p.PlaySongs(ctx, []string{"111"}, 0); return err })
	waiting(t, page)
	page.gate <- struct{}{}
	if err := result(t, play); err != nil {
		t.Fatalf("PlaySongs: %v", err)
	}
	pause := async(func() error { return p.Pause(ctx) })
	waiting(t, page)
	resume := async(func() error { return p.Resume(ctx) })
	closed := async(p.Close)
	for name, ch := range map[string]<-chan error{"in-flight Pause": pause, "waiting Resume": resume} {
		if err := result(t, ch); !errors.Is(err, ErrClosed) {
			t.Errorf("%s after Close = %v, want ErrClosed", name, err)
		}
	}
	// The stop is held by the page: Close gives up after its timeout.
	if err := result(t, closed); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if got, want := page.Calls(), []string{`play(["111"],0)`, "pause()", "stop()"}; !slices.Equal(got, want) {
		t.Errorf("page calls = %v, want %v", got, want)
	}
}

func TestSupportsEverythingAndNothingIsUnsupported(t *testing.T) {
	p := New(newPage())
	for _, c := range []playback.Capability{playback.CapFavorites, playback.CapEditPlaylists, playback.CapCatalogSearch, "something-else"} {
		for _, id := range []string{"", "1000000001", "p.1"} {
			if !p.Supports(c, id) {
				t.Errorf("Supports(%s, %q) = false, want true", c, id)
			}
		}
	}
	ctx := context.Background()
	reads := map[string]func() error{
		"SearchCatalog":    func() error { _, err := p.SearchCatalog(ctx, "x", 5); return err },
		"Artist":           func() error { _, err := p.Artist(ctx, "1"); return err },
		"Album":            func() error { _, err := p.Album(ctx, "1"); return err },
		"SongAlbum":        func() error { _, err := p.SongAlbum(ctx, "1"); return err },
		"CatalogPlaylist":  func() error { _, err := p.CatalogPlaylist(ctx, "pl.1"); return err },
		"Playlists":        func() error { _, err := p.Playlists(ctx); return err },
		"LibraryPlaylist":  func() error { _, err := p.LibraryPlaylist(ctx, "p.1"); return err },
		"PlayPlaylist":     func() error { return p.PlayPlaylist(ctx, "p.1") },
		"PlayPlaylistFrom": func() error { return p.PlayPlaylistFrom(ctx, "p.1", 0) },
		"Favorites":        func() error { _, err := p.Favorites(ctx, []string{"1"}); return err },
	}
	calls := map[string]func() error{
		"CreatePlaylist": func() error { _, err := p.CreatePlaylist(ctx, "n", "", nil); return err },
		"AddToPlaylist":  func() error { return p.AddToPlaylist(ctx, "p.1", []string{"1"}) },
		"Favorite":       func() error { _, err := p.Favorite(ctx, "1"); return err },
		"SetFavorite":    func() error { return p.SetFavorite(ctx, "1", true) },
	}
	maps.Copy(calls, reads)
	for name, call := range calls {
		if err := call(); errors.Is(err, playback.ErrUnsupported) {
			t.Errorf("%s = %v, want it supported", name, err)
		}
	}
	_ = p.Close()
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrClosed) {
			t.Errorf("%s after Close = %v, want ErrClosed", name, err)
		}
	}
	if err := p.Pause(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Pause after Close = %v, want ErrClosed", err)
	}
	if _, err := p.Authorize(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Authorize after Close = %v, want ErrClosed", err)
	}
}

func TestHostileTextNeverBreaksTheExpression(t *testing.T) {
	page := newPage()
	p := New(page, WithPollInterval(time.Hour))
	defer p.Close()
	ctx := context.Background()
	hostile := "x\"); window.__nu11signal.stop(); (\"\u2028`${1}`</script>\\"
	_ = p.SetRepeat(ctx, playback.RepeatMode(hostile)) // refused in Go
	_, _ = p.PlaySongs(ctx, []string{hostile, "111"}, 1)
	for _, expr := range page.exprs {
		if strings.Contains(expr, hostile) || strings.Contains(expr, "\u2028") {
			t.Errorf("expression carries the raw text: %.120s", expr)
		}
	}
	if got, want := page.Calls(), []string{`play(["111"],0)`}; !slices.Equal(got, want) {
		t.Errorf("page calls = %v, want %v", got, want)
	}
}

func TestConcurrentUse(t *testing.T) {
	page := newPage()
	page.setHandle(replying(nowReply("playing")))
	p := New(page, WithPollInterval(time.Millisecond))
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			_, _ = p.PlaySongs(ctx, []string{"111", "222"}, i%2)
			_ = p.Seek(ctx, time.Duration(i)*time.Second)
			_ = p.SetVolume(ctx, float64(i)/8)
			_, _ = p.Volume(ctx)
			_ = p.Pause(ctx)
			_, _ = p.Authorize(ctx)
		})
	}
	go func() {
		for range p.States() {
		}
	}()
	wg.Wait()
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
