package webplayer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wahh-22/nu11signal/internal/playback"
)

// lp is the path of the library playlists; pid is the playlist the
// track fixtures belong to.
const (
	lp  = "/v1/me/library/playlists"
	pid = "p.A1b2C3d4E5f6G7"
)

func TestPlaylistsFollowPagesInAPIOrder(t *testing.T) {
	p, page := catalogPlayer(t, routes(map[string]string{
		lp + "?limit=100": fixture(t, "library-playlists.json"),
		lp + "?offset=3":  fixture(t, "library-playlists-2.json"),
	}))
	lists, err := p.Playlists(context.Background())
	if err != nil {
		t.Fatalf("Playlists: %v", err)
	}
	want := []playback.Playlist{
		{ID: "p.A1b2C3d4E5f6G7", Name: "Afternoon Focus", Editable: true},
		{ID: "p.B2c3D4e5F6g7H8", Name: "Evening Calm"},
		{ID: "p.C3d4E5f6G7h8I9", Name: "Morning Run", Editable: true},
		{ID: "p.D4e5F6g7H8i9J0", Name: "Road Trip", Editable: true},
		{ID: "p.E5f6G7h8I9j0K1", Name: "Weekend Mix", Editable: true},
	}
	if !slices.Equal(lists, want) {
		t.Errorf("playlists = %+v\nwant %+v", lists, want)
	}
	if sent, want := page.sent(), []string{lp + "?limit=100", lp + "?offset=3"}; !slices.Equal(sent, want) {
		t.Errorf("sent %q\nwant %q", sent, want)
	}
}

func TestPlaylistsEmptyLibrary(t *testing.T) {
	p, _ := catalogPlayer(t, func(apiCall) (int, string) { return 200, `{"data":[]}` })
	lists, err := p.Playlists(context.Background())
	if err != nil || lists == nil || len(lists) != 0 {
		t.Errorf("Playlists = %v, %v; want an empty list", lists, err)
	}
}

// trackRoutes serves the playlist fixtures of pid.
func trackRoutes(t *testing.T) func(a apiCall) (int, string) {
	return routes(map[string]string{
		lp + "/" + pid + "?":                 fixture(t, "library-playlist.json"),
		lp + "/" + pid + "/tracks?limit=100": fixture(t, "library-playlist-tracks.json"),
		lp + "/" + pid + "/tracks?offset=4":  fixture(t, "library-playlist-tracks-2.json"),
	})
}

// wantTracks are the songs of the track fixtures: library songs with a
// catalog id play as it, the others are LibraryOnly, and the music video
// is left out.
var wantTracks = []playback.Song{
	{ID: "1000000001", Title: "Opening Theme", Artist: "Example Artist", Album: "Example Album", Duration: 215 * time.Second},
	{ID: "i.BBBB2222cccc", Title: "Home Recording", Artist: "Uploader", Album: "Demos", Duration: 180500 * time.Millisecond, LibraryOnly: true},
	{ID: "1000000003", Title: "Catalog Song", Artist: "Another Artist", Album: "Another Album", Duration: 241 * time.Second},
	{ID: "1000000005", Title: "Closing Theme", Artist: "Example Artist", Album: "Example Album", Duration: 199999 * time.Millisecond},
	{ID: "i.EEEE5555ffff", Title: "Odd Catalog Id", Artist: "Example Artist", Album: "Example Album", LibraryOnly: true},
}

func TestLibraryPlaylistMapsCatalogAndLibraryOnlySongs(t *testing.T) {
	p, page := catalogPlayer(t, trackRoutes(t))
	d, err := p.LibraryPlaylist(context.Background(), pid)
	if err != nil {
		t.Fatalf("LibraryPlaylist: %v", err)
	}
	if want := (playback.CatalogPlaylist{ID: pid, Name: "Afternoon Focus"}); d.Playlist != want {
		t.Errorf("playlist = %+v, want %+v", d.Playlist, want)
	}
	if !slices.Equal(d.Tracks, wantTracks) {
		t.Errorf("tracks = %+v\nwant %+v", d.Tracks, wantTracks)
	}
	if want := "Quiet songs\nfor working & reading."; d.Notes != want {
		t.Errorf("notes = %q, want %q", d.Notes, want)
	}
	sent := page.sent()
	slices.Sort(sent)
	want := []string{lp + "/" + pid + "/tracks?limit=100", lp + "/" + pid + "/tracks?offset=4", lp + "/" + pid + "?"}
	if !slices.Equal(sent, want) {
		t.Errorf("sent %q\nwant %q", sent, want)
	}
}

func TestLibraryPlaylistFailures(t *testing.T) {
	ctx := context.Background()
	// The playlist read failing fails the page, whatever the songs did.
	p, _ := catalogPlayer(t, func(a apiCall) (int, string) {
		if strings.HasSuffix(a.path, "/tracks") {
			return 200, fixture(t, "library-playlist-tracks-2.json")
		}
		return 404, pageText
	})
	if _, err := p.LibraryPlaylist(ctx, pid); !errors.Is(err, ErrCatalogNotFound) {
		t.Errorf("LibraryPlaylist of a missing playlist = %v, want ErrCatalogNotFound", err)
	}
	// So does a page of songs failing.
	p, _ = catalogPlayer(t, func(a apiCall) (int, string) {
		if strings.HasSuffix(a.path, "/tracks") {
			return 500, pageText
		}
		return 200, fixture(t, "library-playlist.json")
	})
	if _, err := p.LibraryPlaylist(ctx, pid); !errors.Is(err, ErrCatalogUnavailable) {
		t.Errorf("LibraryPlaylist with failing songs = %v, want ErrCatalogUnavailable", err)
	}
	// A reply without a data array is malformed, not an empty playlist.
	p, _ = catalogPlayer(t, func(apiCall) (int, string) { return 200, `{"results":{}}` })
	if _, err := p.LibraryPlaylist(ctx, pid); !errors.Is(err, ErrCatalogUnavailable) {
		t.Errorf("LibraryPlaylist without data = %v, want ErrCatalogUnavailable", err)
	}
	if _, err := p.Playlists(ctx); !errors.Is(err, ErrCatalogUnavailable) {
		t.Errorf("Playlists without data = %v, want ErrCatalogUnavailable", err)
	}
}

