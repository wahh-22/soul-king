package webplayer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/wahh-22/nu11signal/internal/playback"
)

var (
	// ErrClosed is returned by calls made after Close.
	ErrClosed = errors.New("webplayer: player closed")
	// ErrInvalidArgument is returned, before anything reaches the page,
	// for a start song that is not a catalog song or an unknown repeat
	// mode.
	ErrInvalidArgument = errors.New("webplayer: invalid argument")
	// ErrVolumeUnknown is Volume's error while the page has not reported
	// its volume.
	ErrVolumeUnknown = errors.New("webplayer: the page has not reported its volume")
	// errNotInstalled is a page that still lacks the player script right
	// after it was installed.
	errNotInstalled = errors.New("webplayer: the page did not keep the player script")
	// errMalformed is a page answer that is not the JSON asked for.
	errMalformed = errors.New("webplayer: malformed page answer")
)

const (
	// DefaultPollInterval is how often the page's now-playing report is
	// polled once a play reached it.
	DefaultPollInterval = time.Second
	// DefaultStopTimeout bounds the stop Close sends, well within how
	// long quitting waits for Close.
	DefaultStopTimeout = 2 * time.Second
	// stateBuffer is how many states wait for the UI.
	stateBuffer = 16
	// drift is how far the UI's own estimate of the position (it advances
	// the last one while playing) may be off before a report corrects it.
	drift = time.Second
	// maxSeconds bounds a position or duration the page reports.
	maxSeconds = 1e7
)

// Evaluator runs a JavaScript expression in the page and returns its
// value as JSON, as (*Page).Evaluate does.
type Evaluator interface {
	Evaluate(ctx context.Context, expr string) (json.RawMessage, error)
}

// Player is the Linux Apple Music playback.Player over Apple's web player
// (and a playback.Capabilities and playback.LateAuthorizer): it drives
// the page's MusicKit through a small script it installs there (see
// bootstrap), and installs again when the page reloaded. It authorizes,
// queues catalog songs, pauses, resumes, skips, stops, seeks and sets the
// repeat mode and the page's volume, searches and browses the catalog,
// and reads the library playlists and which songs are favorites through
// the page's own MusicKit API client, through which it also marks
// favorites, creates library playlists and adds songs to them. Library
// playlists play as their catalog songs. Methods are safe for concurrent
// use; commands reach the page one at a time, while catalog reads and
// library edits do not wait for them.
//
// A command succeeds when the page's MusicKit call resolved, not when
// audio is heard. An applied play, pause, resume, stop, seek or repeat
// emits the state it asked for at once (next and previous leave the song
// they reach to the poller); from the first play on, a poller also
// reports what the page says it plays (see States).
type Player struct {
	// startupWarning is immutable after Open returns.
	startupWarning string
	page           Evaluator
	// every is how often the page's now-playing report is polled.
	every time.Duration
	// now and pause are the clock and the wait between polls; tests
	// replace pause.
	now   func() time.Time
	pause func(ctx context.Context, d time.Duration) error
	// ctx ends with Close, ending the waits in flight.
	ctx    context.Context
	cancel context.CancelFunc
	// slot is held by the command this player has at the page: the next
	// waits for it, so MusicKit calls never overlap.
	slot chan struct{}
	// stopTimeout bounds the stop Close sends.
	stopTimeout time.Duration
	closeOnce   sync.Once
	// catalog reads the catalog through fetch, each request bounded by
	// catalogTimeout.
	catalog        *catalog
	catalogTimeout time.Duration

	// script guards installed and hidden.
	script sync.Mutex
	// installed is set once the bootstrap was installed; a call that
	// finds it missing installs it again.
	installed bool
	// hidden is set by Hide; a new install hides the page again.
	hidden bool

	mu      sync.Mutex
	closed  bool
	states  chan playback.State
	errs    chan error
	started bool // a play was sent to the page
	// state is the last state emitted, at stateAt.
	state   playback.State
	stateAt time.Time
	// since is when the last command was applied: what the page reported
	// before then is older than that command, and ignored.
	since time.Time
	// polled is closed when the poller ends; nil until it starts.
	polled chan struct{}
	// failing is set once the poller reported a failure, until a poll
	// succeeds.
	failing bool
	// volume is the page's volume, 0 to 1, as last reported or set, if
	// hasVolume.
	volume    float64
	hasVolume bool
	// seeks and levels are the seek and the volume change waiting for
	// the slot, if any.
	seeks, levels *queued

	// browser, set by Open, is the browser the player owns: Close closes
	// it, and watched is closed when the goroutine reporting its exit
	// ends.
	browser browser
	watched chan struct{}
}

