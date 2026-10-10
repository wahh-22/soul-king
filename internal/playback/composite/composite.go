// Package composite joins two playback.Players into one: a primary
// backend (Apple Music, through the helper; nil when there is none, as on
// Linux) and the local files backend. Calls are routed by the namespace of
// their ids (playback.SourceOf): local ids go to the local backend,
// everything else to the primary one.
//
//   - Playlists lists the primary's playlists, then the local ones, each
//     marked with its Source.
//   - A play (PlaySongs, PlayPlaylist, PlayPlaylistFrom) goes to the
//     backend of its start song or playlist, which becomes the active one;
//     switching backends stops the one left first. A queue mixing sources
//     plays only the start song's; the others are reported Skipped.
//   - Transport, seek, repeat and volume go to the active backend, and only
//     its States and Levels reach the UI (so a backend left behind cannot
//     overwrite what plays). Errors of both are merged.
//   - The catalog (search and its pages), favorites and playlist editing
//     are the primary's: local ids, and every call without a primary, are
//     playback.ErrUnsupported, which Supports announces beforehand.
//   - Authorize always succeeds when the primary refuses or fails: the
//     local files need no permission. The refusal is reported once on
//     Errors and the primary is left out for the session (no catalog, no
//     Apple playlists), so the UI stays usable for local files instead of
//     stopping at an access error. A primary that authorizes late
//     (playback.LateAuthorizer) and is not determined yet waits instead:
//     reported once, left out like a refused one, and asked again by each
//     Authorize and every few seconds in the background until it is
//     authorized (then used, reported ready) or refuses (then left out for
//     the session).
package composite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wahh-22/nu11signal/internal/playback"
)

// buffer is how many states and errors wait for the UI.
const buffer = 16

// stopTimeout bounds the Stop sent to the backend left by a switch, when
// the play's own context leaves more time.
const stopTimeout = 3 * time.Second

// recheckInterval is how often a pending primary is asked again, and
// recheckTimeout bounds each question: longer than a bridge request, so
// the primary answers a bridge that is down itself.
const (
	recheckInterval = 3 * time.Second
	recheckTimeout  = 10 * time.Second
)

// newTicker ticks every recheckInterval until stopped.
func newTicker() (<-chan time.Time, func()) {
	t := time.NewTicker(recheckInterval)
	return t.C, t.Stop
}

// Player is a playback.Player (and LevelSource, Capabilities and
// LibraryWatcher) routing between a primary and a local backend. Methods
// are safe for concurrent use.
type Player struct {
	primary playback.Player // nil: no Apple Music
	local   playback.Player

	states  chan playback.State
	errs    chan error
	levels  chan playback.Spectrum
	changed chan struct{}
	done    chan struct{}
	wg      sync.WaitGroup
	// life ends with Close, ending a re-check's question in flight.
	life context.Context
	end  context.CancelFunc

	closeOnce sync.Once
	closeErr  error

	// ticker paces the re-check of a pending primary; tests replace it.
	ticker func() (<-chan time.Time, func())

	mu           sync.Mutex
	active       playback.Player
	appleOff     bool // the primary refused authorization or failed it
	applePending bool // the primary authorizes late and has not yet
	rechecking   bool // a goroutine asks the pending primary again
	closed       bool // the channels are closed
}

// New joins primary (nil when there is no Apple Music) and local, which
// must not be nil. The primary is active until something local plays; the
// Player owns both and closes them with Close.
func New(primary, local playback.Player) *Player {
	p := &Player{
		primary: primary,
		local:   local,
		states:  make(chan playback.State, buffer),
		errs:    make(chan error, buffer),
		levels:  make(chan playback.Spectrum, 1),
		changed: make(chan struct{}, 1),
		done:    make(chan struct{}),
		ticker:  newTicker,
		active:  local,
	}
	if primary != nil {
		p.active = primary
	}
	// Optional startup hints are queued once, not on each authorization.
	if h, ok := primary.(interface{ StartupWarning() string }); ok {
		if warning := h.StartupWarning(); warning != "" {
			p.notify(errors.New(warning))
		}
	}
	p.life, p.end = context.WithCancel(context.Background())
	for _, b := range p.backends() {
		p.forward(b)
	}
	if w, ok := local.(playback.LibraryWatcher); ok {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for range w.LibraryChanged() {
				select {
				case p.changed <- struct{}{}:
				default:
				}
			}
		}()
	}
	return p
}

// LocalOnly joins no primary with local, as New(nil, local) does, for an
// Apple Music that could not start, and reports once on Errors that it is
// unavailable for reason.
func LocalOnly(local playback.Player, reason string) *Player {
	p := New(nil, local)
	p.notify(unavailableFor(reason))
	return p
}