// plays lists the play() calls the page received.
func plays(page *catalogPage) []string {
	var out []string
	for _, c := range page.Calls() {
		if strings.HasPrefix(c, "play(") {
			out = append(out, c)
		}
	}
	return out
}

func TestPlayPlaylistQueuesCatalogSongs(t *testing.T) {
	ctx := context.Background()
	const queue = `["1000000001","1000000003","1000000005"]`
	cases := []struct {
		name string
		play func(p *Player) error
		want string
	}{
		{"PlayPlaylist", func(p *Player) error { return p.PlayPlaylist(ctx, pid) }, "play(" + queue + ",0)"},
		{"from the first", func(p *Player) error { return p.PlayPlaylistFrom(ctx, pid, 0) }, "play(" + queue + ",0)"},
		// The start keeps pointing at the chosen song past a skipped one.
		{"from a catalog song", func(p *Player) error { return p.PlayPlaylistFrom(ctx, pid, 2) }, "play(" + queue + ",1)"},
		{"from the last playable", func(p *Player) error { return p.PlayPlaylistFrom(ctx, pid, 3) }, "play(" + queue + ",2)"},
	}
	for _, tc := range cases {
		p, page := catalogPlayer(t, trackRoutes(t))
		if err := tc.play(p); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := plays(page); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("%s: plays %q, want %q", tc.name, got, tc.want)
		}
		// Only the songs are read, not the playlist itself.
		if sent, want := page.sent(), []string{lp + "/" + pid + "/tracks?limit=100", lp + "/" + pid + "/tracks?offset=4"}; !slices.Equal(sent, want) {
			t.Errorf("%s: sent %q, want %q", tc.name, sent, want)
		}
		select {
		case s := <-p.States():
			if s.Status != playback.StatusPlaying {
				t.Errorf("%s: state %+v, want playing", tc.name, s)
			}
		default:
			t.Errorf("%s: no state", tc.name)
		}
	}
}

func TestPlayPlaylistRefusesLibraryOnlyStarts(t *testing.T) {
	ctx := context.Background()
	p, page := catalogPlayer(t, trackRoutes(t))
	for _, start := range []int{1, 4} {
		err := p.PlayPlaylistFrom(ctx, pid, start)
		if !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "not in the apple music catalog") {
			t.Errorf("PlayPlaylistFrom(%d) of a LibraryOnly song = %v", start, err)
		} else if strings.Contains(err.Error(), wantTracks[start].Title) {
			t.Errorf("error %q names the song", err)
		}
	}
	if err := p.PlayPlaylistFrom(ctx, pid, 5); !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "has 5 songs") {
		t.Errorf("PlayPlaylistFrom past the end = %v", err)
	}
	n := len(page.sent())
	if err := p.PlayPlaylistFrom(ctx, pid, -1); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("PlayPlaylistFrom(-1) = %v", err)
	}
	if len(page.sent()) != n {
		t.Error("PlayPlaylistFrom(-1) read the playlist")
	}
	if got := plays(page); len(got) != 0 {
		t.Errorf("plays %q, want none", got)
	}

	onlyLibrary := `{"data":[{"id":"i.X1","type":"library-songs","attributes":{"name":"Upload"}}]}`
	for name, body := range map[string]string{"empty": `{"data":[]}`, "library songs only": onlyLibrary} {
		p, page := catalogPlayer(t, func(apiCall) (int, string) { return 200, body })
		if err := p.PlayPlaylist(ctx, pid); !errors.Is(err, ErrNothingToPlay) {
			t.Errorf("%s: PlayPlaylist = %v, want ErrNothingToPlay", name, err)
		}
		if got := plays(page); len(got) != 0 {
			t.Errorf("%s: plays %q", name, got)
		}
	}
}

func TestLibraryQueue(t *testing.T) {
	songs := func(ids ...string) []playback.Song {
		var out []playback.Song
		for _, id := range ids {
			out = append(out, playback.Song{ID: id, LibraryOnly: strings.HasPrefix(id, "i.")})
		}
		return out
	}
	ids, at, err := libraryQueue(songs("i.1", "i.2", "3", "i.4", "5"), 0, false)
	if err != nil || !slices.Equal(ids, []string{"3", "5"}) || at != 0 {
		t.Errorf("libraryQueue from the first = %q, %d, %v", ids, at, err)
	}
	ids, at, err = libraryQueue(songs("i.1", "i.2", "3", "i.4", "5"), 4, true)
	if err != nil || !slices.Equal(ids, []string{"3", "5"}) || at != 1 {
		t.Errorf("libraryQueue from 4 = %q, %d, %v", ids, at, err)
	}
}

