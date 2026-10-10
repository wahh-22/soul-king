package webplayer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wahh-22/nu11signal/internal/playback"
)

// sf is the catalog path prefix, with the storefront placeholder
// MusicKit fills in.
const sf = "/v1/catalog/{{storefrontId}}"

// pageText stands for anything the page or Apple says; no error may
// carry it.
const pageText = "Resource with requested id was not found"

// fixture reads testdata/name.
func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// apiCall is one api() call the fake page received, as "path?query"
// with the query encoded in key order.
type apiCall struct {
	path   string
	params map[string]string
}

func (a apiCall) String() string {
	q := url.Values{}
	for k, v := range a.params {
		q.Set(k, v)
	}
	return a.path + "?" + q.Encode()
}

// catalogPage is a fake page whose api() calls serve answers with a
// status and a body; it records them.
type catalogPage struct {
	*fakePage
	mu    sync.Mutex
	calls []apiCall
	serve func(a apiCall) (int, string)
}

func newCatalogPage(t *testing.T, serve func(a apiCall) (int, string)) *catalogPage {
	t.Helper()
	cp := &catalogPage{fakePage: newPage(), serve: serve}
	cp.setHandle(func(c call) (any, error) {
		if c.fn != "api" {
			return nil, nil
		}
		var path string
		var params map[string]string
		if err := json.Unmarshal([]byte("["+c.args+"]"), &[]any{&path, &params}); err != nil {
			t.Errorf("api(%s): arguments are not a path and flat params: %v", c.args, err)
			return nil, err
		}
		a := apiCall{path: path, params: params}
		cp.mu.Lock()
		cp.calls = append(cp.calls, a)
		serve := cp.serve
		cp.mu.Unlock()
		status, body := serve(a)
		if status != 200 {
			return map[string]any{"status": status, "error": "request failed"}, nil
		}
		return map[string]any{"status": 200, "body": json.RawMessage(body)}, nil
	})
	return cp
}

// sent lists the api() calls as "path?query".
func (cp *catalogPage) sent() []string {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	var out []string
	for _, a := range cp.calls {
		out = append(out, a.String())
	}
	return out
}

// routes serves the body of the first route whose key, "path?query" or
// the path alone, matches the call; anything else is a 404.
func routes(bodies map[string]string) func(a apiCall) (int, string) {
	return func(a apiCall) (int, string) {
		if body, ok := bodies[a.String()]; ok {
			return 200, body
		}
		if body, ok := bodies[a.path]; ok {
			return 200, body
		}
		return 404, ""
	}
}

func TestSearchCatalogThroughThePage(t *testing.T) {
	page := newCatalogPage(t, routes(map[string]string{sf + "/search": fixture(t, "search.json")}))
	p := New(page)
	defer p.Close()

	res, err := p.SearchCatalog(context.Background(), "radiohead", 3)
	if err != nil {
		t.Fatalf("SearchCatalog: %v", err)
	}
	wantSongs := []playback.Song{
		{ID: "1097862231", Title: "Creep", Artist: "Radiohead", Album: "Pablo Honey", Duration: 238640 * time.Millisecond},
		{ID: "1097861834", Title: "Let Down", Artist: "Radiohead", Album: "OK Computer", Duration: 299560 * time.Millisecond},
		{ID: "1109715293", Title: "All I Need", Artist: "Radiohead", Album: "In Rainbows", Duration: 228747 * time.Millisecond},
	}
	if !slices.Equal(res.Songs, wantSongs) {
		t.Errorf("songs = %+v\nwant %+v", res.Songs, wantSongs)
	}
	wantAlbums := []playback.Album{
		{ID: "1097861387", Title: "OK Computer", Artist: "Radiohead", Year: 1997, TrackCount: 12},
		{ID: "1109714933", Title: "In Rainbows", Artist: "Radiohead", Year: 2007, TrackCount: 10},
		{ID: "1097862870", Title: "Kid A", Artist: "Radiohead", Year: 2000, TrackCount: 11},
	}
	if !slices.Equal(res.Albums, wantAlbums) {
		t.Errorf("albums = %+v\nwant %+v", res.Albums, wantAlbums)
	}
	var artists []string
	for _, a := range res.Artists {
		artists = append(artists, a.ID+" "+a.Name+" "+strings.Join(a.Genres, ","))
	}
	if want := []string{"657515 Radiohead Alternative", "156334706 Ed O'Brien Pop", "39753073 Thom Yorke Alternative"}; !slices.Equal(artists, want) {
		t.Errorf("artists = %q, want %q", artists, want)
	}
	wantPlaylists := []playback.CatalogPlaylist{
		{ID: "pl.fdc21e2843764a8a99076ad5b6c40f4d", Name: "Radiohead Essentials", Curator: "Apple Music Alternative"},
		{ID: "pl.c1b72602d9f24dc9adb6e0bfd1e70573", Name: "Radiohead: Chill", Curator: "Apple Music Alternative"},
		{ID: "pl.1631c4a4658f43b88cc409c4ebb9c8fb", Name: "Radiohead: Influences", Curator: "Apple Music Alternative"},
	}
	if !slices.Equal(res.Playlists, wantPlaylists) {
		t.Errorf("playlists = %+v\nwant %+v", res.Playlists, wantPlaylists)
	}

	// The page's own api client is asked, with the params JSON-encoded.
	var search string
	for _, c := range page.Calls() {
		if strings.HasPrefix(c, `api("`+sf+`/search",`) {
			search = c
		}
	}
	want := `api("` + sf + `/search",{"limit":"3","term":"radiohead","types":"songs,albums,artists,playlists","with":"topResults"})`
	if search != want {
		t.Errorf("search call = %s\nwant %s\n(all: %q)", search, want, page.Calls())
	}
}

