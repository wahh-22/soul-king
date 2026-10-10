package composite

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wahh-22/nu11signal/internal/playback"
	"github.com/wahh-22/nu11signal/internal/playback/playbacktest"
)

type warningBackend struct{ *playbacktest.Fake }

func (*warningBackend) StartupWarning() string {
	return "apple music has no sound: install libpulse (libpulse0)"
}

func TestStartupWarningReportedOnce(t *testing.T) {
	apple := &warningBackend{playbacktest.New()}
	p := New(apple, playbacktest.New())
	defer p.Close()
	if err := recv(t, p.Errors()); err.Error() != apple.StartupWarning() {
		t.Fatalf("notice = %v", err)
	}
	for range 2 {
		if _, err := p.Authorize(ctx); err != nil {
			t.Fatal(err)
		}
	}
	quiet(t, p.Errors())
}

var ctx = context.Background()

const (
	loc1 = "local:aaaa"
	loc2 = "local:bbbb"
	locP = "local:pl:cccc"
)

func methods(f *playbacktest.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		out = append(out, c.Method)
	}
	return out
}

func called(f *playbacktest.Fake, method string) []playbacktest.Call {
	var out []playbacktest.Call
	for _, c := range f.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func newPair(t *testing.T) (*Player, *playbacktest.Fake, *playbacktest.Fake) {
	t.Helper()
	apple, local := playbacktest.New(), playbacktest.New()
	p := New(apple, local)
	t.Cleanup(func() { _ = p.Close() })
	return p, apple, local
}

// recv waits for one value of ch.
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("nothing delivered")
	}
	var zero T
	return zero
}

// quiet checks ch delivers nothing for a moment.
func quiet[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("unexpected %+v", v)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPlaylistsMergeAppleThenLocalWithTheirSource(t *testing.T) {
	p, apple, local := newPair(t)
	apple.PlaylistsResult = []playback.Playlist{{ID: "p.1", Name: "Night Drive", Editable: true}}
	local.PlaylistsResult = []playback.Playlist{{ID: locP, Name: "Music"}}
	got, err := p.Playlists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []playback.Playlist{
		{ID: "p.1", Name: "Night Drive", Editable: true, Source: playback.SourceApple},
		{ID: locP, Name: "Music", Source: playback.SourceLocal},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Playlists = %+v\nwant %+v", got, want)
	}
}

func TestPlaylistsKeepLocalWhenAppleFails(t *testing.T) {
	p, apple, local := newPair(t)
	apple.MethodErr = map[string]error{"Playlists": errors.New("offline")}
	local.PlaylistsResult = []playback.Playlist{{ID: locP, Name: "Music"}}
	got, err := p.Playlists(ctx)
	if err != nil || len(got) != 1 || got[0].ID != locP {
		t.Fatalf("Playlists = %+v, %v; want the local one", got, err)
	}
	if err := recv(t, p.Errors()); err == nil {
		t.Error("the Apple failure was not reported")
	}
	local.PlaylistsResult = nil
	if _, err := p.Playlists(ctx); err == nil {
		t.Error("with nothing local, the Apple failure should fail Playlists")
	}
}

func TestPlaySongsRoutesByTheStartSongAndSkipsTheOtherSource(t *testing.T) {
	p, apple, local := newPair(t)
	rep, err := p.PlaySongs(ctx, []string{"s1", loc1, "s2", loc2}, 1)
	if err != nil {
		t.Fatal(err)
	}
	calls := called(local, "PlaySongs")
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, []any{[]string{loc1, loc2}, 0}) {
		t.Errorf("local PlaySongs = %+v, want [loc1 loc2] from 0", calls)
	}
	if !reflect.DeepEqual(rep.Skipped, []string{"s1", "s2"}) {
		t.Errorf("Skipped = %v, want the Apple songs", rep.Skipped)
	}
	if n := len(called(apple, "PlaySongs")); n != 0 {
		t.Errorf("Apple played %d times", n)
	}

	rep, err = p.PlaySongs(ctx, []string{"s1", loc1, "s2"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	calls = called(apple, "PlaySongs")
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].Args, []any{[]string{"s1", "s2"}, 1}) {
		t.Errorf("Apple PlaySongs = %+v, want [s1 s2] from 1", calls)
	}
	if !reflect.DeepEqual(rep.Skipped, []string{loc1}) {
		t.Errorf("Skipped = %v", rep.Skipped)
	}
}