func TestFavoritesReadRatings(t *testing.T) {
	ctx := context.Background()
	p, page := catalogPlayer(t, routes(map[string]string{
		"/v1/me/ratings/songs?ids=" + commas("1000000001,1000000003,1000000005"):  fixture(t, "ratings-songs.json"),
		"/v1/me/ratings/library-songs?ids=" + commas("i.BBBB2222cccc,i.EEEE5555"): fixture(t, "ratings-library-songs.json"),
	}))
	ids := []string{"1000000001", "i.BBBB2222cccc", "1000000003", "1000000001", "i.EEEE5555", "1000000005"}
	loved, err := p.Favorites(ctx, ids)
	if err != nil {
		t.Fatalf("Favorites: %v", err)
	}
	want := map[string]bool{"1000000001": true, "i.BBBB2222cccc": true, "1000000003": false, "i.EEEE5555": false, "1000000005": false}
	if !maps.Equal(loved, want) {
		t.Errorf("Favorites = %v, want %v", loved, want)
	}
	sent := page.sent()
	slices.Sort(sent)
	wantSent := []string{
		"/v1/me/ratings/library-songs?ids=" + commas("i.BBBB2222cccc,i.EEEE5555"),
		"/v1/me/ratings/songs?ids=" + commas("1000000001,1000000003,1000000005"),
	}
	if !slices.Equal(sent, wantSent) {
		t.Errorf("sent %q\nwant %q", sent, wantSent)
	}

	// A ratings read answered 404 rated none of its songs.
	p, _ = catalogPlayer(t, func(apiCall) (int, string) { return 404, pageText })
	if loved, err := p.Favorites(ctx, []string{"1", "i.2"}); err != nil || !maps.Equal(loved, map[string]bool{"1": false, "i.2": false}) {
		t.Errorf("Favorites answered 404 = %v, %v", loved, err)
	}
	// No ids ask nothing.
	p, page = catalogPlayer(t, func(apiCall) (int, string) { return 200, `{"data":[]}` })
	if loved, err := p.Favorites(ctx, nil); err != nil || loved == nil || len(loved) != 0 {
		t.Errorf("Favorites(nil) = %v, %v", loved, err)
	}
	// An id of neither kind fails before anything is asked.
	for _, id := range []string{"", "p.1", "i.", "abc", "1234567890123", "1,2", "i.a/b", "i..x", "local:1"} {
		if _, err := p.Favorites(ctx, []string{"1", id}); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Favorites with %q = %v, want ErrInvalidArgument", id, err)
		}
	}
	if sent := page.sent(); len(sent) != 0 {
		t.Errorf("sent %q", sent)
	}
}

// commas encodes a query value as apiCall.String does.
func commas(v string) string { return strings.ReplaceAll(v, ",", "%2C") }

func TestFavoritesBatchesAndFailures(t *testing.T) {
	ctx := context.Background()
	var ids []string
	for i := range 250 {
		ids = append(ids, strconv.Itoa(1000+i))
	}
	p, page := catalogPlayer(t, func(apiCall) (int, string) { return 200, `{"data":[{"id":"1000","attributes":{"value":1}}]}` })
	loved, err := p.Favorites(ctx, ids)
	if err != nil || len(loved) != 250 || !loved["1000"] || loved["1001"] {
		t.Fatalf("Favorites of 250 = %d answers, %v", len(loved), err)
	}
	var sizes []int
	for _, a := range page.sent() {
		_, q, _ := strings.Cut(a, "ids=")
		sizes = append(sizes, strings.Count(q, "%2C")+1)
	}
	slices.Sort(sizes)
	if want := []int{50, 100, 100}; !slices.Equal(sizes, want) {
		t.Errorf("batch sizes %v, want %v", sizes, want)
	}

	for name, serve := range map[string]func(apiCall) (int, string){
		"no data": func(apiCall) (int, string) { return 200, `{"errors":[]}` },
		"one fails": func(a apiCall) (int, string) {
			if strings.Contains(a.path, "library") {
				return 500, pageText
			}
			return 200, `{"data":[]}`
		},
		"refused":    func(apiCall) (int, string) { return 403, pageText },
		"wrong type": func(apiCall) (int, string) { return 200, `{"data":{"id":1}}` },
	} {
		p, _ := catalogPlayer(t, serve)
		_, err := p.Favorites(ctx, []string{"1", "i.2"})
		if err == nil {
			t.Errorf("%s: Favorites succeeded", name)
			continue
		}
		noErrorLeak(t, err)
	}
}

func TestLibraryPathAllowlist(t *testing.T) {
	for _, p := range []string{
		lp, lp + "/" + pid, lp + "/" + pid + "/tracks", lp + "/p.a.b", "/v1/me/ratings/songs", "/v1/me/ratings/library-songs",
	} {
		if !libraryPathValid(p) {
			t.Errorf("libraryPathValid(%q) = false", p)
		}
	}
	refused := []string{
		"", "/v1/me", "/v1/me/", "/v1/me/account", "/v1/me/storefront", "/v1/me/library", "/v1/me/library/songs",
		"/v1/me/library/albums", "/v1/me/library/search", "/v1/me/recent/played", "/v1/me/history/heavy-rotation",
		"/v1/me/recommendations", "/v1/me/ratings/albums", "/v1/me/ratings/songs/1000000001",
		"/v1/me/ratings/library-songs/i.1", "/v1/me/favorites", lp + "/", lp + "/" + pid + "/", lp + "/" + pid + "/catalog",
		lp + "/" + pid + "/tracks/1", lp + "/" + pid + "/tracks?offset=1", lp + "/pl.u-123", lp + "/p.", lp + "/p..x",
		lp + "/p.a/../b", lp + "/p.a%2Fb", lp + "/p.a-b", lp + "/p." + strings.Repeat("a", 65), lp + "/" + pid + "#x",
		"/v1/ME/library/playlists", "/v1/me/library/playlists/../songs", "//v1/me/library/playlists",
		"https://api.music.apple.com" + lp, "/v1/catalog/us/me/library/playlists",
	}
	for _, p := range refused {
		if libraryPathValid(p) {
			t.Errorf("libraryPathValid(%q) = true", p)
		}
	}
	// fetch refuses them too, before the page.
	p, page := catalogPlayer(t, func(apiCall) (int, string) { return 200, `{"data":[]}` })
	for _, path := range refused {
		if _, err := p.fetch(context.Background(), path, nil); !errors.Is(err, ErrCatalogUnavailable) {
			t.Errorf("fetch(%q) = %v, want ErrCatalogUnavailable", path, err)
		}
	}
	if calls := page.Calls(); len(calls) != 0 {
		t.Errorf("page calls %q", calls)
	}
}