// catalogPlayer is a player over a catalog page serving serve.
func catalogPlayer(t *testing.T, serve func(a apiCall) (int, string), opts ...Option) (*Player, *catalogPage) {
	t.Helper()
	page := newCatalogPage(t, serve)
	p := New(page, opts...)
	t.Cleanup(func() { _ = p.Close() })
	return p, page
}

// noErrorLeak fails when err's text carries page text.
func noErrorLeak(t *testing.T, err error) {
	t.Helper()
	for _, secret := range []string{pageText, "request failed", "api failed"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q carries %q", err, secret)
		}
	}
}

// withNext returns the document fixture with the next page of its first
// resource's tracks set.
func withNext(t *testing.T, name, next string) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(fixture(t, name)), &doc); err != nil {
		t.Fatal(err)
	}
	res := doc["data"].([]any)[0].(map[string]any)
	tracks := res["relationships"].(map[string]any)["tracks"].(map[string]any)
	tracks["next"] = next
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCatalogSearchTopResultsAndSuggestions(t *testing.T) {
	top := `{"results":{"top":{"data":[
		{"id":"1","type":"stations","attributes":{"name":"Radio"}},
		{"id":"657515","type":"artists","attributes":{"name":"Radiohead","genreNames":["Alternative"]}},
		{"id":"1097862231","type":"songs","attributes":{"name":"Creep","artistName":"Radiohead","albumName":"Pablo Honey","durationInMillis":238640}},
		{"id":"1097861387","type":"albums","attributes":{"name":"OK Computer","artistName":"Radiohead","releaseDate":"1997","trackCount":12}},
		{"id":"pl.fdc21e2843764a8a99076ad5b6c40f4d","type":"playlists","attributes":{"name":"Radiohead Essentials","curatorName":"Apple Music"}}
	]}}}`
	suggestions := `{"results":{"suggestions":[
		{"kind":"terms","searchTerm":"radiohead","displayTerm":"radiohead"},
		{"kind":"topResults","content":{"id":"657515","type":"artists"}},
		{"kind":"terms","searchTerm":"radiohead creep","displayTerm":"radiohead creep"},
		{"kind":"terms","searchTerm":"radiohead ok computer"}
	]}}`
	p, page := catalogPlayer(t, routes(map[string]string{
		sf + "/search":             top,
		sf + "/search/suggestions": suggestions,
	}))
	res, err := p.SearchCatalog(context.Background(), "radiohead", 2)
	if err != nil {
		t.Fatalf("SearchCatalog: %v", err)
	}
	if want := []string{"radiohead", "radiohead creep"}; !slices.Equal(res.Suggestions, want) {
		t.Errorf("suggestions = %q, want %q", res.Suggestions, want)
	}
	var kinds []string
	for _, it := range res.Top {
		kinds = append(kinds, string(it.Kind))
	}
	if want := []string{"artist", "song"}; !slices.Equal(kinds, want) {
		t.Fatalf("top kinds = %q, want %q (stations skipped, cut at the limit)", kinds, want)
	}
	if res.Top[1].Song.Duration != 238640*time.Millisecond || res.Top[0].Artist.Name != "Radiohead" {
		t.Errorf("top = %+v", res.Top)
	}
	if res.Songs != nil || res.Albums != nil || res.Artists != nil || res.Playlists != nil {
		t.Errorf("sections = %+v, want none", res)
	}
	sent := page.sent()
	slices.Sort(sent)
	want := []string{
		sf + "/search/suggestions?kinds=terms&limit=2&term=radiohead",
		sf + "/search?limit=2&term=radiohead&types=songs%2Calbums%2Cartists%2Cplaylists&with=topResults",
	}
	if !slices.Equal(sent, want) {
		t.Errorf("sent %q\nwant %q", sent, want)
	}

	// Many top results stop at six; an album of a year-only date keeps it.
	res, err = p.SearchCatalog(context.Background(), "radiohead", 25)
	if err != nil || len(res.Top) != 4 || res.Top[2].Album.Year != 1997 || res.Top[3].Playlist.Curator != "Apple Music" {
		t.Errorf("SearchCatalog limit 25 = %+v, %v", res.Top, err)
	}
	if len(res.Suggestions) != 3 {
		t.Errorf("suggestions = %q", res.Suggestions)
	}
}