func TestTransportGoesToTheActiveBackendAndSwitchingStopsTheOther(t *testing.T) {
	p, apple, local := newPair(t)
	if err := p.PlayPlaylist(ctx, locP); err != nil {
		t.Fatal(err)
	}
	if n := len(called(apple, "Stop")); n != 1 {
		t.Errorf("switching to local stopped Apple %d times, want once", n)
	}
	_ = p.Pause(ctx)
	_ = p.Next(ctx)
	_ = p.Seek(ctx, time.Second)
	_ = p.SetVolume(ctx, 0.5)
	_ = p.SetRepeat(ctx, playback.RepeatAll)
	for _, m := range []string{"Pause", "Next", "Seek", "SetVolume", "SetRepeat"} {
		if len(called(local, m)) != 1 || len(called(apple, m)) != 0 {
			t.Errorf("%s: local %v, apple %v", m, methods(local), methods(apple))
		}
	}
	if err := p.PlayPlaylistFrom(ctx, "p.1", 2); err != nil {
		t.Fatal(err)
	}
	if n := len(called(local, "Stop")); n != 1 {
		t.Errorf("switching to Apple stopped local %d times, want once", n)
	}
	_ = p.Resume(ctx)
	if len(called(apple, "Resume")) != 1 {
		t.Errorf("Resume after the switch went to %v", methods(local))
	}
	// Playing on the same source again stops nothing.
	_, _ = p.PlaySongs(ctx, []string{"s1"}, 0)
	if n := len(called(local, "Stop")); n != 1 {
		t.Errorf("local stopped %d times", n)
	}
}

func TestStatesAndLevelsComeFromTheActiveBackend(t *testing.T) {
	p, apple, local := newPair(t)
	// Apple is active until something local plays.
	apple.PushState(playback.State{Title: "apple"})
	if s := recv(t, p.States()); s.Title != "apple" {
		t.Fatalf("state = %+v", s)
	}
	local.PushState(playback.State{Title: "local idle"})
	quiet(t, p.States())

	if _, err := p.PlaySongs(ctx, []string{loc1}, 0); err != nil {
		t.Fatal(err)
	}
	apple.PushState(playback.State{Title: "apple stopped"})
	local.PushState(playback.State{Title: "local playing"})
	if s := recv(t, p.States()); s.Title != "local playing" {
		t.Fatalf("state = %+v, want the local one", s)
	}
	apple.PushLevels(playback.Spectrum{Bands: []float64{0.1}})
	quiet(t, p.Levels())
	local.PushLevels(playback.Spectrum{Bands: []float64{0.9}})
	if l := recv(t, p.Levels()); len(l.Bands) != 1 || l.Bands[0] != 0.9 {
		t.Fatalf("levels = %+v, want the local reading", l)
	}
}

func TestErrorsOfBothBackendsAreMerged(t *testing.T) {
	p, apple, local := newPair(t)
	apple.PushError(errors.New("a"))
	local.PushError(errors.New("b"))
	got := []string{recv(t, p.Errors()).Error(), recv(t, p.Errors()).Error()}
	slices.Sort(got)
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("errors = %v", got)
	}
}

func TestSupportsPerId(t *testing.T) {
	p, _, _ := newPair(t)
	for _, c := range []playback.Capability{playback.CapCatalogSearch, playback.CapFavorites, playback.CapEditPlaylists} {
		if !p.Supports(c, "s1") || !p.Supports(c, "") {
			t.Errorf("%s: Apple ids and the player should support it", c)
		}
		if p.Supports(c, loc1) || p.Supports(c, locP) {
			t.Errorf("%s: local ids should not support it", c)
		}
	}
}

// limited is a primary offering only some capabilities, as the Linux
// bridge does.
type limited struct {
	*playbacktest.Fake
	offers map[playback.Capability]bool
}

func (l limited) Supports(c playback.Capability, _ string) bool { return l.offers[c] }