func TestLibraryInvalidIDsAreNeverSent(t *testing.T) {
	p, page := catalogPlayer(t, func(apiCall) (int, string) { return 200, `{"data":[]}` })
	ctx := context.Background()
	for _, id := range []string{"", "p.", "pl.u-1", "123", "i.123", "p.a/b", "p..x", "p.a?x", "p.a b", "P.abc", "local:pl:x"} {
		if _, err := p.LibraryPlaylist(ctx, id); !errors.Is(err, ErrInvalidLibraryID) {
			t.Errorf("LibraryPlaylist(%q) = %v", id, err)
		}
		if err := p.PlayPlaylist(ctx, id); !errors.Is(err, ErrInvalidLibraryID) {
			t.Errorf("PlayPlaylist(%q) = %v", id, err)
		}
		if err := p.PlayPlaylistFrom(ctx, id, 0); !errors.Is(err, ErrInvalidLibraryID) {
			t.Errorf("PlayPlaylistFrom(%q) = %v", id, err)
		}
	}
	if calls := page.Calls(); len(calls) != 0 {
		t.Errorf("page calls %q", calls)
	}
}

func TestLibraryNextPages(t *testing.T) {
	ctx := context.Background()
	for _, next := range []string{
		"/v1/me/library/songs?offset=3",
		lp + "/p.B2c3D4e5F6g7H8?offset=3",
		lp + "/" + pid + "/tracks?offset=3",
		"/v1/catalog/us/playlists?offset=3",
		"https://api.music.apple.com" + lp + "?offset=3",
		"//evil.example" + lp + "?offset=3",
		lp + "/../library/playlists?offset=3",
		lp + "/?offset=3",
		lp + "?offset=3&offset=4",
		lp + "?offset=3#x",
		lp + "%3Foffset=3",
		"/v1/me/account?offset=3",
	} {
		p, page := catalogPlayer(t, func(apiCall) (int, string) {
			return 200, `{"next":"` + next + `","data":[{"id":"p.1","attributes":{"name":"one"}}]}`
		})
		if _, err := p.Playlists(ctx); !errors.Is(err, ErrCatalogUnavailable) {
			t.Errorf("next %q: Playlists = %v, want ErrCatalogUnavailable", next, err)
		}
		if sent := page.sent(); len(sent) != 1 {
			t.Errorf("next %q: sent %q, want the first page alone", next, sent)
		}
	}

	// A next pointing back to an offset already read ends the list.
	p, page := catalogPlayer(t, func(a apiCall) (int, string) {
		if a.params["offset"] == "1" {
			return 200, `{"next":"` + lp + `","data":[{"id":"p.2","attributes":{"name":"two"}}]}`
		}
		return 200, `{"next":"` + lp + `?offset=1","data":[{"id":"p.1","attributes":{"name":"one"}}]}`
	})
	lists, err := p.Playlists(ctx)
	if err != nil || len(lists) != 2 {
		t.Errorf("Playlists with a looping next = %v, %v", lists, err)
	}
	if sent, want := page.sent(), []string{lp + "?limit=100", lp + "?offset=1"}; !slices.Equal(sent, want) {
		t.Errorf("sent %q, want %q", sent, want)
	}
}

func TestLibraryReadsAreBounded(t *testing.T) {
	// Every page is full and has a next: the reads stop at their caps.
	full := func(prefix, typ string) func(a apiCall) (int, string) {
		return func(a apiCall) (int, string) {
			offset, _ := strconv.Atoi(a.params["offset"])
			var items []string
			for i := range libraryPageLimit {
				items = append(items, fmt.Sprintf(`{"id":"%s%d","type":"%s","attributes":{"name":"n","playParams":{"catalogId":"%d"}}}`, prefix, offset+i, typ, 100+offset+i))
			}
			return 200, fmt.Sprintf(`{"next":"%s?offset=%d","data":[%s]}`, a.path, offset+libraryPageLimit, strings.Join(items, ","))
		}
	}
	p, page := catalogPlayer(t, full("p.", "library-playlists"))
	lists, err := p.Playlists(context.Background())
	if err != nil || len(lists) != maxLibraryPlaylists {
		t.Errorf("Playlists = %d, %v; want %d", len(lists), err, maxLibraryPlaylists)
	}
	if n := len(page.sent()); n != maxLibraryPlaylists/libraryPageLimit {
		t.Errorf("Playlists sent %d requests", n)
	}
	p, page = catalogPlayer(t, full("i.", "library-songs"))
	d, err := p.LibraryPlaylist(context.Background(), pid)
	if err != nil || len(d.Tracks) != maxLibraryTracks {
		t.Errorf("LibraryPlaylist = %d tracks, %v; want %d", len(d.Tracks), err, maxLibraryTracks)
	}
	if n := len(page.sent()); n != 1+maxLibraryTracks/libraryPageLimit {
		t.Errorf("LibraryPlaylist sent %d requests", n)
	}
}

func TestLibraryStatusErrorsNeverLeak(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{401, ErrCatalogUnauthorized},
		{403, ErrCatalogUnauthorized},
		{404, ErrCatalogNotFound},
		{429, ErrCatalogUnavailable},
		{500, ErrCatalogUnavailable},
	}
	ctx := context.Background()
	for _, tc := range cases {
		page := newPage()
		page.setHandle(func(c call) (any, error) {
			return map[string]any{"status": tc.status, "error": pageText, "message": pageText}, nil
		})
		p := New(page)
		calls := map[string]func() error{
			"Playlists":        func() error { _, err := p.Playlists(ctx); return err },
			"LibraryPlaylist":  func() error { _, err := p.LibraryPlaylist(ctx, pid); return err },
			"PlayPlaylist":     func() error { return p.PlayPlaylist(ctx, pid) },
			"PlayPlaylistFrom": func() error { return p.PlayPlaylistFrom(ctx, pid, 1) },
		}
		if tc.status != 404 {
			calls["Favorites"] = func() error { _, err := p.Favorites(ctx, []string{"1"}); return err }
		}
		for name, call := range calls {
			err := call()
			if !errors.Is(err, tc.want) {
				t.Errorf("%d: %s = %v, want %v", tc.status, name, err, tc.want)
				continue
			}
			noErrorLeak(t, err)
			if !strings.Contains(err.Error(), "library ") {
				t.Errorf("%d: %s error %q does not say library", tc.status, name, err)
			}
		}
		_ = p.Close()
	}
}