func TestCatalogSearchClampsAndEncodes(t *testing.T) {
	p, page := catalogPlayer(t, routes(map[string]string{sf + "/search": fixture(t, "empty-search.json")}))
	ctx := context.Background()
	const hostile = `AC/DC & co?#"); window.__nu11signal.stop(); ("`
	for _, limit := range []int{-1, 0, 1} {
		if _, err := p.SearchCatalog(ctx, hostile, limit); err != nil {
			t.Fatalf("SearchCatalog: %v", err)
		}
	}
	if _, err := p.SearchCatalog(ctx, "x", 99); err != nil {
		t.Fatalf("SearchCatalog: %v", err)
	}
	var limits []string
	page.mu.Lock()
	for _, a := range page.calls {
		if a.path != sf+"/search" {
			continue
		}
		limits = append(limits, a.params["limit"])
		if term := a.params["term"]; term != hostile && term != "x" {
			t.Errorf("term = %q", term)
		}
	}
	page.mu.Unlock()
	if want := []string{"1", "1", "1", "25"}; !slices.Equal(limits, want) {
		t.Errorf("limits = %q, want %q", limits, want)
	}

	n := len(page.sent())
	for _, term := range []string{"", " \t\n"} {
		if _, err := p.SearchCatalog(ctx, term, 5); !errors.Is(err, ErrEmptyTerm) {
			t.Errorf("SearchCatalog(%q) = %v, want ErrEmptyTerm", term, err)
		}
	}
	if len(page.sent()) != n {
		t.Error("a blank search was sent")
	}
}

func TestCatalogEmptySearch(t *testing.T) {
	p, _ := catalogPlayer(t, routes(map[string]string{sf + "/search": fixture(t, "empty-search.json")}))
	res, err := p.SearchCatalog(context.Background(), "zzqqxx", 5)
	if err != nil {
		t.Fatalf("SearchCatalog: %v", err)
	}
	if res.Suggestions != nil || res.Top != nil || res.Artists != nil || res.Albums != nil || res.Songs != nil || res.Playlists != nil {
		t.Errorf("results = %+v, want empty", res)
	}
}