// backends are the backends there are, the primary first.
func (p *Player) backends() []playback.Player {
	if p.primary == nil {
		return []playback.Player{p.local}
	}
	return []playback.Player{p.primary, p.local}
}

// forward relays b's states and levels while it is active, and all its
// errors, until b closes them.
func (p *Player) forward(b playback.Player) {
	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		for s := range b.States() {
			if p.isActive(b) {
				trySend(p.states, s, p.done)
			}
		}
		p.lost(b)
	}()
	go func() {
		defer p.wg.Done()
		for err := range b.Errors() {
			trySend(p.errs, err, p.done)
		}
	}()
	src, ok := b.(playback.LevelSource)
	if !ok {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for l := range src.Levels() {
			if !p.isActive(b) {
				continue
			}
			// Only the latest reading matters: replace one unread.
			select {
			case <-p.levels:
			default:
			}
			trySend(p.levels, l, p.done)
		}
	}()
}

// trySend sends v on ch unless done closes first; it reports whether v
// was sent.
func trySend[T any](ch chan T, v T, done <-chan struct{}) bool {
	select {
	case ch <- v:
		return true
	case <-done:
		return false
	}
}

// lost handles a backend whose states closed before Close (a helper that
// exited): when it was active, the other backend takes over, the UI is
// told the music stopped, and why.
func (p *Player) lost(b playback.Player) {
	select {
	case <-p.done:
		return
	default:
	}
	p.mu.Lock()
	if p.active != b || p.primary == nil {
		p.mu.Unlock()
		return
	}
	if b == p.primary {
		p.active, p.appleOff = p.local, true
	} else {
		p.active = p.primary
	}
	p.mu.Unlock()
	trySend(p.states, playback.State{Status: playback.StatusStopped}, p.done)
	p.notify(fmt.Errorf("%s backend offline", sourceName(b == p.primary)))
}

func sourceName(primary bool) string {
	if primary {
		return "apple music"
	}
	return "local files"
}

// notify reports err on Errors without blocking (dropped when the UI is
// that far behind) and never after Close.
func (p *Player) notify(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	select {
	case p.errs <- err:
	default:
	}
}

func (p *Player) isActive(b playback.Player) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active == b
}

func (p *Player) current() playback.Player {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

// apple is the primary backend, or ErrUnsupported when there is none, it
// refused authorization or it is still waiting for it.
func (p *Player) apple() (playback.Player, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.primary == nil || p.appleOff || p.applePending {
		return nil, playback.ErrUnsupported
	}
	return p.primary, nil
}

// backend is the backend of a song or playlist id.
func (p *Player) backend(id string) (playback.Player, error) {
	if playback.SourceOf(id) == playback.SourceLocal {
		return p.local, nil
	}
	return p.apple()
}

// catalog is the primary for a call about id, which local ids never have.
func (p *Player) catalog(id string) (playback.Player, error) {
	if playback.SourceOf(id) == playback.SourceLocal {
		return nil, playback.ErrUnsupported
	}
	return p.apple()
}

// switchTo makes b the active backend, stopping the one it replaces so two
// never play at once. b is active before its play is sent, so the states
// that play reports are relayed.
func (p *Player) switchTo(ctx context.Context, b playback.Player) {
	p.mu.Lock()
	old := p.active
	p.active = b
	p.mu.Unlock()
	if old == b || old == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	_ = old.Stop(ctx) // best effort: it may have nothing playing
}

// Supports reports whether c is offered for id (see playback.Capabilities):
// the catalog, favorites and playlist editing only for Apple Music ids (or
// with an empty id, at all) while the primary is there, authorized and,
// when it implements playback.Capabilities itself, offers them.
func (p *Player) Supports(c playback.Capability, id string) bool {
	switch c {
	case playback.CapCatalogSearch, playback.CapFavorites, playback.CapEditPlaylists:
		if id != "" && playback.SourceOf(id) == playback.SourceLocal {
			return false
		}
		apple, err := p.apple()
		return err == nil && playback.Supports(apple, c, id)
	}
	return true
}

// Authorize authorizes the local backend and, when there is one, the
// primary; a primary that refuses or fails is left out (see the package
// documentation) and the result is still authorized.
//
// A primary that authorizes late and answers AuthNotDetermined is pending
// instead, and asked again by the next Authorize and, every
// recheckInterval, by one goroutine (see recheck). The question is asked
// there, not by the calls apple gates: Supports must stay cheap (the UI
// asks while drawing) and they have a context to bound it.
func (p *Player) Authorize(ctx context.Context) (playback.AuthStatus, error) {
	if _, err := p.local.Authorize(ctx); err != nil {
		return "", err
	}
	p.mu.Lock()
	off := p.primary == nil || p.appleOff
	p.mu.Unlock()
	if off {
		return playback.AuthAuthorized, nil
	}
	s, err := p.primary.Authorize(ctx)
	if err == nil && s == playback.AuthAuthorized {
		p.mu.Lock()
		p.applePending = false
		p.mu.Unlock()
		return s, nil
	}
	if err == nil && s == playback.AuthNotDetermined && authorizesLate(p.primary) {
		p.mu.Lock()
		first := !p.applePending && !p.appleOff
		p.applePending = true
		p.leavePrimary()
		p.mu.Unlock()
		if first {
			p.notify(waiting(p.primary))
			p.startRecheck()
		}
		return playback.AuthAuthorized, nil
	}
	p.mu.Lock()
	p.appleOff, p.applePending = true, false
	p.leavePrimary()
	p.mu.Unlock()
	p.notify(unavailable(s, err))
	return playback.AuthAuthorized, nil
}

// waiting is the notice of a primary that waits for authorization, with
// its hint on what to do (playback.AuthorizationHinter) when it has one.
func waiting(primary playback.Player) error {
	const notice = "apple music waiting for authorization"
	if h, ok := primary.(playback.AuthorizationHinter); ok {
		if hint := h.AuthorizationHint(); hint != "" {
			return errors.New(notice + " // " + hint)
		}
	}
	return errors.New(notice)
}

// unavailable is the notice of a primary left out for answering s, err.
func unavailable(s playback.AuthStatus, err error) error {
	reason := "access " + string(s)
	if err != nil {
		reason = err.Error()
	}
	return unavailableFor(reason)
}

// unavailableFor is the notice of a primary left out for reason.
func unavailableFor(reason string) error {
	return fmt.Errorf("apple music unavailable (%s) // local files only", reason)
}

// startRecheck starts the goroutine asking the pending primary again,
// unless one runs or the Player is closing.
func (p *Player) startRecheck() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rechecking || p.life.Err() != nil {
		return
	}
	p.rechecking = true
	p.wg.Add(1)
	go p.recheck()
}