func TestLibraryReadsEndWithClose(t *testing.T) {
	page := newPage()
	page.gate = make(chan struct{})
	p := New(page, WithCatalogTimeout(time.Hour))
	ctx := context.Background()
	pending := map[string]<-chan error{}
	for name, read := range map[string]func() error{
		"Playlists":       func() error { _, err := p.Playlists(ctx); return err },
		"LibraryPlaylist": func() error { _, err := p.LibraryPlaylist(ctx, pid); return err },
		"PlayPlaylist":    func() error { return p.PlayPlaylist(ctx, pid) },
		"Favorites":       func() error { _, err := p.Favorites(ctx, []string{"1"}); return err },
	} {
		pending[name] = async(read)
		waiting(t, page)
	}
	_ = p.Close()
	for name, done := range pending {
		if err := result(t, done); !errors.Is(err, ErrClosed) {
			t.Errorf("%s across Close = %v, want ErrClosed", name, err)
		}
	}
}

// writeCall is one write() call the fake page received; body is the JSON
// text sent, empty for none.
type writeCall struct{ method, path, body string }

func (w writeCall) String() string { return w.method + " " + w.path + " " + w.body }

// editPage is a catalog page whose write() calls serve answers with a
// status and a body; it records them.
type editPage struct {
	*catalogPage
	mu     sync.Mutex
	writes []writeCall
	serve  func(w writeCall) (int, string)
}

// editPlayer is a player over a page serving writes with serve and reads
// with read (nil reads answer an empty document).
func editPlayer(t *testing.T, serve func(w writeCall) (int, string), read func(apiCall) (int, string)) (*Player, *editPage) {
	t.Helper()
	if read == nil {
		read = func(apiCall) (int, string) { return 200, `{"data":[]}` }
	}
	ep := &editPage{catalogPage: newCatalogPage(t, read), serve: serve}
	api := ep.handle
	ep.setHandle(func(c call) (any, error) {
		if c.fn != "write" {
			return api(c)
		}
		var w writeCall
		var body *string
		if err := json.Unmarshal([]byte("["+c.args+"]"), &[]any{&w.method, &w.path, &body}); err != nil {
			t.Errorf("write(%s): arguments are not a method, a path and a body: %v", c.args, err)
			return nil, err
		}
		if body != nil {
			w.body = *body
		}
		ep.mu.Lock()
		ep.writes = append(ep.writes, w)
		ep.mu.Unlock()
		status, reply := ep.serve(w)
		if status < 200 || status > 299 {
			return map[string]any{"status": status, "error": "request failed"}, nil
		}
		if reply == "" {
			return map[string]any{"status": status}, nil
		}
		return map[string]any{"status": status, "body": json.RawMessage(reply)}, nil
	})
	p := New(ep)
	t.Cleanup(func() { _ = p.Close() })
	return p, ep
}

// written lists the write() calls as "METHOD path body".
func (ep *editPage) written() []string {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	var out []string
	for _, w := range ep.writes {
		out = append(out, w.String())
	}
	return out
}

const ratingBody = `{"attributes":{"value":1},"type":"rating"}`

func TestSetFavoriteLovesASong(t *testing.T) {
	p, page := editPlayer(t, func(writeCall) (int, string) {
		return 200, `{"data":[{"id":"1000000001","type":"ratings","attributes":{"value":1}}]}`
	}, nil)
	if err := p.SetFavorite(context.Background(), "1000000001", true); err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}
	want := []string{"PUT /v1/me/ratings/songs/1000000001 " + ratingBody}
	if got := page.written(); !slices.Equal(got, want) {
		t.Errorf("writes = %q\nwant %q", got, want)
	}
	if sent := page.sent(); len(sent) != 0 {
		t.Errorf("reads %q", sent)
	}
}

func TestSetFavoriteClearsARating(t *testing.T) {
	ctx := context.Background()
	// A library song is rated through its own endpoint; clearing sends
	// no body.
	p, page := editPlayer(t, func(writeCall) (int, string) { return 204, "" }, nil)
	if err := p.SetFavorite(ctx, "i.BBBB2222cccc", false); err != nil {
		t.Fatalf("SetFavorite off: %v", err)
	}
	if err := p.SetFavorite(ctx, "i.BBBB2222cccc", true); err != nil {
		t.Fatalf("SetFavorite on: %v", err)
	}
	want := []string{
		"DELETE /v1/me/ratings/library-songs/i.BBBB2222cccc ",
		"PUT /v1/me/ratings/library-songs/i.BBBB2222cccc " + ratingBody,
	}
	if got := page.written(); !slices.Equal(got, want) {
		t.Errorf("writes = %q\nwant %q", got, want)
	}
	if calls := page.Calls(); len(calls) != 2 || calls[0] != `write("DELETE","/v1/me/ratings/library-songs/i.BBBB2222cccc",null)` {
		t.Errorf("page calls %q", calls)
	}

	// Clearing a song without a rating (404) succeeds; loving one that
	// does not exist does not.
	p, _ = editPlayer(t, func(writeCall) (int, string) { return 404, pageText }, nil)
	if err := p.SetFavorite(ctx, "1000000001", false); err != nil {
		t.Errorf("SetFavorite off answered 404 = %v", err)
	}
	if err := p.SetFavorite(ctx, "1000000001", true); !errors.Is(err, ErrCatalogNotFound) {
		t.Errorf("SetFavorite on answered 404 = %v", err)
	} else {
		noErrorLeak(t, err)
	}
	p, _ = editPlayer(t, func(writeCall) (int, string) { return 500, pageText }, nil)
	if err := p.SetFavorite(ctx, "1000000001", false); !errors.Is(err, ErrCatalogUnavailable) {
		t.Errorf("SetFavorite off answered 500 = %v", err)
	}
}