func TestCatalogArtist(t *testing.T) {
	const base = sf + "/artists/657515"
	serve := routes(map[string]string{
		base + "?views=top-songs%2Cfull-albums": fixture(t, "artist.json"),
		base + "?extend=origin%2CbornOrFormed":  `{"data":[{"id":"657515","type":"artists","attributes":{"name":"Radiohead","origin":"Abingdon, England","bornOrFormed":"1985-01-01"}}]}`,
		base + "/view/featured-albums":          `{"data":[{"id":"1097861387","type":"albums","attributes":{"name":"OK Computer","artistName":"Radiohead","releaseDate":"1997-05-21","trackCount":12}}]}`,
		base + "/playlists":                     `{"data":[]}`,
		base + "/view/featured-playlists":       `{"data":[{"id":"pl.fdc21e2843764a8a99076ad5b6c40f4d","type":"playlists","attributes":{"name":"Radiohead Essentials","curatorName":"Apple Music Alternative"}}]}`,
	})
	p, _ := catalogPlayer(t, func(a apiCall) (int, string) {
		if a.path == base+"/view/singles" {
			return 500, ""
		}
		return serve(a) // compilation-albums: 404
	})
	d, err := p.Artist(context.Background(), "657515")
	if err != nil {
		t.Fatalf("Artist: %v", err)
	}
	if d.Artist.ID != "657515" || d.Artist.Name != "Radiohead" || !slices.Equal(d.Artist.Genres, []string{"Alternative"}) {
		t.Errorf("artist = %+v", d.Artist)
	}
	var top []string
	for _, s := range d.TopSongs {
		top = append(top, s.ID+" "+s.Title)
	}
	if want := []string{"1097862231 Creep", "1097861834 Let Down", "1097861842 No Surprises"}; !slices.Equal(top, want) {
		t.Errorf("top songs = %q, want %q", top, want)
	}
	want := playback.Album{ID: "1111577743", Title: "A Moon Shaped Pool", Artist: "Radiohead", Year: 2016, TrackCount: 11}
	if len(d.Albums) != 3 || d.Albums[0] != want {
		t.Errorf("albums = %+v, want %+v first", d.Albums, want)
	}
	if len(d.EssentialAlbums) != 1 || d.EssentialAlbums[0].Title != "OK Computer" {
		t.Errorf("essential albums = %+v", d.EssentialAlbums)
	}
	if d.Singles != nil || d.Compilations != nil {
		t.Errorf("failed sections = %+v, %+v, want empty", d.Singles, d.Compilations)
	}
	if len(d.Playlists) != 1 || d.Playlists[0].Name != "Radiohead Essentials" {
		t.Errorf("playlists = %+v, want the featured ones", d.Playlists)
	}
	if want := (playback.ArtistAbout{Genre: "Alternative", Origin: "Abingdon, England", Formed: "1985"}); d.About != want {
		t.Errorf("about = %+v, want %+v", d.About, want)
	}
}

func TestCatalogArtistOwnPlaylistsWin(t *testing.T) {
	const base = sf + "/artists/657515"
	p, page := catalogPlayer(t, routes(map[string]string{
		base:                fixture(t, "artist.json"),
		base + "/playlists": `{"data":[{"id":"pl.c1b72602d9f24dc9adb6e0bfd1e70573","type":"playlists","attributes":{"name":"Radiohead: Chill"}}]}`,
	}))
	d, err := p.Artist(context.Background(), "657515")
	if err != nil {
		t.Fatalf("Artist: %v", err)
	}
	if len(d.Playlists) != 1 || d.Playlists[0].Name != "Radiohead: Chill" {
		t.Errorf("playlists = %+v", d.Playlists)
	}
	if slices.Contains(page.sent(), base+"/view/featured-playlists?") {
		t.Error("featured playlists asked although the artist has its own")
	}
}

func TestCatalogAlbumSingle(t *testing.T) {
	p, page := catalogPlayer(t, routes(map[string]string{sf + "/albums/6817757403": fixture(t, "album.json")}))
	d, err := p.Album(context.Background(), "6817757403")
	if err != nil {
		t.Fatalf("Album: %v", err)
	}
	want := playback.AlbumDetail{
		Album: playback.Album{ID: "6817757403", Title: "SUMMER - Single", Artist: "Samuel G & Angeliyo El Blanco", Year: 2026, TrackCount: 1},
		Tracks: []playback.Track{{
			Song:   playback.Song{ID: "6817757404", Title: "SUMMER", Artist: "Samuel G & Angeliyo El Blanco", Album: "SUMMER - Single", Duration: 161630 * time.Millisecond},
			Number: 1, Disc: 1,
		}},
		Genre:       d.Genre,
		ReleaseDate: "2026-10-06",
		RecordLabel: "Samuel G",
		Copyright:   "℗ 2026 Samuel G, Distribuido en exclusiva por ADA.",
	}
	if d.Genre == "" || d.Notes != "" {
		t.Errorf("genre %q, notes %q", d.Genre, d.Notes)
	}
	if fmt.Sprintf("%+v", d) != fmt.Sprintf("%+v", want) {
		t.Errorf("album = %+v\nwant %+v", d, want)
	}
	if sent := page.sent(); !slices.Equal(sent, []string{sf + "/albums/6817757403?"}) {
		t.Errorf("sent %q", sent)
	}
	if got, want := page.Calls(), []string{`api("` + sf + `/albums/6817757403",{})`}; !slices.Equal(got, want) {
		t.Errorf("page calls = %q, want %q", got, want)
	}
}