// StartupWarning is an optional, non-fatal notice for the composite UI.
func (p *Player) StartupWarning() string { return p.startupWarning }

// browser is the part of a launched *Browser the player and Open use, so
// tests can stand in for it.
type browser interface {
	Client() *Client
	Done() <-chan struct{}
	Close(ctx context.Context) error
}

// queued is a seek or a volume change waiting for the slot; superseded
// is closed when a later one takes its place.
type queued struct{ superseded chan struct{} }

// errSuperseded ends a queued call a later one replaced.
var errSuperseded = errors.New("webplayer: superseded")

// Option configures New.
type Option func(*Player)

// WithPollInterval sets how often the page's now-playing report is
// polled once a play reached it (default DefaultPollInterval).
func WithPollInterval(d time.Duration) Option {
	return func(p *Player) {
		if d > 0 {
			p.every = d
		}
	}
}

// WithClock sets the clock states are timed by (default time.Now).
func WithClock(now func() time.Time) Option {
	return func(p *Player) {
		if now != nil {
			p.now = now
		}
	}
}

// WithStopTimeout bounds the stop Close sends (default
// DefaultStopTimeout).
func WithStopTimeout(d time.Duration) Option {
	return func(p *Player) {
		if d > 0 {
			p.stopTimeout = d
		}
	}
}

// WithCatalogTimeout bounds each catalog request (default
// DefaultCatalogTimeout).
func WithCatalogTimeout(d time.Duration) Option {
	return func(p *Player) {
		if d > 0 {
			p.catalogTimeout = d
		}
	}
}