func TestFavoriteReadsTheRating(t *testing.T) {
	ctx := context.Background()
	p, page := editPlayer(t, func(writeCall) (int, string) { return 200, "" }, routes(map[string]string{
		"/v1/me/ratings/songs?ids=1000000001": `{"data":[{"id":"1000000001","attributes":{"value":1}}]}`,
		"/v1/me/ratings/songs?ids=1000000002": `{"data":[{"id":"1000000002","attributes":{"value":-1}}]}`,
	}))
	for id, want := range map[string]bool{"1000000001": true, "1000000002": false, "1000000003": false} {
		if loved, err := p.Favorite(ctx, id); err != nil || loved != want {
			t.Errorf("Favorite(%s) = %v, %v; want %v", id, loved, err, want)
		}
	}
	for _, id := range []string{"", "p.1", "1234567890123", "i.a/b"} {
		if _, err := p.Favorite(ctx, id); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("Favorite(%q) = %v, want ErrInvalidArgument", id, err)
		}
	}
	if w := page.written(); len(w) != 0 {
		t.Errorf("writes %q", w)
	}
}

func TestCreatePlaylistReturnsTheNewPlaylist(t *testing.T) {
	ctx := context.Background()
	reply := `{"data":[{"id":"p.NewList1","type":"library-playlists","attributes":{"name":"Road <Trip> & co","canEdit":true}}]}`
	p, page := editPlayer(t, func(writeCall) (int, string) { return 201, reply }, nil)
	got, err := p.CreatePlaylist(ctx, "Road <Trip> & co", "For the drive", []string{"1000000001", "i.BBBB2222cccc", "1000000001"})
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	if want := (playback.Playlist{ID: "p.NewList1", Name: "Road <Trip> & co"}); got != want {
		t.Errorf("CreatePlaylist = %+v, want %+v", got, want)
	}
	// An empty description and no songs are left out; the name falls
	// back to the requested one.
	page.serve = func(writeCall) (int, string) { return 201, `{"data":[{"id":"p.NewList2"}]}` }
	got, err = p.CreatePlaylist(ctx, "Empty", "", nil)
	if err != nil || got != (playback.Playlist{ID: "p.NewList2", Name: "Empty"}) {
		t.Errorf("CreatePlaylist empty = %+v, %v", got, err)
	}
	want := []string{
		lp + ` {"attributes":{"description":"For the drive","name":"Road <Trip> & co"},"relationships":{"tracks":{"data":[` +
			`{"id":"1000000001","type":"songs"},{"id":"i.BBBB2222cccc","type":"library-songs"},{"id":"1000000001","type":"songs"}]}}}`,
		lp + ` {"attributes":{"name":"Empty"}}`,
	}
	for i, w := range page.written() {
		if i >= len(want) || w != "POST "+want[i] {
			t.Errorf("write %d = %s\nwant POST %s", i, w, want[min(i, len(want)-1)])
		}
	}
	if n := len(page.written()); n != 2 {
		t.Errorf("%d writes, want 2", n)
	}

	// A reply that does not name the playlist is an error.
	for _, bad := range []string{"", `{"data":[]}`, `{"data":[{"id":""}]}`, `{"data":[{"id":"p.a/b"}]}`, `{"data":{}}`, `[1]`} {
		page.serve = func(writeCall) (int, string) { return 201, bad }
		if _, err := p.CreatePlaylist(ctx, "x", "", nil); !errors.Is(err, ErrCatalogUnavailable) {
			t.Errorf("CreatePlaylist answered %q = %v", bad, err)
		}
	}
	// Blank names and ids of neither kind fail before anything is sent.
	before := len(page.written())
	for _, name := range []string{"", "   ", "\n\t"} {
		if _, err := p.CreatePlaylist(ctx, name, "", nil); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("CreatePlaylist(%q) = %v", name, err)
		}
	}
	for _, id := range []string{"", "p.1", "pl.1", "1234567890123", "i.", "i..x", "1,2", "local:1"} {
		if _, err := p.CreatePlaylist(ctx, "x", "", []string{"1", id}); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("CreatePlaylist with %q = %v", id, err)
		}
	}
	if n := len(page.written()); n != before {
		t.Errorf("%d writes sent for invalid creations", n-before)
	}
}

func TestAddToPlaylistSendsTheSongsInOrder(t *testing.T) {
	ctx := context.Background()
	p, page := editPlayer(t, func(writeCall) (int, string) { return 204, "" }, nil)
	// As on macOS, every song goes in one request, repeats kept.
	var ids []string
	var data []string
	for i := range 250 {
		id := strconv.Itoa(1000000000 + i)
		if i%2 == 1 {
			id = "i.Lib" + strconv.Itoa(i)
		}
		ids = append(ids, id)
		typ := "songs"
		if i%2 == 1 {
			typ = "library-songs"
		}
		data = append(data, `{"id":"`+id+`","type":"`+typ+`"}`)
	}
	ids = append(ids, ids[0])
	data = append(data, data[0])
	if err := p.AddToPlaylist(ctx, pid, ids); err != nil {
		t.Fatalf("AddToPlaylist: %v", err)
	}
	want := []string{"POST " + lp + "/" + pid + `/tracks {"data":[` + strings.Join(data, ",") + `]}`}
	if got := page.written(); !slices.Equal(got, want) {
		t.Errorf("writes = %q\nwant %q", got, want)
	}

	for _, id := range []string{"", "p.", "pl.u-1", "123", "i.123", "p.a/b", "p..x", "P.abc", "local:pl:x"} {
		if err := p.AddToPlaylist(ctx, id, []string{"1"}); !errors.Is(err, ErrInvalidLibraryID) {
			t.Errorf("AddToPlaylist(%q) = %v, want ErrInvalidLibraryID", id, err)
		}
	}
	for _, songs := range [][]string{nil, {}, {"1", "p.1"}, {"abc"}} {
		if err := p.AddToPlaylist(ctx, pid, songs); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("AddToPlaylist(%q) = %v, want ErrInvalidArgument", songs, err)
		}
	}
	if n := len(page.written()); n != 1 {
		t.Errorf("%d writes, want 1", n)
	}
}