func TestCatalogAlbumMulti(t *testing.T) {
	p, _ := catalogPlayer(t, routes(map[string]string{sf + "/albums/1097861387": fixture(t, "album-multi.json")}))
	d, err := p.Album(context.Background(), "1097861387")
	if err != nil {
		t.Fatalf("Album: %v", err)
	}
	if len(d.Tracks) != 12 || d.Album.TrackCount != 12 || d.Album.Year != 1997 || d.Genre != "Alternative" {
		t.Fatalf("album = %+v, %d tracks", d.Album, len(d.Tracks))
	}
	for i, tr := range d.Tracks {
		if tr.Number != i+1 || tr.Disc != 1 || tr.Album != "OK Computer" || tr.Duration <= 0 {
			t.Errorf("track %d = %+v", i, tr)
		}
	}
	if d.Tracks[4].ID != "1097861834" || d.Tracks[4].Title != "Let Down" {
		t.Errorf("track 5 = %+v", d.Tracks[4])
	}
	if !strings.HasPrefix(d.Notes, "100 Best Albums Few albums so audacious") || strings.ContainsAny(d.Notes, "<>") || strings.Contains(d.Notes, "\n\n") {
		t.Errorf("notes = %.120q…", d.Notes)
	}
	if d.ReleaseDate != "1997-05-21" || d.RecordLabel != "XL Recordings" {
		t.Errorf("facts = %q %q", d.ReleaseDate, d.RecordLabel)
	}
}

func TestCatalogSongAlbum(t *testing.T) {
	p, page := catalogPlayer(t, routes(map[string]string{
		sf + "/songs/6817757404?include=albums": fixture(t, "song.json"),
		sf + "/albums/6817757403":               fixture(t, "album.json"),
	}))
	d, err := p.SongAlbum(context.Background(), "6817757404")
	if err != nil {
		t.Fatalf("SongAlbum: %v", err)
	}
	if d.Album.ID != "6817757403" || len(d.Tracks) != 1 || d.Tracks[0].ID != "6817757404" {
		t.Errorf("album = %+v", d)
	}
	want := []string{sf + "/songs/6817757404?include=albums", sf + "/albums/6817757403?"}
	if sent := page.sent(); !slices.Equal(sent, want) {
		t.Errorf("sent %q, want %q", sent, want)
	}

	// A song without an album.
	page.mu.Lock()
	page.serve = routes(map[string]string{
		sf + "/songs/1": `{"data":[{"id":"1","type":"songs","attributes":{"name":"x"},"relationships":{"albums":{"data":[]}}}]}`,
	})
	page.mu.Unlock()
	if _, err := p.SongAlbum(context.Background(), "1"); !errors.Is(err, ErrCatalogNotFound) {
		t.Errorf("SongAlbum without an album = %v, want ErrCatalogNotFound", err)
	}
}

func TestCatalogPlaylistFollowsPages(t *testing.T) {
	const id = "pl.fdc21e2843764a8a99076ad5b6c40f4d"
	// Apple's next links name the storefront; they are asked under the
	// placeholder, their query as params.
	const next = "/v1/catalog/co/playlists/" + id + "/tracks"
	const tracks = sf + "/playlists/" + id + "/tracks"
	p, page := catalogPlayer(t, routes(map[string]string{
		sf + "/playlists/" + id + "?include=tracks": withNext(t, "playlist.json", next+"?offset=4"),
		tracks + "?offset=4": `{"next":"` + next + `?offset=6&l=es-MX","data":[
			{"id":"1097861770","type":"songs","attributes":{"name":"Paranoid Android","artistName":"Radiohead","albumName":"OK Computer","durationInMillis":387213}},
			{"id":"1","type":"music-videos","attributes":{"name":"A video"}}]}`,
		tracks + "?l=es-MX&offset=6": `{"data":[{"id":"1097861842","type":"songs","attributes":{"name":"No Surprises","artistName":"Radiohead","albumName":"OK Computer","durationInMillis":229120}}]}`,
	}))
	d, err := p.CatalogPlaylist(context.Background(), id)
	if err != nil {
		t.Fatalf("CatalogPlaylist: %v", err)
	}
	if d.Playlist != (playback.CatalogPlaylist{ID: id, Name: "Radiohead Essentials", Curator: "Apple Music Alternative"}) {
		t.Errorf("playlist = %+v", d.Playlist)
	}
	var titles []string
	for _, s := range d.Tracks {
		titles = append(titles, s.Title)
	}
	if len(titles) != 6 {
		t.Fatalf("tracks = %q", titles)
	}
	if want := []string{"Let Down", "Creep", "Karma Police", titles[3], "Paranoid Android", "No Surprises"}; !slices.Equal(titles, want) {
		t.Errorf("tracks = %q, want %q", titles, want)
	}
	if d.Tracks[0] != (playback.Song{ID: "1097861834", Title: "Let Down", Artist: "Radiohead", Album: "OK Computer", Duration: 299560 * time.Millisecond}) {
		t.Errorf("first track = %+v", d.Tracks[0])
	}
	if !strings.HasPrefix(d.Notes, "As hard as it is to believe now") || strings.Contains(d.Notes, "<i>") {
		t.Errorf("notes = %.80q", d.Notes)
	}
	want := []string{sf + "/playlists/" + id + "?include=tracks", tracks + "?offset=4", tracks + "?l=es-MX&offset=6"}
	if sent := page.sent(); !slices.Equal(sent, want) {
		t.Errorf("sent %q\nwant %q", sent, want)
	}
}