func TestSupportsAsksAPrimaryWithCapabilities(t *testing.T) {
	apple := limited{playbacktest.New(), map[playback.Capability]bool{playback.CapCatalogSearch: true}}
	p := New(apple, playbacktest.New())
	defer p.Close()
	if !p.Supports(playback.CapCatalogSearch, "s1") || !p.Supports(playback.CapCatalogSearch, "") {
		t.Error("search is offered by the primary")
	}
	for _, c := range []playback.Capability{playback.CapFavorites, playback.CapEditPlaylists} {
		if p.Supports(c, "s1") || p.Supports(c, "") {
			t.Errorf("%s: the primary does not offer it", c)
		}
	}
}

func TestLocalOnlyHasNoCatalog(t *testing.T) {
	local := playbacktest.New()
	local.PlaylistsResult = []playback.Playlist{{ID: locP, Name: "Music"}}
	p := New(nil, local)
	defer p.Close()
	if s, err := p.Authorize(ctx); err != nil || s != playback.AuthAuthorized {
		t.Fatalf("Authorize = %v, %v", s, err)
	}
	if p.Supports(playback.CapCatalogSearch, "") || p.Supports(playback.CapFavorites, "") || p.Supports(playback.CapEditPlaylists, "") {
		t.Error("a player without Apple Music should support no catalog capability")
	}
	checks := map[string]error{}
	_, checks["SearchCatalog"] = p.SearchCatalog(ctx, "x", 5)
	_, checks["Artist"] = p.Artist(ctx, "a")
	_, checks["Album"] = p.Album(ctx, "a")
	_, checks["SongAlbum"] = p.SongAlbum(ctx, "s1")
	_, checks["CatalogPlaylist"] = p.CatalogPlaylist(ctx, "p")
	_, checks["CreatePlaylist"] = p.CreatePlaylist(ctx, "n", "", nil)
	checks["AddToPlaylist"] = p.AddToPlaylist(ctx, "p.1", []string{"s1"})
	_, checks["Favorite"] = p.Favorite(ctx, "s1")
	checks["SetFavorite"] = p.SetFavorite(ctx, "s1", true)
	_, checks["PlaySongs"] = p.PlaySongs(ctx, []string{"s1"}, 0)
	for name, err := range checks {
		if !errors.Is(err, playback.ErrUnsupported) {
			t.Errorf("%s = %v, want ErrUnsupported", name, err)
		}
	}
	if pls, err := p.Playlists(ctx); err != nil || len(pls) != 1 {
		t.Errorf("Playlists = %v, %v", pls, err)
	}
	// Local is active from the start: its states drive the UI.
	local.PushState(playback.State{Title: "local"})
	if s := recv(t, p.States()); s.Title != "local" {
		t.Errorf("state = %+v", s)
	}
}

func TestLocalOnlyReportsWhyAppleMusicIsUnavailableOnce(t *testing.T) {
	local := playbacktest.New()
	p := LocalOnly(local, "web player did not load")
	defer p.Close()
	if err := recv(t, p.Errors()); err.Error() != "apple music unavailable (web player did not load) // local files only" {
		t.Fatalf("notice = %q", err)
	}
	if s, err := p.Authorize(ctx); err != nil || s != playback.AuthAuthorized {
		t.Fatalf("Authorize = %v, %v", s, err)
	}
	quiet(t, p.Errors())
	if p.Supports(playback.CapCatalogSearch, "") {
		t.Error("a local-only player offers the catalog")
	}
	local.PushState(playback.State{Title: "local"})
	if s := recv(t, p.States()); s.Title != "local" {
		t.Errorf("state = %+v", s)
	}
}

func TestAppleRefusalLeavesLocalUsable(t *testing.T) {
	p, apple, _ := newPair(t)
	apple.AuthStatus = playback.AuthDenied
	if s, err := p.Authorize(ctx); err != nil || s != playback.AuthAuthorized {
		t.Fatalf("Authorize = %v, %v; want authorized for the local files", s, err)
	}
	if err := recv(t, p.Errors()); err == nil {
		t.Error("the refusal was not reported")
	}
	if p.Supports(playback.CapCatalogSearch, "") {
		t.Error("search should be unsupported once Apple Music refused")
	}
	if _, err := p.SearchCatalog(ctx, "x", 5); !errors.Is(err, playback.ErrUnsupported) {
		t.Errorf("SearchCatalog = %v", err)
	}
}