// recheck asks the pending primary again at every tick, since the UI asks
// Authorize only at startup, until it answers authorized (then it joins,
// reported "apple music ready"), refuses or fails (then it is left out as
// by Authorize), is no longer pending (an Authorize settled it), or Close.
// A question that outlives recheckTimeout leaves it pending. The UI reads
// Supports again whenever it draws, so the catalog appears once ready.
func (p *Player) recheck() {
	defer p.wg.Done()
	ticks, stop := p.ticker()
	defer stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticks:
		}
		if !p.stillPending() {
			return
		}
		ctx, cancel := context.WithTimeout(p.life, recheckTimeout)
		s, err := p.primary.Authorize(ctx)
		timedOut := err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		if p.life.Err() != nil {
			return
		}
		if timedOut || (err == nil && s == playback.AuthNotDetermined) {
			continue
		}
		p.settle(s, err)
		return
	}
}

// stillPending reports whether the primary still waits; when not, the
// re-check ends.
func (p *Player) stillPending() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.applePending && !p.appleOff {
		return true
	}
	p.rechecking = false
	return false
}

// settle ends the re-check with the primary's answer s, err (authorized,
// refused or failed), unless something else settled the wait meanwhile.
func (p *Player) settle(s playback.AuthStatus, err error) {
	p.mu.Lock()
	p.rechecking = false
	if !p.applePending || p.appleOff {
		p.mu.Unlock()
		return
	}
	p.applePending = false
	ready := err == nil && s == playback.AuthAuthorized
	if !ready {
		p.appleOff = true
		p.leavePrimary()
	}
	p.mu.Unlock()
	if ready {
		p.notify(errors.New("apple music ready"))
		return
	}
	p.notify(unavailable(s, err))
}

// leavePrimary makes the local backend active in place of the primary.
// p.mu must be held.
func (p *Player) leavePrimary() {
	if p.active == p.primary {
		p.active = p.local
	}
}

// authorizesLate reports whether b's authorization can complete after it
// answered AuthNotDetermined (see playback.LateAuthorizer).
func authorizesLate(b playback.Player) bool {
	l, ok := b.(playback.LateAuthorizer)
	return ok && l.AuthorizesLate()
}