func TestCatalogPagesAreBounded(t *testing.T) {
	const tracks = sf + "/albums/1097861387/tracks"
	const next = "/v1/catalog/us/albums/1097861387/tracks"
	p, page := catalogPlayer(t, func(a apiCall) (int, string) {
		if a.path == tracks {
			return 200, `{"next":"` + next + `?offset=1","data":[{"id":"7","type":"songs","attributes":{"name":"again"}}]}`
		}
		return 200, withNext(t, "album-multi.json", next+"?offset=12")
	})
	d, err := p.Album(context.Background(), "1097861387")
	if err != nil {
		t.Fatalf("Album: %v", err)
	}
	if n := len(page.sent()); n != maxTrackPages {
		t.Errorf("sent %d requests, want %d", n, maxTrackPages)
	}
	if len(d.Tracks) != 12+maxTrackPages-1 {
		t.Errorf("%d tracks", len(d.Tracks))
	}
}

func TestCatalogRefusesForeignNextPages(t *testing.T) {
	for _, next := range []string{
		"https://evil.example/v1/catalog/us/albums/1/tracks?offset=1",
		"//evil.example/v1/catalog/us/albums/1/tracks",
		"https://api.music.apple.com/v1/catalog/us/albums/1/tracks",
		"/v1/catalog/us/../../me/library",
		"/v1/catalog/us/albums/1/../../../me/library/songs",
		"/v1/catalog/us/albums/1/%2e%2e/x",
		"/v1/catalog/us/albums/1/tracks#x",
		"/v1/catalog/us/albums/1/tracks?offset=1&offset=2",
		"/v1/catalog/us/albums/1/tracks?a%20b=1",
		"/v1/catalog/us/albums/1/tracks?offset=%zz",
		"/v1/catalog/us/albums/1//tracks",
		"/v1/catalog/us/albums/1/tracks/",
		"/v1/catalog/{{storefrontId}}/albums/1/tracks",
		"/v1/catalog/USA/albums/1/tracks",
		"/v1/me/library/songs",
		"/v1/catalog/us/",
		"/v1/catalog/us",
		"v1/catalog/us/albums/1/tracks",
		"javascript:alert(1)",
		`\\evil.example\v1\catalog\us\albums`,
	} {
		p, page := catalogPlayer(t, func(apiCall) (int, string) {
			return 200, withNext(t, "album-multi.json", next)
		})
		_, err := p.Album(context.Background(), "1097861387")
		if !errors.Is(err, ErrCatalogUnavailable) {
			t.Errorf("next %q: Album = %v, want ErrCatalogUnavailable", next, err)
		}
		if n := len(page.sent()); n != 1 {
			t.Errorf("next %q: sent %q, want the album alone", next, page.sent())
		}
	}
}

func TestNextPage(t *testing.T) {
	p, params, ok := nextPage("/v1/catalog/gb/playlists/pl.u-8aAVZAJIo1dDN0/tracks?offset=100&fields[songs]=name")
	if !ok || p != sf+"/playlists/pl.u-8aAVZAJIo1dDN0/tracks" || !maps.Equal(params, map[string]string{"offset": "100", "fields[songs]": "name"}) {
		t.Errorf("nextPage = %q, %v, %v", p, params, ok)
	}
	if p, params, ok := nextPage("/v1/catalog/us/albums/1/tracks"); !ok || p != sf+"/albums/1/tracks" || len(params) != 0 {
		t.Errorf("nextPage without a query = %q, %v, %v", p, params, ok)
	}
}