// late is a primary whose authorization can complete after the first
// Authorize (see playback.LateAuthorizer), answering status.
type late struct {
	*playbacktest.Fake
	mu     sync.Mutex
	status playback.AuthStatus
	err    error
	// hold, when set, makes Authorize report on it and then wait for its
	// context to end.
	hold chan struct{}
}

func newLate(t *testing.T) (*Player, *late, *playbacktest.Fake) {
	t.Helper()
	apple := &late{Fake: playbacktest.New(), status: playback.AuthNotDetermined}
	local := playbacktest.New()
	p := New(apple, local)
	t.Cleanup(func() { _ = p.Close() })
	return p, apple, local
}

func (l *late) AuthorizesLate() bool { return true }

func (l *late) Authorize(ctx context.Context) (playback.AuthStatus, error) {
	if _, err := l.Fake.Authorize(ctx); err != nil {
		return "", err
	}
	l.mu.Lock()
	status, err, hold := l.status, l.err, l.hold
	l.mu.Unlock()
	if hold != nil {
		hold <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	return status, nil
}

func (l *late) set(s playback.AuthStatus) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.status = s
}

// hinted is a late authorizer that tells the user how to authorize it.
type hinted struct{ *late }

func (hinted) AuthorizationHint() string { return "run sign-in" }

// The waiting notice carries the primary's hint, when it has one.
func TestAWaitingPrimarysHintIsInTheNotice(t *testing.T) {
	apple := hinted{&late{Fake: playbacktest.New(), status: playback.AuthNotDetermined}}
	p := New(apple, playbacktest.New())
	t.Cleanup(func() { _ = p.Close() })
	mustAuthorize(t, p)
	if err := recv(t, p.Errors()); err.Error() != "apple music waiting for authorization // run sign-in" {
		t.Errorf("notice = %v", err)
	}
}

func mustAuthorize(t *testing.T, p *Player) {
	t.Helper()
	if s, err := p.Authorize(ctx); err != nil || s != playback.AuthAuthorized {
		t.Fatalf("Authorize = %v, %v; want authorized", s, err)
	}
}

func TestALateAuthorizerWaitsThenJoins(t *testing.T) {
	p, apple, local := newLate(t)
	mustAuthorize(t, p)
	if err := recv(t, p.Errors()); err.Error() != "apple music waiting for authorization" {
		t.Errorf("notice = %v", err)
	}
	if p.Supports(playback.CapCatalogSearch, "") {
		t.Error("search offered before Apple Music is authorized")
	}
	if _, err := p.SearchCatalog(ctx, "x", 5); !errors.Is(err, playback.ErrUnsupported) {
		t.Errorf("SearchCatalog = %v, want ErrUnsupported", err)
	}
	_ = p.Pause(ctx)
	if len(called(local, "Pause")) != 1 || len(called(apple.Fake, "Pause")) != 0 {
		t.Errorf("Pause while waiting went to apple %v, local %v", methods(apple.Fake), methods(local))
	}

	// Still waiting: asked again, but not reported again.
	mustAuthorize(t, p)
	quiet(t, p.Errors())
	if n := len(called(apple.Fake, "Authorize")); n != 2 {
		t.Errorf("Apple asked %d times, want 2", n)
	}

	apple.set(playback.AuthAuthorized)
	mustAuthorize(t, p)
	quiet(t, p.Errors())
	if !p.Supports(playback.CapCatalogSearch, "") || !p.Supports(playback.CapFavorites, "s1") {
		t.Error("the catalog is not offered once Apple Music is authorized")
	}
	if _, err := p.SearchCatalog(ctx, "x", 5); err != nil || len(called(apple.Fake, "SearchCatalog")) != 1 {
		t.Errorf("SearchCatalog = %v; apple %v", err, methods(apple.Fake))
	}
	if _, err := p.PlaySongs(ctx, []string{"s1"}, 0); err != nil || len(called(apple.Fake, "PlaySongs")) != 1 {
		t.Errorf("PlaySongs = %v; apple %v", err, methods(apple.Fake))
	}
}