// Playlists lists the primary's playlists, then the local ones. A backend
// that fails is reported on Errors and left out, unless the other has
// nothing to list: then the call fails, so the UI offers a retry.
func (p *Player) Playlists(ctx context.Context) ([]playback.Playlist, error) {
	var out []playback.Playlist
	var appleErr error
	appleListed := false
	if apple, err := p.apple(); err == nil {
		pls, err := apple.Playlists(ctx)
		if err != nil {
			appleErr = err
		} else {
			appleListed = true
			for _, pl := range pls {
				if pl.Source == "" {
					pl.Source = playback.SourceApple
				}
				out = append(out, pl)
			}
		}
	}
	locals, localErr := p.local.Playlists(ctx)
	for _, pl := range locals {
		pl.Source = playback.SourceLocal
		out = append(out, pl)
	}
	switch {
	case appleErr != nil && len(locals) == 0:
		return nil, appleErr
	case localErr != nil && !appleListed:
		return nil, localErr
	}
	if appleErr != nil {
		p.notify(fmt.Errorf("apple music playlists: %w", appleErr))
	}
	if localErr != nil {
		p.notify(fmt.Errorf("local playlists: %w", localErr))
	}
	return out, nil
}

// LibraryPlaylist loads a playlist from its backend.
func (p *Player) LibraryPlaylist(ctx context.Context, playlistID string) (playback.PlaylistDetail, error) {
	b, err := p.backend(playlistID)
	if err != nil {
		return playback.PlaylistDetail{}, err
	}
	return b.LibraryPlaylist(ctx, playlistID)
}

// PlaySongs plays the songs of ids[start]'s source, from it; the songs of
// the other source are left out and reported Skipped.
func (p *Player) PlaySongs(ctx context.Context, ids []string, start int) (playback.QueueReport, error) {
	if start < 0 || start >= len(ids) {
		return playback.QueueReport{}, fmt.Errorf("composite: start %d out of range", start)
	}
	src := playback.SourceOf(ids[start])
	b, err := p.backend(ids[start])
	if err != nil {
		return playback.QueueReport{}, err
	}
	var keep, skipped []string
	at := 0
	for i, id := range ids {
		if playback.SourceOf(id) != src {
			skipped = append(skipped, id)
			continue
		}
		if i == start {
			at = len(keep)
		}
		keep = append(keep, id)
	}
	p.switchTo(ctx, b)
	rep, err := b.PlaySongs(ctx, keep, at)
	if len(skipped) > 0 {
		rep.Skipped = append(skipped, rep.Skipped...)
	}
	return rep, err
}

// PlayPlaylist plays a playlist on its backend.
func (p *Player) PlayPlaylist(ctx context.Context, id string) error {
	b, err := p.backend(id)
	if err != nil {
		return err
	}
	p.switchTo(ctx, b)
	return b.PlayPlaylist(ctx, id)
}

// PlayPlaylistFrom plays a playlist on its backend from index start.
func (p *Player) PlayPlaylistFrom(ctx context.Context, playlistID string, start int) error {
	b, err := p.backend(playlistID)
	if err != nil {
		return err
	}
	p.switchTo(ctx, b)
	return b.PlayPlaylistFrom(ctx, playlistID, start)
}

// The transport, seek, repeat and volume act on the active backend.

func (p *Player) Pause(ctx context.Context) error    { return p.current().Pause(ctx) }
func (p *Player) Resume(ctx context.Context) error   { return p.current().Resume(ctx) }
func (p *Player) Next(ctx context.Context) error     { return p.current().Next(ctx) }
func (p *Player) Previous(ctx context.Context) error { return p.current().Previous(ctx) }
func (p *Player) Stop(ctx context.Context) error     { return p.current().Stop(ctx) }

func (p *Player) Seek(ctx context.Context, position time.Duration) error {
	return p.current().Seek(ctx, position)
}

func (p *Player) SetRepeat(ctx context.Context, mode playback.RepeatMode) error {
	return p.current().SetRepeat(ctx, mode)
}

func (p *Player) Volume(ctx context.Context) (float64, error) { return p.current().Volume(ctx) }

func (p *Player) SetVolume(ctx context.Context, level float64) error {
	return p.current().SetVolume(ctx, level)
}

// The catalog, favorites and playlist editing are the primary's.

func (p *Player) SearchCatalog(ctx context.Context, term string, limit int) (playback.SearchResults, error) {
	apple, err := p.apple()
	if err != nil {
		return playback.SearchResults{}, err
	}
	return apple.SearchCatalog(ctx, term, limit)
}

func (p *Player) Artist(ctx context.Context, artistID string) (playback.ArtistDetail, error) {
	apple, err := p.catalog(artistID)
	if err != nil {
		return playback.ArtistDetail{}, err
	}
	return apple.Artist(ctx, artistID)
}