func TestCatalogStatusErrors(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{404, ErrCatalogNotFound},
		{401, ErrCatalogUnauthorized},
		{403, ErrCatalogUnauthorized},
		{429, ErrCatalogUnavailable},
		{500, ErrCatalogUnavailable},
		{302, ErrCatalogUnavailable},
		{0, ErrCatalogUnavailable},
	}
	for _, tc := range cases {
		page := newPage()
		page.setHandle(func(c call) (any, error) {
			// Whatever else a page might add, only the status counts.
			return map[string]any{"status": tc.status, "error": pageText, "message": pageText, "body": map[string]any{"errors": []any{map[string]any{"detail": pageText}}}}, nil
		})
		p := New(page)
		ctx := context.Background()
		calls := map[string]func() error{
			"SearchCatalog":   func() error { _, err := p.SearchCatalog(ctx, "x", 1); return err },
			"Artist":          func() error { _, err := p.Artist(ctx, "1"); return err },
			"Album":           func() error { _, err := p.Album(ctx, "1"); return err },
			"SongAlbum":       func() error { _, err := p.SongAlbum(ctx, "1"); return err },
			"CatalogPlaylist": func() error { _, err := p.CatalogPlaylist(ctx, "pl.1"); return err },
		}
		for name, call := range calls {
			err := call()
			if !errors.Is(err, tc.want) {
				t.Errorf("%d: %s = %v, want %v", tc.status, name, err, tc.want)
				continue
			}
			noErrorLeak(t, err)
		}
		_ = p.Close()
	}
}

func TestCatalogPageFailures(t *testing.T) {
	cases := map[string]struct {
		err  error
		want error
	}{
		"script error": {&ScriptError{Description: "api failed: " + pageText}, ErrCatalogUnavailable},
		"cdp error":    {&CDPError{Code: -32000, Message: pageText}, ErrCatalogUnavailable},
		"browser gone": {fmt.Errorf("%w: read: %s", ErrBrowserGone, pageText), ErrBrowserGone},
	}
	for name, tc := range cases {
		page := newPage()
		page.setHandle(func(call) (any, error) { return nil, tc.err })
		p := New(page)
		_, err := p.Album(context.Background(), "1")
		if !errors.Is(err, ErrCatalogUnavailable) || !errors.Is(err, tc.want) {
			t.Errorf("%s: Album = %v, want %v", name, err, tc.want)
		} else {
			noErrorLeak(t, err)
		}
		_ = p.Close()
	}
}

func TestCatalogMalformedReplies(t *testing.T) {
	cases := map[string]any{
		"null":         nil,
		"not a reply":  "nope",
		"no body":      map[string]any{"status": 200},
		"wrong schema": map[string]any{"status": 200, "body": map[string]any{"data": map[string]any{"id": 1}}},
		"odd status":   map[string]any{"status": "200", "body": map[string]any{"data": []any{}}},
	}
	for name, reply := range cases {
		page := newPage()
		page.setHandle(func(call) (any, error) { return reply, nil })
		p := New(page)
		if _, err := p.Album(context.Background(), "6817757403"); !errors.Is(err, ErrCatalogUnavailable) {
			t.Errorf("%s: Album = %v, want ErrCatalogUnavailable", name, err)
		}
		_ = p.Close()
	}

	// An empty document is not found.
	p, _ := catalogPlayer(t, func(apiCall) (int, string) { return 200, `{"data":[]}` })
	if _, err := p.Album(context.Background(), "1"); !errors.Is(err, ErrCatalogNotFound) {
		t.Errorf("Album of an empty document = %v, want ErrCatalogNotFound", err)
	}
}

func TestCatalogInvalidIDsAreNeverSent(t *testing.T) {
	p, page := catalogPlayer(t, func(apiCall) (int, string) { return 200, "{}" })
	ctx := context.Background()
	bad := []string{"", "abc", "1/../2", "1?x=1", "12a", "i.123", "-1", "١٢٣", strings.Repeat("1", 21)}
	for _, id := range bad {
		if _, err := p.Artist(ctx, id); !errors.Is(err, ErrInvalidCatalogID) {
			t.Errorf("Artist(%q) = %v", id, err)
		}
		if _, err := p.Album(ctx, id); !errors.Is(err, ErrInvalidCatalogID) {
			t.Errorf("Album(%q) = %v", id, err)
		}
		if _, err := p.SongAlbum(ctx, id); !errors.Is(err, ErrInvalidCatalogID) {
			t.Errorf("SongAlbum(%q) = %v", id, err)
		}
	}
	for _, id := range []string{"", "pl.", "p.123", "123", "pl.a/b", "pl.a.b", "pl.a?x", "pl.../x", "PL.abc", "pl." + strings.Repeat("a", 65)} {
		if _, err := p.CatalogPlaylist(ctx, id); !errors.Is(err, ErrInvalidCatalogID) {
			t.Errorf("CatalogPlaylist(%q) = %v", id, err)
		}
	}
	if calls := page.Calls(); len(calls) != 0 {
		t.Errorf("page calls %q", calls)
	}
	for _, id := range []string{"pl.u-8aAVZAJIo1dDN0", "pl.fdc21e2843764a8a99076ad5b6c40f4d"} {
		if !playlistIDValid(id) {
			t.Errorf("playlistIDValid(%q) = false", id)
		}
	}
}