func TestALateAuthorizerThatRefusesIsLeftOut(t *testing.T) {
	for _, first := range []playback.AuthStatus{playback.AuthDenied, playback.AuthNotDetermined} {
		p, apple, _ := newLate(t)
		apple.set(first)
		mustAuthorize(t, p)
		if first == playback.AuthNotDetermined {
			recv(t, p.Errors()) // waiting
			apple.set(playback.AuthDenied)
			mustAuthorize(t, p)
		}
		if err := recv(t, p.Errors()); err.Error() != "apple music unavailable (access denied) // local files only" {
			t.Errorf("%s: notice = %v", first, err)
		}
		asked := len(called(apple.Fake, "Authorize"))
		apple.set(playback.AuthAuthorized)
		mustAuthorize(t, p)
		if n := len(called(apple.Fake, "Authorize")); n != asked {
			t.Errorf("%s: a refused Apple Music was asked again", first)
		}
		if p.Supports(playback.CapCatalogSearch, "") {
			t.Errorf("%s: search offered after a refusal", first)
		}
	}
}

func TestAPlainPrimaryNotDeterminedIsLeftOut(t *testing.T) {
	p, apple, _ := newPair(t)
	apple.AuthStatus = playback.AuthNotDetermined
	mustAuthorize(t, p)
	if err := recv(t, p.Errors()); err.Error() != "apple music unavailable (access notDetermined) // local files only" {
		t.Errorf("notice = %v", err)
	}
	apple.AuthStatus = playback.AuthAuthorized
	mustAuthorize(t, p)
	if n := len(called(apple, "Authorize")); n != 1 {
		t.Errorf("Apple asked %d times, want once", n)
	}
	if p.Supports(playback.CapCatalogSearch, "") {
		t.Error("search offered after notDetermined from a primary that cannot authorize late")
	}
}

func TestALateAuthorizerIsSafeForConcurrentUse(t *testing.T) {
	p, apple, _ := newLate(t)
	go func() {
		for range p.Errors() {
		}
	}()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 20 {
				if i == 0 && j == 10 {
					apple.set(playback.AuthAuthorized)
				}
				_, _ = p.Authorize(ctx)
				_ = p.Supports(playback.CapCatalogSearch, "")
				_, _ = p.SearchCatalog(ctx, "x", 5)
				_, _ = p.Playlists(ctx)
			}
		}()
	}
	wg.Wait()
	mustAuthorize(t, p)
	if !p.Supports(playback.CapCatalogSearch, "") {
		t.Error("the catalog is not offered once Apple Music is authorized")
	}
}

// manualTicks makes p's re-check of a pending primary tick only when the
// test sends on the returned channel, and counts the re-checks started.
func manualTicks(p *Player) (chan time.Time, *atomic.Int32) {
	ticks := make(chan time.Time)
	started := new(atomic.Int32)
	p.ticker = func() (<-chan time.Time, func()) {
		started.Add(1)
		return ticks, func() {}
	}
	return ticks, started
}

// mustTick waits for the re-check to take one tick: it asked once more for
// every earlier tick.
func mustTick(t *testing.T, ticks chan<- time.Time) {
	t.Helper()
	select {
	case ticks <- time.Time{}:
	case <-time.After(2 * time.Second):
		t.Fatal("the re-check did not take a tick")
	}
}