// Statuses map as on macOS, and no error carries the page's text.
func TestLibraryEditErrorsNeverLeak(t *testing.T) {
	cases := []struct {
		status    int
		want, add error
	}{
		{401, ErrCatalogUnauthorized, ErrCatalogUnauthorized},
		{403, ErrCatalogUnauthorized, ErrPlaylistNotEditable},
		{404, ErrCatalogNotFound, ErrCatalogNotFound},
		{429, ErrCatalogUnavailable, ErrCatalogUnavailable},
		{500, ErrCatalogUnavailable, ErrCatalogUnavailable},
	}
	ctx := context.Background()
	for _, tc := range cases {
		page := newPage()
		page.setHandle(func(c call) (any, error) {
			return map[string]any{"status": tc.status, "error": pageText, "message": pageText, "body": pageText}, nil
		})
		p := New(page)
		calls := map[string]struct {
			call func() error
			want error
		}{
			"CreatePlaylist": {func() error { _, err := p.CreatePlaylist(ctx, "n", "", []string{"1"}); return err }, tc.want},
			"AddToPlaylist":  {func() error { return p.AddToPlaylist(ctx, pid, []string{"1"}) }, tc.add},
			"SetFavorite":    {func() error { return p.SetFavorite(ctx, "1", true) }, tc.want},
		}
		for name, c := range calls {
			err := c.call()
			if !errors.Is(err, c.want) {
				t.Errorf("%d: %s = %v, want %v", tc.status, name, err, c.want)
				continue
			}
			noErrorLeak(t, err)
			if !strings.Contains(err.Error(), "library ") {
				t.Errorf("%d: %s error %q does not say library", tc.status, name, err)
			}
		}
		_ = p.Close()
	}
	// A page that throws: its message stays in the page.
	page := newPage()
	page.setHandle(func(call) (any, error) { return nil, errors.New(pageText) })
	p := New(page)
	defer p.Close()
	if err := p.SetFavorite(ctx, "1", false); !errors.Is(err, ErrCatalogUnavailable) {
		t.Errorf("SetFavorite on a throwing page = %v", err)
	} else {
		noErrorLeak(t, err)
	}
}

// A creation or addition that times out may still be applied: it says so
// and is sent once, never again.
func TestLibraryEditTimeoutsAreNeverRetried(t *testing.T) {
	page := newPage()
	page.gate = make(chan struct{})
	p := New(page, WithCatalogTimeout(20*time.Millisecond))
	defer p.Close()
	ctx := context.Background()
	_, err := p.CreatePlaylist(ctx, "n", "", nil)
	if !errors.Is(err, ErrEditOutcomeUnknown) || !errors.Is(err, ErrCatalogUnavailable) {
		t.Errorf("CreatePlaylist past the timeout = %v", err)
	}
	var marked interface{ OutcomeUnknown() bool }
	if !errors.As(err, &marked) || !marked.OutcomeUnknown() {
		t.Errorf("CreatePlaylist past the timeout = %v: not marked OutcomeUnknown", err)
	}
	err = p.AddToPlaylist(ctx, pid, []string{"1"})
	if !errors.Is(err, ErrEditOutcomeUnknown) || !errors.As(err, &marked) || !marked.OutcomeUnknown() {
		t.Errorf("AddToPlaylist past the timeout = %v", err)
	}
	if err := p.SetFavorite(ctx, "1", true); !errors.Is(err, ErrCatalogUnavailable) || errors.As(err, &marked) {
		t.Errorf("SetFavorite past the timeout = %v", err)
	}
	if calls := page.Calls(); len(calls) != 3 {
		t.Errorf("page calls %q, want one write each", calls)
	}

	// A page that reloaded is asked again: the first write never ran.
	p2, ep := editPlayer(t, func(writeCall) (int, string) { return 204, "" }, nil)
	ep.missing = 1
	if err := p2.AddToPlaylist(ctx, pid, []string{"1"}); err != nil {
		t.Fatalf("AddToPlaylist after a reload: %v", err)
	}
	if n := len(ep.written()); n != 1 {
		t.Errorf("%d writes ran, want 1", n)
	}
}

func TestLibraryEditsEndWithClose(t *testing.T) {
	page := newPage()
	page.gate = make(chan struct{})
	p := New(page, WithCatalogTimeout(time.Hour))
	edit := async(func() error { return p.SetFavorite(context.Background(), "1", true) })
	waiting(t, page)
	_ = p.Close()
	if err := result(t, edit); !errors.Is(err, ErrClosed) {
		t.Errorf("SetFavorite through Close = %v, want ErrClosed", err)
	}
}