// New returns a Player driving the page, which is attached to
// music.apple.com (Attach). Nothing reaches the page until the first
// call. Close releases the player; the page and its browser are left
// running (Open returns a player that owns its browser instead).
func New(page Evaluator, opts ...Option) *Player {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Player{
		page:           page,
		every:          DefaultPollInterval,
		now:            time.Now,
		pause:          sleep,
		ctx:            ctx,
		cancel:         cancel,
		slot:           make(chan struct{}, 1),
		stopTimeout:    DefaultStopTimeout,
		catalogTimeout: DefaultCatalogTimeout,
		states:         make(chan playback.State, stateBuffer),
		errs:           make(chan error, 1),
		state:          playback.State{Repeat: playback.RepeatOff, VolumeMode: playback.VolumeApp},
	}
	p.catalog = &catalog{fetch: p.fetch}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// own makes the player own b: Close closes it after its stop, and b
// exiting before then is reported once on Errors (the calls that reach
// the page then fail with ErrBrowserGone, and Authorize with them).
func (p *Player) own(b browser) {
	p.browser = b
	p.watched = make(chan struct{})
	go func() {
		defer close(p.watched)
		select {
		case <-b.Done():
			p.mu.Lock()
			p.report(fmt.Errorf("webplayer: the browser exited: %w", ErrBrowserGone))
			p.mu.Unlock()
		case <-p.ctx.Done():
		}
	}()
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// install evaluates the bootstrap unless it was installed and force is
// false, and hides the page again after Hide.
func (p *Player) install(ctx context.Context, force bool) error {
	p.script.Lock()
	done, hidden := p.installed && !force, p.hidden
	p.script.Unlock()
	if done {
		return nil
	}
	raw, err := p.page.Evaluate(ctx, bootstrap)
	if err != nil {
		return err
	}
	var r struct {
		Installed bool `json:"installed"`
	}
	if json.Unmarshal(raw, &r) != nil || !r.Installed {
		return errNotInstalled
	}
	if hidden {
		expr, err := callExpr("hide")
		if err != nil {
			return err
		}
		if _, err := p.page.Evaluate(ctx, expr); err != nil {
			return err
		}
	}
	p.script.Lock()
	p.installed = true
	p.script.Unlock()
	return nil
}

// call calls fn of the page's namespace with args and returns its answer
// (null for none). The bootstrap is installed first, once, and again
// when the page reports the namespace missing (it reloaded); the call is
// then retried once.
func (p *Player) call(ctx context.Context, fn string, args ...any) (json.RawMessage, error) {
	expr, err := callExpr(fn, args...)
	if err != nil {
		return nil, err
	}
	for retry := false; ; retry = true {
		if err := p.install(ctx, retry); err != nil {
			return nil, err
		}
		raw, err := p.page.Evaluate(ctx, expr)
		if err != nil {
			return nil, err
		}
		var r struct {
			Missing bool            `json:"missing"`
			Value   json.RawMessage `json:"value"`
		}
		if json.Unmarshal(raw, &r) != nil {
			return nil, errMalformed
		}
		if !r.Missing {
			if len(r.Value) == 0 {
				r.Value = json.RawMessage("null")
			}
			return r.Value, nil
		}
		if retry {
			return nil, errNotInstalled
		}
	}
}

func (p *Player) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// check returns ErrClosed once Close began, or ctx's error.
func (p *Player) check(ctx context.Context) error {
	if p.isClosed() || p.ctx.Err() != nil {
		return ErrClosed
	}
	return ctx.Err()
}

// run calls fn with args as one command, once the player's previous one
// settled. When ctx ends during either wait it returns ctx.Err(); a call
// already evaluating in the page may still take effect there. Close ends
// the waits too, with ErrClosed.
func (p *Player) run(ctx context.Context, op, fn string, args ...any) error {
	return p.latest(ctx, op, nil, func(ctx context.Context) error {
		_, err := p.call(ctx, fn, args...)
		return err
	})
}

// latest runs send as one command through queue (seeks or levels), when
// it is not nil: a later call through the same queue that comes while
// this one waits for the slot takes its place, and this one returns
// errSuperseded, unsent.
func (p *Player) latest(ctx context.Context, op string, queue **queued, send func(context.Context) error) error {
	if err := p.check(ctx); err != nil {
		return err
	}
	var q *queued
	var superseded <-chan struct{} // nil, never ready, without a queue
	if queue != nil {
		q = &queued{superseded: make(chan struct{})}
		superseded = q.superseded
		p.mu.Lock()
		if *queue != nil {
			close((*queue).superseded)
		}
		*queue = q
		p.mu.Unlock()
	}
	select {
	case p.slot <- struct{}{}:
	case <-superseded:
		return errSuperseded
	case <-ctx.Done():
		p.dequeue(queue, q)
		return p.ended(ctx)
	case <-p.ctx.Done():
		p.dequeue(queue, q)
		return ErrClosed
	}
	defer func() { <-p.slot }()
	if queue != nil {
		p.mu.Lock()
		current := *queue == q
		if current {
			*queue = nil
		}
		p.mu.Unlock()
		if !current {
			return errSuperseded
		}
	}
	if err := p.check(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	err := send(ctx)
	switch {
	case err == nil:
		return nil
	case p.ctx.Err() != nil:
		return ErrClosed
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return fmt.Errorf("webplayer %s: %w", op, err)
}

// dequeue removes q from queue, unless a later call replaced it.
func (p *Player) dequeue(queue **queued, q *queued) {
	if queue == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if *queue == q {
		*queue = nil
	}
}

// ended is the error of a call whose ctx ended: ErrClosed after Close.
func (p *Player) ended(ctx context.Context) error {
	if p.ctx.Err() != nil {
		return ErrClosed
	}
	return ctx.Err()
}

// applied publishes the state an applied command asked for: status on a
// new song, of which nothing else is known yet, or, without one, on the
// song and at the position the UI reached. What the page reported before
// it is ignored from then on.
func (p *Player) applied(status playback.Status, song string) {
	p.settle(func(next *playback.State) {
		if song != "" {
			*next = playback.State{SongID: song, Repeat: next.Repeat, VolumeMode: next.VolumeMode}
		}
		next.Status = status
	})
}

// settle records that a command applied now, so that what the page
// reported before is ignored, and publishes the state change makes of
// the last one (at the position the UI reached), when change is not nil.
func (p *Player) settle(change func(next *playback.State)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.since = now
	if change == nil {
		return
	}
	next := p.state
	next.Position = p.position(now)
	change(&next)
	p.publish(next, now)
}

// position is where the UI has the song at now: the last position,
// advanced while playing, within the song. mu is held.
func (p *Player) position(now time.Time) time.Duration {
	pos := p.state.Position
	if p.state.Status == playback.StatusPlaying {
		pos += now.Sub(p.stateAt)
	}
	return within(pos, p.state.Duration)
}

// within limits pos to the song, when its duration is known.
func within(pos, duration time.Duration) time.Duration {
	if duration > 0 {
		pos = min(pos, duration)
	}
	return max(pos, 0)
}

// publish emits s as the state at at, dropping the oldest unread state
// when the buffer is full so the newest one gets through. mu is held, so
// the drain-then-send cannot block; nothing is sent after Close.
func (p *Player) publish(s playback.State, at time.Time) {
	if p.closed {
		return
	}
	p.state, p.stateAt = s, at
	select {
	case p.states <- s:
		return
	default:
	}
	select {
	case <-p.states:
	default:
	}
	p.states <- s
}

// report delivers an asynchronous failure on Errors, unless one is
// already waiting or the player is closed. mu is held.
func (p *Player) report(err error) {
	if p.closed {
		return
	}
	select {
	case p.errs <- err:
	default:
	}
}

// watch starts the poller, once. mu is held.
//
// It starts with the first play rather than with New: until then the
// page plays nothing this player started, and a player used only to
// authorize sends no polls.
func (p *Player) watch() {
	if p.closed || p.polled != nil {
		return
	}
	p.polled = make(chan struct{})
	go p.poll(p.polled)
}

// poll asks the page what it plays every p.every, one request at a time,
// until Close.
func (p *Player) poll(done chan<- struct{}) {
	defer close(done)
	for p.pause(p.ctx, p.every) == nil {
		sent := p.now()
		np, err := p.nowPlaying(p.ctx)
		if p.ctx.Err() != nil {
			return
		}
		p.observe(np, sent, err)
	}
}

// nowPlaying is the page's now() report. A field the page does not know
// is empty, or nil.
type nowPlaying struct {
	Status    string   `json:"status"`
	Title     string   `json:"title"`
	Artist    string   `json:"artist"`
	Album     string   `json:"album"`
	SongID    string   `json:"songID"`
	PositionS *float64 `json:"positionS"`
	DurationS *float64 `json:"durationS"`
	Repeat    string   `json:"repeat"`
	Volume    *float64 `json:"volume"`
}

// nowPlaying asks the page for its now() report.
func (p *Player) nowPlaying(ctx context.Context) (nowPlaying, error) {
	raw, err := p.call(ctx, "now")
	if err != nil {
		return nowPlaying{}, err
	}
	var np nowPlaying
	if json.Unmarshal(raw, &np) != nil {
		return nowPlaying{}, errMalformed
	}
	return np, nil
}

// observe turns a poll sent at sent into a state, when it tells the UI
// something new.
//
// A page between documents (a protocol error while it reloads) is
// skipped quietly; any other failure is reported once until a poll
// succeeds, and a browser gone while playing is reported stopped, as the
// composite reports a backend it lost. A poll sent before the last
// command applied is ignored, so it cannot undo that command's state.
func (p *Player) observe(np nowPlaying, sent time.Time, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		now := p.now()
		if errors.Is(err, ErrBrowserGone) && (p.state.Status == playback.StatusPlaying || p.state.Status == playback.StatusSeeking) {
			next := p.state
			next.Status, next.Position = playback.StatusStopped, p.position(now)
			p.publish(next, now)
		}
		var cdp *CDPError
		if !errors.As(err, &cdp) && !p.failing {
			p.failing = true
			p.report(fmt.Errorf("webplayer status: %w", err))
		}
		return
	}
	p.failing = false
	if sent.Before(p.since) {
		return
	}
	if v, ok := level(np.Volume); ok {
		p.volume, p.hasVolume = v, true
	}
	now := p.now()
	if next := p.reported(np, now); p.changed(next, now) {
		p.publish(next, now)
	}
}

// reported is the last state updated by np: its status (loading and
// unknown keep the last one), its repeat mode (unknown keeps the last
// one), its song, and its position. A song field np leaves out keeps its
// value on the same song, and is empty on another. mu is held.
func (p *Player) reported(np nowPlaying, now time.Time) playback.State {
	title, artist, album := clean(np.Title), clean(np.Artist), clean(np.Album)
	songID := np.SongID
	if !catalogID(songID) {
		songID = ""
	}
	next := p.state
	if p.sameSong(songID, title) {
		next.Position = p.position(now)
	} else {
		next = playback.State{Status: next.Status, SongID: songID, Repeat: next.Repeat, VolumeMode: next.VolumeMode}
	}
	if repeatValid(np.Repeat) {
		next.Repeat = playback.RepeatMode(np.Repeat)
	}
	switch np.Status {
	case "playing":
		next.Status = playback.StatusPlaying
	case "paused":
		next.Status = playback.StatusPaused
	case "stopped":
		next.Status = playback.StatusStopped
	case "seeking":
		next.Status = playback.StatusSeeking
	}
	set := func(field *string, v string) {
		if v != "" {
			*field = v
		}
	}
	set(&next.Title, title)
	set(&next.Artist, artist)
	set(&next.Album, album)
	if d, ok := seconds(np.DurationS); ok && d > 0 {
		next.Duration = d
	}
	if pos, ok := seconds(np.PositionS); ok {
		next.Position = pos
	}
	next.Position = within(next.Position, next.Duration)
	return next
}

// seconds converts a reported number of seconds, when it is one.
func seconds(v *float64) (time.Duration, bool) {
	if v == nil || math.IsNaN(*v) || *v < 0 || *v > maxSeconds {
		return 0, false
	}
	return time.Duration(math.Round(*v * float64(time.Second))), true
}

// level reads a reported volume, when it is one.
func level(v *float64) (float64, bool) {
	if v == nil || math.IsNaN(*v) || *v < 0 || *v > 1 {
		return 0, false
	}
	return *v, true
}

// sameSong reports whether a report names the last state's song: the
// same catalog id or, without one, no other title. mu is held.
func (p *Player) sameSong(id, title string) bool {
	if id != "" {
		return id == p.state.SongID
	}
	return title == "" || p.state.Title == "" || title == p.state.Title
}

// changed reports whether next tells the UI something new: anything but
// the position, or a position the UI's own estimate misses by more than
// drift. mu is held.
func (p *Player) changed(next playback.State, now time.Time) bool {
	off := next.Position - p.position(now)
	cur := p.state
	cur.Position, next.Position = 0, 0
	return cur != next || off > drift || off < -drift
}

// clean removes control and format characters (bidirectional overrides
// among them) from page text, and the spaces around it.
func clean(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf) {
			return -1
		}
		return r
	}, s))
}