func TestFetchRefusesPathsOutsideTheCatalog(t *testing.T) {
	p, page := catalogPlayer(t, func(apiCall) (int, string) { return 200, "{}" })
	ctx := context.Background()
	for _, path := range []string{
		"", "/v1/me/library/songs", "/v1/catalog/us/albums/1", sf, sf + "/", sf + "/albums/../../me",
		sf + "/albums//1", sf + "/albums/1?x=1", sf + "/albums/1#x", "https://evil.example" + sf + "/albums/1",
	} {
		if _, err := p.fetch(ctx, path, nil); !errors.Is(err, ErrCatalogUnavailable) {
			t.Errorf("fetch(%q) = %v, want ErrCatalogUnavailable", path, err)
		}
	}
	for _, params := range []map[string]string{{"": "1"}, {"a b": "1"}, {"a=b": "1"}, {"a&b": "1"}, {strings.Repeat("k", 65): "1"}} {
		if _, err := p.fetch(ctx, sf+"/albums/1", params); !errors.Is(err, ErrCatalogUnavailable) {
			t.Errorf("fetch with params %q = %v, want ErrCatalogUnavailable", params, err)
		}
	}
	if calls := page.Calls(); len(calls) != 0 {
		t.Errorf("page calls %q", calls)
	}
}

func TestCatalogContextTimeoutAndClose(t *testing.T) {
	// The page never answers: a request ends with its timeout.
	page := newPage()
	page.gate = make(chan struct{})
	p := New(page, WithCatalogTimeout(20*time.Millisecond))
	defer p.Close()
	_, err := p.Album(context.Background(), "1")
	if !errors.Is(err, ErrCatalogUnavailable) || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("Album past the timeout = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.SearchCatalog(ctx, "x", 1); !errors.Is(err, context.Canceled) {
		t.Errorf("SearchCatalog with a cancelled context = %v", err)
	}

	slow := New(page, WithCatalogTimeout(time.Hour))
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := slow.Artist(ctx, "1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Artist past the context deadline = %v", err)
	}

	// Close ends a read in flight.
	for len(page.waiting) > 0 {
		<-page.waiting
	}
	read := async(func() error { _, err := slow.CatalogPlaylist(context.Background(), "pl.1"); return err })
	waiting(t, page)
	_ = slow.Close()
	if err := result(t, read); !errors.Is(err, ErrClosed) {
		t.Errorf("CatalogPlaylist across Close = %v, want ErrClosed", err)
	}
}

func TestCatalogReadsDoNotWaitForCommands(t *testing.T) {
	page := newCatalogPage(t, routes(map[string]string{sf + "/albums/6817757403": fixture(t, "album.json")}))
	p := New(page)
	defer p.Close()
	// A command holds the player's slot.
	p.slot <- struct{}{}
	defer func() { <-p.slot }()
	if _, err := p.Album(context.Background(), "6817757403"); err != nil {
		t.Errorf("Album while a command is pending = %v", err)
	}
}

func TestPlainText(t *testing.T) {
	cases := map[string]string{
		"<b>Bold</b> and <i>it</i>":                 "Bold and it",
		"one<br>two<BR/>three</p>four</div >five":   "one\ntwo\nthree\nfour\nfive",
		"a &amp; b &lt;c&gt; &#39;d&#x2014;e&#X41;": "a & b <c> 'd—eA",
		"&unknown; &#xZZ; &#99999999999;":           "&unknown; &#xZZ; &#99999999999;",
		"esc&#27;[31m red\x1b[0m\ttab\r\n":          "esc[31m red[0m tab",
		"  spaced \n\n\n  lines  \u00a0 ":           "spaced\nlines",
		"":                                          "",
	}
	for in, want := range cases {
		if got := plainText(in); got != want {
			t.Errorf("plainText(%q) = %q, want %q", in, got, want)
		}
	}
}