// noTick checks no re-check is waiting for a tick.
func noTick(t *testing.T, ticks chan<- time.Time) {
	t.Helper()
	select {
	case ticks <- time.Time{}:
		t.Fatal("a re-check is still running")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAPendingPrimaryIsAskedAgainUntilAuthorized(t *testing.T) {
	p, apple, _ := newLate(t)
	ticks, started := manualTicks(p)
	mustAuthorize(t, p)
	if err := recv(t, p.Errors()); err.Error() != "apple music waiting for authorization" {
		t.Fatalf("notice = %v", err)
	}
	for range 3 {
		mustTick(t, ticks)
	}
	quiet(t, p.Errors())
	if p.Supports(playback.CapCatalogSearch, "") {
		t.Error("search offered while still waiting")
	}

	apple.set(playback.AuthAuthorized)
	mustTick(t, ticks)
	if err := recv(t, p.Errors()); err.Error() != "apple music ready" {
		t.Fatalf("notice = %v, want apple music ready", err)
	}
	if !p.Supports(playback.CapCatalogSearch, "") {
		t.Error("the catalog is not offered once Apple Music is authorized")
	}
	if n := len(called(apple.Fake, "Authorize")); n != 5 {
		t.Errorf("Apple asked %d times, want 1 + 4 ticks", n)
	}
	noTick(t, ticks)
	if n := started.Load(); n != 1 {
		t.Errorf("%d re-checks started, want 1", n)
	}
	if _, err := p.PlaySongs(ctx, []string{"s1"}, 0); err != nil || len(called(apple.Fake, "PlaySongs")) != 1 {
		t.Errorf("PlaySongs = %v; apple %v", err, methods(apple.Fake))
	}
}

func TestAPendingPrimaryThatRefusesOrFailsIsLeftOut(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status playback.AuthStatus
		err    error
		want   string
	}{
		{"denied", playback.AuthDenied, nil, "apple music unavailable (access denied) // local files only"},
		{"failed", "", errors.New("bridge denied the request"), "apple music unavailable (bridge denied the request) // local files only"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, apple, _ := newLate(t)
			ticks, _ := manualTicks(p)
			mustAuthorize(t, p)
			recv(t, p.Errors()) // waiting
			apple.mu.Lock()
			apple.status, apple.err = tt.status, tt.err
			apple.mu.Unlock()
			mustTick(t, ticks)
			if err := recv(t, p.Errors()); err.Error() != tt.want {
				t.Fatalf("notice = %v, want %s", err, tt.want)
			}
			noTick(t, ticks)
			asked := len(called(apple.Fake, "Authorize"))
			apple.set(playback.AuthAuthorized)
			mustAuthorize(t, p)
			if n := len(called(apple.Fake, "Authorize")); n != asked {
				t.Error("a refused Apple Music was asked again")
			}
			if p.Supports(playback.CapCatalogSearch, "") {
				t.Error("search offered after a refusal")
			}
		})
	}
}

// An Authorize call that settles the wait ends the re-check quietly; a
// primary pending again is re-checked by the same one.
func TestAuthorizeAndTheRecheckShareOneWait(t *testing.T) {
	p, apple, _ := newLate(t)
	ticks, started := manualTicks(p)
	mustAuthorize(t, p)
	recv(t, p.Errors()) // waiting
	mustAuthorize(t, p)
	mustAuthorize(t, p)
	apple.set(playback.AuthAuthorized)
	mustAuthorize(t, p)
	if !p.Supports(playback.CapCatalogSearch, "") {
		t.Fatal("the catalog is not offered once Apple Music is authorized")
	}
	apple.set(playback.AuthNotDetermined)
	mustAuthorize(t, p) // pending again, before the re-check ticked
	recv(t, p.Errors()) // waiting
	apple.set(playback.AuthAuthorized)
	mustTick(t, ticks)
	if err := recv(t, p.Errors()); err.Error() != "apple music ready" {
		t.Fatalf("notice = %v, want apple music ready", err)
	}
	noTick(t, ticks)
	if n := started.Load(); n != 1 {
		t.Errorf("%d re-checks started, want 1", n)
	}

	// Settled by Authorize between ticks: the next tick ends it quietly.
	p, apple, _ = newLate(t)
	ticks, _ = manualTicks(p)
	mustAuthorize(t, p)
	recv(t, p.Errors()) // waiting
	apple.set(playback.AuthAuthorized)
	mustAuthorize(t, p)
	asked := len(called(apple.Fake, "Authorize"))
	mustTick(t, ticks)
	noTick(t, ticks)
	quiet(t, p.Errors())
	if n := len(called(apple.Fake, "Authorize")); n != asked {
		t.Errorf("an authorized Apple Music was asked again")
	}
}