// catalogID reports whether id is a catalog id: 1 to 20 ASCII digits.
func catalogID(id string) bool {
	if len(id) < 1 || len(id) > 20 {
		return false
	}
	for _, c := range []byte(id) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func repeatValid(mode string) bool {
	switch playback.RepeatMode(mode) {
	case playback.RepeatOff, playback.RepeatOne, playback.RepeatAll:
		return true
	}
	return false
}

// AuthorizesLate reports true: the user may sign in to the page after
// Authorize first answers AuthNotDetermined.
func (p *Player) AuthorizesLate() bool { return true }

// Authorize reports whether the page is signed in, without asking it to
// sign in: AuthAuthorized when MusicKit says so, and AuthNotDetermined
// while it is not signed in, not ready, or between documents. A browser
// gone is an error, since asking again cannot change it.
func (p *Player) Authorize(ctx context.Context) (playback.AuthStatus, error) {
	if err := p.check(ctx); err != nil {
		return "", err
	}
	_, authorized, err := p.status(ctx)
	switch {
	case err != nil:
		return "", err
	case authorized:
		return playback.AuthAuthorized, nil
	}
	return playback.AuthNotDetermined, nil
}

// status asks the page whether its MusicKit is ready and signed in. A
// page that cannot answer (between documents, not loaded yet) is neither;
// only ctx ending and a browser gone are errors.
func (p *Player) status(ctx context.Context) (ready, authorized bool, err error) {
	raw, err := p.call(ctx, "status")
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return false, false, ctx.Err()
	case errors.Is(err, ErrBrowserGone):
		return false, false, fmt.Errorf("webplayer authorize: %w", err)
	default:
		return false, false, nil
	}
	var s struct {
		Ready      bool `json:"ready"`
		Authorized bool `json:"authorized"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return false, false, nil
	}
	return s.Ready, s.Ready && s.Authorized, nil
}

// AuthorizationHint tells the user how to sign in while Authorize waits:
// in the window of nu11signal --apple-music-login, which needs the
// profile this player's browser holds, so nu11signal must quit first.
func (p *Player) AuthorizationHint() string {
	return "quit and run nu11signal --apple-music-login"
}

// PlaySongs queues the catalog songs and plays ids[start]; the page takes
// the whole queue. Ids that are not catalog ids (library or local songs)
// cannot be queued: they are left out and reported Skipped, and starting
// at one is an error. One song is queued on its own.
func (p *Player) PlaySongs(ctx context.Context, ids []string, start int) (playback.QueueReport, error) {
	if start < 0 || start >= len(ids) {
		return playback.QueueReport{}, fmt.Errorf("webplayer play: start %d out of range: %w", start, ErrInvalidArgument)
	}
	if !catalogID(ids[start]) {
		return playback.QueueReport{}, fmt.Errorf("webplayer play: not a catalog song: %w", ErrInvalidArgument)
	}
	queue, at, skipped := catalogQueue(ids, start)
	err := p.latest(ctx, "play", nil, func(ctx context.Context) error {
		// The page may start playing even if its answer is lost.
		p.mu.Lock()
		p.started = true
		p.watch()
		p.mu.Unlock()
		_, err := p.call(ctx, "play", queue, at)
		return err
	})
	if err != nil {
		return playback.QueueReport{}, err
	}
	p.applied(playback.StatusPlaying, ids[start])
	return playback.QueueReport{Skipped: skipped}, nil
}

// catalogQueue is the queue PlaySongs sends for ids from start, ids[start]
// being a catalog id: the catalog ids, in order; at is ids[start]'s index
// in it. skipped are the other ids, in order.
func catalogQueue(ids []string, start int) (queue []string, at int, skipped []string) {
	for i, id := range ids {
		if i == start {
			at = len(queue)
		}
		if catalogID(id) {
			queue = append(queue, id)
		} else {
			skipped = append(skipped, id)
		}
	}
	return queue, at, skipped
}

// Pause pauses the page.
func (p *Player) Pause(ctx context.Context) error {
	if err := p.run(ctx, "pause", "pause"); err != nil {
		return err
	}
	p.applied(playback.StatusPaused, "")
	return nil
}

// Resume resumes the page.
func (p *Player) Resume(ctx context.Context) error {
	if err := p.run(ctx, "resume", "resume"); err != nil {
		return err
	}
	p.applied(playback.StatusPlaying, "")
	return nil
}

// Next skips to the next song of the page's queue. The song it reaches
// is reported by the poller.
func (p *Player) Next(ctx context.Context) error {
	if err := p.run(ctx, "next", "next"); err != nil {
		return err
	}
	p.settle(nil)
	return nil
}

// Previous skips to the previous song of the page's queue (or, as
// MusicKit decides, the start of the song). The song it reaches is
// reported by the poller.
func (p *Player) Previous(ctx context.Context) error {
	if err := p.run(ctx, "previous", "previous"); err != nil {
		return err
	}
	p.settle(nil)
	return nil
}

// Stop stops the page when a play reached it, so it falls silent when
// the composite switches to another backend. It is best effort: a page
// that is gone or refuses is not an error (nothing plays, or there is
// nothing more to do), but the failure is reported on Errors, so the
// page left playing is not a silent surprise.
func (p *Player) Stop(ctx context.Context) error {
	if err := p.check(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	started := p.started
	p.mu.Unlock()
	if !started {
		return nil
	}
	if err := p.run(ctx, "stop", "stop"); err != nil {
		if errors.Is(err, ErrClosed) {
			return err
		}
		// A caller that gave up has no failure to hear about.
		if ctx.Err() == nil {
			p.mu.Lock()
			p.report(err)
			p.mu.Unlock()
		}
		return nil
	}
	p.applied(playback.StatusStopped, "")
	return nil
}

// Seek moves the song to position (a negative one is the start). Seeks
// made while the player's last command is pending are coalesced: only
// the latest is sent when it settles, and the ones it replaced return
// nil.
func (p *Player) Seek(ctx context.Context, position time.Duration) error {
	position = max(position, 0)
	err := p.latest(ctx, "seek", &p.seeks, func(ctx context.Context) error {
		_, err := p.call(ctx, "seek", position.Seconds())
		return err
	})
	if errors.Is(err, errSuperseded) {
		return nil
	}
	if err != nil {
		return err
	}
	p.settle(func(next *playback.State) { next.Position = within(position, next.Duration) })
	return nil
}

// SetRepeat sets the page's repeat mode.
func (p *Player) SetRepeat(ctx context.Context, mode playback.RepeatMode) error {
	if !repeatValid(string(mode)) {
		return fmt.Errorf("webplayer repeat: mode %q: %w", mode, ErrInvalidArgument)
	}
	if err := p.run(ctx, "repeat", "repeat", string(mode)); err != nil {
		return err
	}
	p.settle(func(next *playback.State) { next.Repeat = mode })
	return nil
}

// Volume reports the page's own volume (VolumeApp): as the page reports
// it now or, when it does not, as it last reported it or SetVolume set
// it. Until then it is ErrVolumeUnknown, or the read's error.
func (p *Player) Volume(ctx context.Context) (float64, error) {
	if err := p.check(ctx); err != nil {
		return 0, err
	}
	sent := p.now()
	np, err := p.nowPlaying(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, ErrClosed
	}
	if v, ok := level(np.Volume); err == nil && ok && !sent.Before(p.since) {
		p.volume, p.hasVolume = v, true
	}
	switch {
	case p.hasVolume:
		return p.volume, nil
	case ctx.Err() != nil:
		return 0, ctx.Err()
	case err != nil:
		return 0, fmt.Errorf("webplayer volume: %w", err)
	}
	return 0, ErrVolumeUnknown
}

// SetVolume sets the page's own volume, clamped with
// playback.ClampVolume and rounded to a whole percent. Like seeks,
// changes made while the player's last command is pending are
// coalesced: only the latest is sent, and the ones it replaced return
// nil.
func (p *Player) SetVolume(ctx context.Context, level float64) error {
	v := math.Round(playback.ClampVolume(level)*100) / 100
	err := p.latest(ctx, "volume", &p.levels, func(ctx context.Context) error {
		_, err := p.call(ctx, "volume", v)
		return err
	})
	if errors.Is(err, errSuperseded) {
		return nil
	}
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.since = p.now()
	p.volume, p.hasVolume = v, true
	return nil
}

// Hide hides the page (no display, animations cancelled), so the browser
// does not spend its time drawing Apple's interface while it plays; the
// page is hidden again whenever the player reinstalls its script after
// a reload. Call it once signed in: a hidden page shows no sign-in.
func (p *Player) Hide(ctx context.Context) error {
	if err := p.check(ctx); err != nil {
		return err
	}
	p.script.Lock()
	p.hidden = true
	p.script.Unlock()
	if _, err := p.call(ctx, "hide"); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("webplayer hide: %w", err)
	}
	return nil
}

// Supports reports true: the player offers the catalog, favorites and
// playlist editing for every id.
func (p *Player) Supports(playback.Capability, string) bool { return true }

// Playlists lists the library playlists in the Apple Music API's order,
// which is alphabetical, up to 500, reading every page.
func (p *Player) Playlists(ctx context.Context) ([]playback.Playlist, error) {
	return fromCatalog(ctx, p, p.catalog.playlists)
}

// LibraryPlaylist loads a library playlist page: its songs in order, up
// to 1000 (music videos are left out), and its description. A song's id
// is its catalog id; a song not in the catalog keeps its library id
// ("i.…") and is LibraryOnly.
func (p *Player) LibraryPlaylist(ctx context.Context, playlistID string) (playback.PlaylistDetail, error) {
	return fromCatalog(ctx, p, func(ctx context.Context) (playback.PlaylistDetail, error) {
		return p.catalog.libraryPlaylist(ctx, playlistID)
	})
}

// PlayPlaylist plays a library playlist from its first catalog song, as
// a queue of its catalog songs: LibraryOnly songs are skipped.
func (p *Player) PlayPlaylist(ctx context.Context, id string) error {
	return p.playLibrary(ctx, id, 0, false)
}

// PlayPlaylistFrom plays a library playlist from the song at index start
// of its LibraryPlaylist tracks, as PlayPlaylist queues it; starting at a
// LibraryOnly song is ErrInvalidArgument. The songs are read again, so a
// playlist changed since LibraryPlaylist may shift the start.
func (p *Player) PlayPlaylistFrom(ctx context.Context, playlistID string, start int) error {
	if start < 0 {
		return fmt.Errorf("webplayer play playlist: start %d out of range: %w", start, ErrInvalidArgument)
	}
	return p.playLibrary(ctx, playlistID, start, true)
}

// playLibrary reads the songs of a library playlist and plays its queue
// (see libraryQueue) through PlaySongs.
func (p *Player) playLibrary(ctx context.Context, playlistID string, start int, from bool) error {
	tracks, err := fromCatalog(ctx, p, func(ctx context.Context) ([]playback.Song, error) {
		return p.catalog.libraryTracks(ctx, playlistID)
	})
	if err != nil {
		return err
	}
	ids, at, err := libraryQueue(tracks, start, from)
	if err != nil {
		return fmt.Errorf("webplayer play playlist: %w", err)
	}
	_, err = p.PlaySongs(ctx, ids, at)
	return err
}

// Favorites reports, for each song (a catalog id or a library id
// "i.…"), whether it is a favorite, reading the ratings in batches of
// 100. No ids answer an empty map without asking the page; any other id
// is ErrInvalidArgument.
func (p *Player) Favorites(ctx context.Context, songIDs []string) (map[string]bool, error) {
	return fromCatalog(ctx, p, func(ctx context.Context) (map[string]bool, error) {
		if len(songIDs) == 0 {
			return map[string]bool{}, nil
		}
		return p.catalog.favorites(ctx, songIDs)
	})
}

// SearchCatalog runs a mixed catalog search, as Apple Music shows it:
// term suggestions (best effort), the top results across kinds, then
// artists, albums, songs and playlists. limit is clamped to 1...25 per
// result type; suggestions stop at 10 and top results at 6.
func (p *Player) SearchCatalog(ctx context.Context, term string, limit int) (playback.SearchResults, error) {
	return fromCatalog(ctx, p, func(ctx context.Context) (playback.SearchResults, error) {
		return p.catalog.search(ctx, term, limit)
	})
}

// Artist loads a catalog artist page; only the artist lookup can fail
// it, each other section is left empty when its request fails.
func (p *Player) Artist(ctx context.Context, artistID string) (playback.ArtistDetail, error) {
	return fromCatalog(ctx, p, func(ctx context.Context) (playback.ArtistDetail, error) {
		return p.catalog.artist(ctx, artistID)
	})
}

// Album loads a catalog album page, its tracks read up to ten pages.
func (p *Player) Album(ctx context.Context, albumID string) (playback.AlbumDetail, error) {
	return fromCatalog(ctx, p, func(ctx context.Context) (playback.AlbumDetail, error) {
		return p.catalog.album(ctx, albumID)
	})
}

// SongAlbum loads the page of a catalog song's first album.
func (p *Player) SongAlbum(ctx context.Context, songID string) (playback.AlbumDetail, error) {
	return fromCatalog(ctx, p, func(ctx context.Context) (playback.AlbumDetail, error) {
		return p.catalog.songAlbum(ctx, songID)
	})
}

// CatalogPlaylist loads a catalog playlist page, its tracks read up to
// ten pages.
func (p *Player) CatalogPlaylist(ctx context.Context, playlistID string) (playback.PlaylistDetail, error) {
	return fromCatalog(ctx, p, func(ctx context.Context) (playback.PlaylistDetail, error) {
		return p.catalog.playlist(ctx, playlistID)
	})
}

// CreatePlaylist creates a library playlist holding the songs (catalog
// ids or library ids "i.…"), in order; description and songIDs may be
// empty, a blank name is ErrInvalidArgument. The playlist returned has
// the new playlist's API library id ("p.…"). A creation that timed out
// is ErrEditOutcomeUnknown: it is not retried, and may still appear.
func (p *Player) CreatePlaylist(ctx context.Context, name, description string, songIDs []string) (playback.Playlist, error) {
	return fromCatalog(ctx, p, func(ctx context.Context) (playback.Playlist, error) {
		return p.createPlaylist(ctx, name, description, songIDs)
	})
}

// AddToPlaylist appends the songs (catalog ids or library ids "i.…"), in
// order and in one request, to a library playlist ("p.…"). A playlist
// Apple refuses to change is ErrPlaylistNotEditable; an addition that
// timed out is ErrEditOutcomeUnknown and is not retried.
func (p *Player) AddToPlaylist(ctx context.Context, playlistID string, songIDs []string) error {
	_, err := fromCatalog(ctx, p, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, p.addToPlaylist(ctx, playlistID, songIDs)
	})
	return err
}

// Favorite reports whether the song (a catalog id or a library id
// "i.…") is a favorite, reading its rating as Favorites does.
func (p *Player) Favorite(ctx context.Context, songID string) (bool, error) {
	loved, err := p.Favorites(ctx, []string{songID})
	if err != nil {
		return false, err
	}
	return loved[songID], nil
}

// SetFavorite loves the song (a catalog id or a library id "i.…"), or
// clears its rating; clearing a song without one succeeds.
func (p *Player) SetFavorite(ctx context.Context, songID string, on bool) error {
	_, err := fromCatalog(ctx, p, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, p.setFavorite(ctx, songID, on)
	})
	return err
}

// States delivers a state for each command applied and, from the first
// play on, for each change the page reports: polled every poll interval
// (WithPollInterval), sent only when the status, song, text, repeat mode
// or duration changed or the position drifted from the UI's own
// estimate. Title, Artist and Album are free of control and format
// characters. A browser gone while playing is reported stopped; a report
// that is missing or stale changes nothing. It is closed by Close.
func (p *Player) States() <-chan playback.State { return p.states }

// Errors delivers the failures no call returns: a Stop that could not
// stop the page, and a poll the page failed (once until a poll succeeds;
// a page between documents is not reported). It is closed by Close.
func (p *Player) Errors() <-chan error { return p.errs }

// Close ends the waits in flight (their calls return ErrClosed) and the
// poller, and closes the channels. When a play reached the page it then
// stops it, as quitting the helper does on macOS: one stop, sent at once
// (not behind a command in flight) and given up after the stop timeout.
// It is best effort: a page that is gone or slow is left as it is, and
// Close still returns nil. The page and its browser are left running,
// unless the player owns the browser (Open): then Close closes it last,
// bounded by the browser's own close timeout. It is idempotent.
func (p *Player) Close() error {
	p.closeOnce.Do(func() {
		p.cancel()
		p.mu.Lock()
		started := p.started
		p.mu.Unlock()
		if started {
			ctx, cancel := context.WithTimeout(context.Background(), p.stopTimeout)
			_, _ = p.call(ctx, "stop")
			cancel()
		}
		p.mu.Lock()
		p.closed = true
		close(p.states)
		close(p.errs)
		polled := p.polled
		p.mu.Unlock()
		if polled != nil {
			<-polled
		}
		if p.browser != nil {
			_ = p.browser.Close(context.Background())
			<-p.watched
		}
	})
	return nil
}

var (
	_ playback.Player              = (*Player)(nil)
	_ playback.Capabilities        = (*Player)(nil)
	_ playback.LateAuthorizer      = (*Player)(nil)
	_ playback.AuthorizationHinter = (*Player)(nil)
	_ browser                      = (*Browser)(nil)
	_ Evaluator                    = (*Page)(nil)
)