// writeValid allows exactly the library edits, with exactly their
// bodies; write refuses anything else before the page.
func TestLibraryWriteAllowlist(t *testing.T) {
	tracks := `{"data":[{"id":"1","type":"songs"},{"id":"i.A1","type":"library-songs"}]}`
	allowed := []struct{ method, path, body string }{
		{"POST", lp, `{"attributes":{"name":"n"}}`},
		{"POST", lp, `{"attributes":{"description":"d","name":"n"},"relationships":{"tracks":` + tracks + `}}`},
		{"POST", lp + "/" + pid + "/tracks", tracks},
		{"PUT", "/v1/me/ratings/songs/1000000001", ratingBody},
		{"PUT", "/v1/me/ratings/library-songs/i.A1", ratingBody},
		{"DELETE", "/v1/me/ratings/songs/1000000001", ""},
		{"DELETE", "/v1/me/ratings/library-songs/i.A1", ""},
	}
	body := func(s string) []byte {
		if s == "" {
			return nil
		}
		return []byte(s)
	}
	for _, a := range allowed {
		if !writeValid(a.method, a.path, body(a.body)) {
			t.Errorf("writeValid(%s %s %s) = false", a.method, a.path, a.body)
		}
	}
	refused := []struct{ method, path, body string }{
		// Other methods and paths.
		{"DELETE", lp + "/" + pid, ""},
		{"DELETE", lp, ""},
		{"DELETE", lp + "/" + pid + "/tracks", ""},
		{"PUT", lp + "/" + pid, `{"attributes":{"name":"n"}}`},
		{"PATCH", lp + "/" + pid, `{"attributes":{"name":"n"}}`},
		{"GET", lp, ""},
		{"post", lp, `{"attributes":{"name":"n"}}`},
		{"POST", lp + "/" + pid, tracks},
		{"POST", lp + "/" + pid + "/tracks/1", tracks},
		{"POST", lp + "/pl.u-1/tracks", tracks},
		{"POST", lp + "/p..x/tracks", tracks},
		{"POST", "/v1/me/library", tracks},
		{"POST", "/v1/me/library/songs", tracks},
		{"DELETE", "/v1/me/library/songs/i.A1", ""},
		{"POST", "/v1/me/account", `{}`},
		{"PUT", "/v1/me/account", `{}`},
		{"POST", "/v1/me/ratings/songs/1000000001", ratingBody},
		{"GET", "/v1/me/ratings/songs/1000000001", ""},
		{"PUT", "/v1/me/ratings/songs", ratingBody},
		{"PUT", "/v1/me/ratings/albums/1000000001", ratingBody},
		{"PUT", "/v1/me/ratings/songs/i.A1", ratingBody},
		{"PUT", "/v1/me/ratings/library-songs/1000000001", ratingBody},
		{"PUT", "/v1/me/ratings/songs/1234567890123", ratingBody},
		{"PUT", "/v1/me/ratings/songs/1/x", ratingBody},
		{"PUT", "/v1/me/ratings/library-songs/i..x", ratingBody},
		{"PUT", sf + "/songs/1", ratingBody},
		{"POST", "https://api.music.apple.com" + lp, `{"attributes":{"name":"n"}}`},
		// Other bodies.
		{"PUT", "/v1/me/ratings/songs/1", ""},
		{"PUT", "/v1/me/ratings/songs/1", `{"attributes":{"value":-1},"type":"rating"}`},
		{"PUT", "/v1/me/ratings/songs/1", `{"attributes":{"value":1},"type":"ratings"}`},
		{"PUT", "/v1/me/ratings/songs/1", `{"attributes":{"value":1},"type":"rating","x":1}`},
		{"PUT", "/v1/me/ratings/songs/1", `{"Attributes":{"value":1},"type":"rating"}`},
		{"PUT", "/v1/me/ratings/songs/1", `{"type":"rating","attributes":{"value":1}}`},
		{"PUT", "/v1/me/ratings/songs/1", ratingBody + ` {}`},
		{"PUT", "/v1/me/ratings/songs/1", ratingBody + `x`},
		{"DELETE", "/v1/me/ratings/songs/1", ratingBody},
		{"DELETE", "/v1/me/ratings/songs/1", `null`},
		{"POST", lp, ""},
		{"POST", lp, `{"attributes":{"name":" "}}`},
		{"POST", lp, `{"attributes":{"description":"","name":"n"}}`},
		{"POST", lp, `{"attributes":{"name":"n","x":1}}`},
		{"POST", lp, `{"attributes":{"name":"n"},"relationships":{"tracks":{"data":[]}}}`},
		{"POST", lp, `{"attributes":{"name":"n"},"relationships":null}`},
		{"POST", lp, `{"attributes":{"name":"n"},"relationships":{"tracks":{"data":[{"id":"p.1","type":"songs"}]}}}`},
		{"POST", lp + "/" + pid + "/tracks", `{"data":[]}`},
		{"POST", lp + "/" + pid + "/tracks", `{"data":[{"id":"1","type":"library-songs"}]}`},
		{"POST", lp + "/" + pid + "/tracks", `{"data":[{"id":"1","type":"songs","x":1}]}`},
		{"POST", lp + "/" + pid + "/tracks", `{"data":[{"id":"1","type":"songs"}],"x":1}`},
		{"POST", lp + "/" + pid + "/tracks", `{"data":[{"id":"1","type":"albums"}]}`},
	}
	for _, r := range refused {
		if writeValid(r.method, r.path, body(r.body)) {
			t.Errorf("writeValid(%s %s %s) = true", r.method, r.path, r.body)
		}
	}
	p, page := editPlayer(t, func(writeCall) (int, string) { return 200, `{"data":[]}` }, nil)
	for _, r := range refused {
		if _, err := p.write(context.Background(), "edit", r.method, r.path, body(r.body)); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("write(%s %s %s) = %v, want ErrInvalidArgument", r.method, r.path, r.body, err)
		}
	}
	if calls := page.Calls(); len(calls) != 0 {
		t.Errorf("page calls %q", calls)
	}
	// The reads never take an edit's path.
	for _, a := range allowed {
		if strings.HasPrefix(a.path, ratingsPrefix) && libraryPathValid(a.path) {
			t.Errorf("libraryPathValid(%q) = true", a.path)
		}
	}
}