func TestNoRecheckWithoutAPendingPrimary(t *testing.T) {
	plain, apple, _ := newPair(t)
	_, plainStarted := manualTicks(plain)
	apple.AuthStatus = playback.AuthNotDetermined
	mustAuthorize(t, plain)

	authorized, lateApple, _ := newLate(t)
	_, authorizedStarted := manualTicks(authorized)
	lateApple.set(playback.AuthAuthorized)
	mustAuthorize(t, authorized)
	mustAuthorize(t, authorized)

	denied, deniedApple, _ := newLate(t)
	_, deniedStarted := manualTicks(denied)
	deniedApple.set(playback.AuthDenied)
	mustAuthorize(t, denied)

	localOnly := New(nil, playbacktest.New())
	t.Cleanup(func() { _ = localOnly.Close() })
	_, localStarted := manualTicks(localOnly)
	mustAuthorize(t, localOnly)

	for name, n := range map[string]int32{
		"plain primary":      plainStarted.Load(),
		"authorized primary": authorizedStarted.Load(),
		"denied primary":     deniedStarted.Load(),
		"no primary":         localStarted.Load(),
	} {
		if n != 0 {
			t.Errorf("%s: %d re-checks started, want none", name, n)
		}
	}
}

// Close ends the re-check, waiting for a tick or asking, and nothing is
// reported after it.
func TestCloseEndsTheRecheck(t *testing.T) {
	for _, asking := range []bool{false, true} {
		p, apple, _ := newLate(t)
		ticks, started := manualTicks(p)
		mustAuthorize(t, p)
		recv(t, p.Errors()) // waiting
		if asking {
			hold := make(chan struct{})
			apple.mu.Lock()
			apple.hold = hold
			apple.mu.Unlock()
			mustTick(t, ticks)
			recv(t, hold)
		}
		closed := make(chan error, 1)
		go func() { closed <- p.Close() }()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Fatalf("asking %v: Close did not end the re-check", asking)
		}
		for err := range p.Errors() {
			t.Errorf("asking %v: reported %v after Close", asking, err)
		}
		if n := started.Load(); n != 1 {
			t.Errorf("asking %v: %d re-checks started, want 1", asking, n)
		}
	}
}

func TestFavoritesAnswerLocalIdsAsNotLoved(t *testing.T) {
	p, apple, _ := newPair(t)
	apple.Loved = map[string]bool{"s1": true}
	got, err := p.Favorites(ctx, []string{"s1", loc1})
	if err != nil {
		t.Fatal(err)
	}
	if !got["s1"] || got[loc1] {
		t.Errorf("Favorites = %v", got)
	}
	if c := called(apple, "Favorites"); len(c) != 1 || !reflect.DeepEqual(c[0].Args, []any{[]string{"s1"}}) {
		t.Errorf("Apple Favorites = %+v, want only the Apple id", c)
	}
}

func TestCloseClosesBothAndTheChannels(t *testing.T) {
	apple, local := playbacktest.New(), playbacktest.New()
	p := New(apple, local)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if !apple.Closed() || !local.Closed() {
		t.Error("Close did not close both backends")
	}
	for range p.States() {
	}
	for range p.Errors() {
	}
	for range p.Levels() {
	}
	_ = p.Close()
}

var (
	_ playback.Player         = (*Player)(nil)
	_ playback.LevelSource    = (*Player)(nil)
	_ playback.Capabilities   = (*Player)(nil)
	_ playback.LibraryWatcher = (*Player)(nil)
)

func TestAHelperThatExitsHandsOverToLocal(t *testing.T) {
	p, apple, local := newPair(t)
	_ = apple.Close() // the helper exited
	if s := recv(t, p.States()); s.Status != playback.StatusStopped {
		t.Fatalf("state = %+v, want stopped", s)
	}
	if err := recv(t, p.Errors()); err == nil {
		t.Fatal("the exit was not reported")
	}
	if p.Supports(playback.CapCatalogSearch, "") {
		t.Error("the catalog outlived the helper")
	}
	local.PushState(playback.State{Title: "local"})
	if s := recv(t, p.States()); s.Title != "local" {
		t.Errorf("state = %+v, want the local backend's", s)
	}
}