func (p *Player) Album(ctx context.Context, albumID string) (playback.AlbumDetail, error) {
	apple, err := p.catalog(albumID)
	if err != nil {
		return playback.AlbumDetail{}, err
	}
	return apple.Album(ctx, albumID)
}

func (p *Player) SongAlbum(ctx context.Context, songID string) (playback.AlbumDetail, error) {
	apple, err := p.catalog(songID)
	if err != nil {
		return playback.AlbumDetail{}, err
	}
	return apple.SongAlbum(ctx, songID)
}

func (p *Player) CatalogPlaylist(ctx context.Context, playlistID string) (playback.PlaylistDetail, error) {
	apple, err := p.catalog(playlistID)
	if err != nil {
		return playback.PlaylistDetail{}, err
	}
	return apple.CatalogPlaylist(ctx, playlistID)
}

func (p *Player) CreatePlaylist(ctx context.Context, name, description string, songIDs []string) (playback.Playlist, error) {
	apple, err := p.apple()
	if err != nil {
		return playback.Playlist{}, err
	}
	for _, id := range songIDs {
		if playback.SourceOf(id) == playback.SourceLocal {
			return playback.Playlist{}, playback.ErrUnsupported
		}
	}
	return apple.CreatePlaylist(ctx, name, description, songIDs)
}

func (p *Player) AddToPlaylist(ctx context.Context, playlistID string, songIDs []string) error {
	apple, err := p.catalog(playlistID)
	if err != nil {
		return err
	}
	for _, id := range songIDs {
		if playback.SourceOf(id) == playback.SourceLocal {
			return playback.ErrUnsupported
		}
	}
	return apple.AddToPlaylist(ctx, playlistID, songIDs)
}

func (p *Player) Favorite(ctx context.Context, songID string) (bool, error) {
	apple, err := p.catalog(songID)
	if err != nil {
		return false, err
	}
	return apple.Favorite(ctx, songID)
}

// Favorites asks the primary about the Apple Music ids; local ids are
// answered not loved, so a page mixing both still reads at once.
func (p *Player) Favorites(ctx context.Context, songIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(songIDs))
	var ask []string
	for _, id := range songIDs {
		if playback.SourceOf(id) == playback.SourceLocal {
			out[id] = false
			continue
		}
		ask = append(ask, id)
	}
	if len(ask) == 0 {
		return out, nil
	}
	apple, err := p.apple()
	if err != nil {
		return nil, err
	}
	loved, err := apple.Favorites(ctx, ask)
	if err != nil {
		return nil, err
	}
	for id, on := range loved {
		out[id] = on
	}
	return out, nil
}

func (p *Player) SetFavorite(ctx context.Context, songID string, on bool) error {
	apple, err := p.catalog(songID)
	if err != nil {
		return err
	}
	return apple.SetFavorite(ctx, songID, on)
}

// States delivers the active backend's states. Closed by Close.
func (p *Player) States() <-chan playback.State { return p.states }

// Errors delivers both backends' errors and the Player's own notices.
// Closed by Close.
func (p *Player) Errors() <-chan error { return p.errs }

// Levels delivers the active backend's readings, when it measures any
// (see playback.LevelSource). Closed by Close.
func (p *Player) Levels() <-chan playback.Spectrum { return p.levels }

// LibraryChanged relays the local backend's (see playback.LibraryWatcher).
// Closed by Close.
func (p *Player) LibraryChanged() <-chan struct{} { return p.changed }

// Close closes both backends, at once (each bounds its own fade and
// shutdown), then the channels. It is idempotent; later calls return the
// first one's result.
func (p *Player) Close() error {
	p.closeOnce.Do(func() {
		// Under mu, so no re-check starts once Close waits for them.
		p.mu.Lock()
		p.end()
		close(p.done)
		p.mu.Unlock()
		bs := p.backends()
		errs := make([]error, len(bs))
		var wg sync.WaitGroup
		for i, b := range bs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = b.Close()
			}()
		}
		wg.Wait()
		p.wg.Wait()
		p.mu.Lock()
		defer p.mu.Unlock()
		p.closed = true
		close(p.states)
		close(p.errs)
		close(p.levels)
		close(p.changed)
		p.closeErr = errors.Join(errs...)
	})
	return p.closeErr
}

var (
	_ playback.Player         = (*Player)(nil)
	_ playback.LevelSource    = (*Player)(nil)
	_ playback.Capabilities   = (*Player)(nil)
	_ playback.LibraryWatcher = (*Player)(nil)
)
