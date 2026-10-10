package webplayer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wahh-22/nu11signal/internal/playback"
)

// The Apple Music API, as the page's MusicKit reaches it with its own
// developer token: paths carry the storefront placeholder, which
// MusicKit replaces with the signed-in account's storefront.
const (
	catalogPrefix         = "/v1/catalog/"
	storefrontPlaceholder = "{{storefrontId}}"
	// DefaultCatalogTimeout bounds one catalog request. A reply larger
	// than the DevTools message cap never arrives, so this also ends it.
	DefaultCatalogTimeout = 10 * time.Second

	// maxSearchLimit is the most results per type the search serves.
	maxSearchLimit = 25
	// maxSuggestions is the most terms the suggestions endpoint serves.
	maxSuggestions = 10
	// maxTopResults keeps the top results a short list above the
	// per-type sections, as in Apple Music.
	maxTopResults = 6
	// maxTrackPages bounds the pages of tracks read for one album or
	// playlist, the first included; longer lists are cut there.
	maxTrackPages = 10
	// playlistIDLimit bounds the part of a playlist id after "pl.".
	playlistIDLimit = 64
	// paramKeyLimit bounds the name of a request parameter.
	paramKeyLimit = 64
)

var (
	// ErrCatalogUnauthorized is Apple refusing the page's request (401
	// or 403): the page's own token expired or the account cannot read
	// the catalog.
	ErrCatalogUnauthorized = errors.New("webplayer: apple music refused the catalog request")
	// ErrCatalogNotFound is a catalog item that does not exist (404), or
	// a song without an album.
	ErrCatalogNotFound = errors.New("webplayer: not found in the apple music catalog")
	// ErrCatalogUnavailable is any other failure: a page that cannot
	// answer, another status, a malformed reply, a next page outside the
	// catalog or a request timeout.
	ErrCatalogUnavailable = errors.New("webplayer: apple music catalog unavailable")
	// ErrInvalidCatalogID is returned, before anything reaches the page,
	// for an id that is not a catalog id: digits, or "pl." and a name for
	// a playlist.
	ErrInvalidCatalogID = errors.New("webplayer: invalid apple music catalog id")
	// ErrEmptyTerm is a search for a blank term.
	ErrEmptyTerm = errors.New("webplayer: empty search term")
)

// catalog reads the Apple Music catalog of the page's storefront through
// fetch, and maps Apple's JSON to playback types. Methods are safe for
// concurrent use.
type catalog struct {
	// fetch reads one catalog path (see catalogPath) with flat params and
	// returns the reply's JSON; its errors wrap a catalog sentinel or are
	// ctx's, and never carry the reply.
	fetch func(ctx context.Context, path string, params map[string]string) ([]byte, error)
}

// fetch reads one catalog or library resource through the page's own
// MusicKit API client (the bootstrap's api), bounded by the catalog
// timeout. Nothing reaches the page for a path outside the catalog and
// the library reads (see libraryPathValid).
func (p *Player) fetch(ctx context.Context, path string, params map[string]string) ([]byte, error) {
	if !catalogPathValid(path) && !libraryPathValid(path) || !paramsValid(params) {
		return nil, fmt.Errorf("invalid request: %w", ErrCatalogUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if params == nil {
		params = map[string]string{}
	}
	rctx, cancel := context.WithTimeout(ctx, p.catalogTimeout)
	defer cancel()
	raw, err := p.call(rctx, "api", path, params)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case rctx.Err() != nil:
		return nil, fmt.Errorf("timed out: %w", ErrCatalogUnavailable)
	case errors.Is(err, ErrBrowserGone):
		return nil, fmt.Errorf("%w: %w", ErrCatalogUnavailable, ErrBrowserGone)
	default:
		// The page's error is left out: it is page text.
		return nil, ErrCatalogUnavailable
	}
	var r struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, fmt.Errorf("malformed reply: %w", ErrCatalogUnavailable)
	}
	switch r.Status {
	case 200:
	case 401, 403:
		return nil, fmt.Errorf("status %d: %w", r.Status, ErrCatalogUnauthorized)
	case 404:
		return nil, ErrCatalogNotFound
	default:
		return nil, fmt.Errorf("status %d: %w", r.Status, ErrCatalogUnavailable)
	}
	if len(r.Body) == 0 {
		return nil, fmt.Errorf("malformed reply: %w", ErrCatalogUnavailable)
	}
	return r.Body, nil
}

// fromCatalog answers a catalog read. Close ends the read with
// ErrClosed. Catalog reads do not wait for the player's commands.
func fromCatalog[T any](ctx context.Context, p *Player, read func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := p.check(ctx); err != nil {
		return zero, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	v, err := read(ctx)
	if err != nil {
		if p.ctx.Err() != nil {
			return zero, ErrClosed
		}
		return zero, err
	}
	return v, nil
}

// search runs a mixed catalog search, as Apple Music shows it: term
// suggestions, the top results across kinds, then artists, albums, songs
// and playlists. limit is clamped to 1...25 per result type; suggestions
// stop at 10 and top results at 6. Suggestions are best effort: when
// their request fails the search still succeeds without them.
func (c *catalog) search(ctx context.Context, term string, limit int) (playback.SearchResults, error) {
	if strings.TrimSpace(term) == "" {
		return playback.SearchResults{}, fmt.Errorf("catalog search: %w", ErrEmptyTerm)
	}
	limit = min(max(limit, 1), maxSearchLimit)
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	suggestions := make(chan []string, 1)
	go func() { suggestions <- c.suggestions(sctx, term, min(limit, maxSuggestions)) }()

	var doc apiSearch
	err := c.get(ctx, "search", catalogPath("search"), map[string]string{
		"term":  term,
		"types": "songs,albums,artists,playlists",
		"limit": strconv.Itoa(limit),
		"with":  "topResults",
	}, &doc)
	if err != nil {
		cancel()
		<-suggestions
		return playback.SearchResults{}, err
	}
	res := playback.SearchResults{
		Artists:   convert(doc.Results["artists"].Data, "artists", catalogID, toArtist),
		Albums:    convert(doc.Results["albums"].Data, "albums", catalogID, toAlbum),
		Songs:     convert(doc.Results["songs"].Data, "songs", catalogID, toSong),
		Playlists: convert(doc.Results["playlists"].Data, "playlists", playlistIDValid, toPlaylist),
	}
	for _, r := range doc.Results["top"].Data {
		if len(res.Top) == min(limit, maxTopResults) {
			break
		}
		if item, ok := topResult(r); ok {
			res.Top = append(res.Top, item)
		}
	}
	res.Suggestions = <-suggestions
	return res, nil
}

// suggestions are the search terms suggested for term; none when their
// request fails.
func (c *catalog) suggestions(ctx context.Context, term string, limit int) []string {
	var doc apiSuggestions
	err := c.get(ctx, "search suggestions", catalogPath("search", "suggestions"), map[string]string{
		"term":  term,
		"kinds": "terms",
		"limit": strconv.Itoa(limit),
	}, &doc)
	if err != nil {
		return nil
	}
	var terms []string
	for _, s := range doc.Results.Suggestions {
		if s.Kind == "terms" && s.SearchTerm != "" && len(terms) < limit {
			terms = append(terms, s.SearchTerm)
		}
	}
	return terms
}

// topResult converts a top result; ok is false for the kinds the UI
// cannot open (stations, music videos, curators and any added later).
func topResult(r apiResource) (item playback.SearchItem, ok bool) {
	switch {
	case r.Type == "artists" && catalogID(r.ID):
		return playback.SearchItem{Kind: playback.ItemArtist, Artist: toArtist(r)}, true
	case r.Type == "albums" && catalogID(r.ID):
		return playback.SearchItem{Kind: playback.ItemAlbum, Album: toAlbum(r)}, true
	case r.Type == "songs" && catalogID(r.ID):
		return playback.SearchItem{Kind: playback.ItemSong, Song: toSong(r)}, true
	case r.Type == "playlists" && playlistIDValid(r.ID):
		return playback.SearchItem{Kind: playback.ItemPlaylist, Playlist: toPlaylist(r)}, true
	}
	return playback.SearchItem{}, false
}

// artist loads an artist page, as Apple Music lays it out. Only the
// artist lookup (with its top songs and albums) can fail the call; each
// other section is its own concurrent request, so one that fails leaves
// just that section empty. "Featured albums" stand in for the essential
// ones.
func (c *catalog) artist(ctx context.Context, artistID string) (playback.ArtistDetail, error) {
	if !catalogID(artistID) {
		return playback.ArtistDetail{}, invalidID("artist")
	}
	op := "artist " + artistID
	var doc apiDocument
	if err := c.get(ctx, op, catalogPath("artists", artistID), map[string]string{"views": "top-songs,full-albums"}, &doc); err != nil {
		return playback.ArtistDetail{}, err
	}
	a, ok := doc.first("artists")
	if !ok {
		return playback.ArtistDetail{}, notFound(op)
	}
	d := playback.ArtistDetail{
		Artist:   toArtist(a),
		TopSongs: convert(a.Views["top-songs"].Data, "songs", catalogID, toSong),
		Albums:   convert(a.Views["full-albums"].Data, "albums", catalogID, toAlbum),
		About: playback.ArtistAbout{
			Notes: a.Attributes.EditorialNotes.text(),
			Genre: firstOf(a.Attributes.GenreNames),
		},
	}
	var wg sync.WaitGroup
	wg.Go(func() { d.EssentialAlbums = c.artistAlbums(ctx, op, artistID, "featured-albums") })
	wg.Go(func() { d.Singles = c.artistAlbums(ctx, op, artistID, "singles") })
	wg.Go(func() { d.Compilations = c.artistAlbums(ctx, op, artistID, "compilation-albums") })
	wg.Go(func() { d.Playlists = c.artistPlaylists(ctx, op, artistID) })
	wg.Go(func() { d.About.Origin, d.About.Formed = c.artistFacts(ctx, op, artistID) })
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return playback.ArtistDetail{}, err
	}
	return d, nil
}

// artistView reads one view of an artist; nothing when it fails.
func (c *catalog) artistView(ctx context.Context, op, artistID, view string) []apiResource {
	var doc apiDocument
	if err := c.get(ctx, op, catalogPath("artists", artistID, "view", view), nil, &doc); err != nil {
		return nil
	}
	return doc.Data
}

func (c *catalog) artistAlbums(ctx context.Context, op, artistID, view string) []playback.Album {
	return convert(c.artistView(ctx, op, artistID, view), "albums", catalogID, toAlbum)
}

// artistPlaylists are the artist's playlists, falling back to its
// featured playlists only when it has none.
func (c *catalog) artistPlaylists(ctx context.Context, op, artistID string) []playback.CatalogPlaylist {
	var doc apiDocument
	if err := c.get(ctx, op, catalogPath("artists", artistID, "playlists"), nil, &doc); err == nil {
		if own := convert(doc.Data, "playlists", playlistIDValid, toPlaylist); len(own) > 0 {
			return own
		}
	}
	return convert(c.artistView(ctx, op, artistID, "featured-playlists"), "playlists", playlistIDValid, toPlaylist)
}

// artistFacts are where the artist is from and when it was born or
// formed (a full date shortened to its year); empty when they fail.
func (c *catalog) artistFacts(ctx context.Context, op, artistID string) (origin, formed string) {
	var doc apiDocument
	if err := c.get(ctx, op, catalogPath("artists", artistID), map[string]string{"extend": "origin,bornOrFormed"}, &doc); err != nil {
		return "", ""
	}
	a, ok := doc.first("artists")
	if !ok {
		return "", ""
	}
	formed = a.Attributes.BornOrFormed
	if isoDate(formed) != "" {
		formed = formed[:4]
	}
	return a.Attributes.Origin, formed
}

// album loads an album page: the album with its tracks (songs only;
// music videos are left out) and the facts listed under them.
func (c *catalog) album(ctx context.Context, albumID string) (playback.AlbumDetail, error) {
	if !catalogID(albumID) {
		return playback.AlbumDetail{}, invalidID("album")
	}
	op := "album " + albumID
	var doc apiDocument
	if err := c.get(ctx, op, catalogPath("albums", albumID), nil, &doc); err != nil {
		return playback.AlbumDetail{}, err
	}
	a, ok := doc.first("albums")
	if !ok {
		return playback.AlbumDetail{}, notFound(op)
	}
	tracks, err := c.tracks(ctx, op, a.Relationships["tracks"])
	if err != nil {
		return playback.AlbumDetail{}, err
	}
	return playback.AlbumDetail{
		Album: toAlbum(a),
		Tracks: convert(tracks, "songs", catalogID, func(r apiResource) playback.Track {
			return playback.Track{Song: toSong(r), Number: r.Attributes.TrackNumber, Disc: r.Attributes.DiscNumber}
		}),
		Genre:       firstOf(a.Attributes.GenreNames),
		ReleaseDate: isoDate(a.Attributes.ReleaseDate),
		RecordLabel: a.Attributes.RecordLabel,
		Copyright:   a.Attributes.Copyright,
		Notes:       a.Attributes.EditorialNotes.text(),
	}, nil
}

// songAlbum loads the page of the song's first album: two sequential
// lookups.
func (c *catalog) songAlbum(ctx context.Context, songID string) (playback.AlbumDetail, error) {
	if !catalogID(songID) {
		return playback.AlbumDetail{}, invalidID("song")
	}
	op := "song " + songID
	var doc apiDocument
	if err := c.get(ctx, op, catalogPath("songs", songID), map[string]string{"include": "albums"}, &doc); err != nil {
		return playback.AlbumDetail{}, err
	}
	s, ok := doc.first("songs")
	if !ok {
		return playback.AlbumDetail{}, notFound(op)
	}
	for _, a := range s.Relationships["albums"].Data {
		if a.Type == "albums" && catalogID(a.ID) {
			return c.album(ctx, a.ID)
		}
	}
	return playback.AlbumDetail{}, fmt.Errorf("catalog %s: no album: %w", op, ErrCatalogNotFound)
}

// playlist loads a catalog playlist page: its songs in order (music
// videos are left out) and its description.
func (c *catalog) playlist(ctx context.Context, playlistID string) (playback.PlaylistDetail, error) {
	if !playlistIDValid(playlistID) {
		return playback.PlaylistDetail{}, invalidID("playlist")
	}
	op := "playlist " + playlistID
	var doc apiDocument
	if err := c.get(ctx, op, catalogPath("playlists", playlistID), map[string]string{"include": "tracks"}, &doc); err != nil {
		return playback.PlaylistDetail{}, err
	}
	p, ok := doc.first("playlists")
	if !ok {
		return playback.PlaylistDetail{}, notFound(op)
	}
	tracks, err := c.tracks(ctx, op, p.Relationships["tracks"])
	if err != nil {
		return playback.PlaylistDetail{}, err
	}
	return playback.PlaylistDetail{
		Playlist: toPlaylist(p),
		Tracks:   convert(tracks, "songs", catalogID, toSong),
		Notes:    p.Attributes.Description.text(),
	}, nil
}

// tracks reads a track list, following its next pages up to
// maxTrackPages in all. A page that fails fails the list.
func (c *catalog) tracks(ctx context.Context, op string, first apiDocument) ([]apiResource, error) {
	items, next := first.Data, first.Next
	for page := 1; next != "" && page < maxTrackPages; page++ {
		p, params, ok := nextPage(next)
		if !ok {
			return nil, fmt.Errorf("catalog %s: malformed next page: %w", op, ErrCatalogUnavailable)
		}
		var doc apiDocument
		if err := c.get(ctx, op, p, params, &doc); err != nil {
			return nil, err
		}
		items = append(items, doc.Data...)
		next = doc.Next
	}
	return items, nil
}

// nextPage converts a "next" link, an absolute path within a
// storefront's catalog such as /v1/catalog/us/albums/1/tracks?offset=10,
// to the same path under the storefront placeholder and its query as
// params. Anything else (a host, another API, a climb out, an unusual
// segment, a repeated or odd parameter) is refused, so the page is never
// asked for anything but the catalog.
func nextPage(next string) (p string, params map[string]string, ok bool) {
	link, params, ok := splitNext(next)
	if !ok {
		return "", nil, false
	}
	rest, found := strings.CutPrefix(link, catalogPrefix)
	if !found {
		return "", nil, false
	}
	storefront, rest, found := strings.Cut(rest, "/")
	if !found || !storefrontValid(storefront) {
		return "", nil, false
	}
	segments := strings.Split(rest, "/")
	for _, segment := range segments {
		if !pathSegmentValid(segment) {
			return "", nil, false
		}
	}
	return catalogPath(segments...), params, true
}

// splitNext splits a "next" link into its path, which must be absolute,
// clean and unescaped, without a host or a fragment, and its query as
// flat params of plain names (each given once).
func splitNext(next string) (p string, params map[string]string, ok bool) {
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.RawPath != "" {
		return "", nil, false
	}
	if !strings.HasPrefix(u.Path, "/") || path.Clean(u.Path) != u.Path {
		return "", nil, false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", nil, false
	}
	params = make(map[string]string, len(query))
	for k, vs := range query {
		if len(vs) != 1 || !paramKeyValid(k) {
			return "", nil, false
		}
		params[k] = vs[0]
	}
	return u.Path, params, true
}

// catalogPath is the catalog path of the parts, which are validated ids
// or fixed names, under the storefront placeholder.
func catalogPath(parts ...string) string {
	return catalogPrefix + storefrontPlaceholder + "/" + strings.Join(parts, "/")
}

// catalogPathValid reports whether p is a catalog path (see catalogPath)
// of id-safe segments.
func catalogPathValid(p string) bool {
	rest, ok := strings.CutPrefix(p, catalogPrefix+storefrontPlaceholder+"/")
	if !ok {
		return false
	}
	for _, segment := range strings.Split(rest, "/") {
		if !pathSegmentValid(segment) {
			return false
		}
	}
	return true
}

// paramsValid reports whether every parameter name is plain (see
// paramKeyValid); the values are any strings.
func paramsValid(params map[string]string) bool {
	for k := range params {
		if !paramKeyValid(k) {
			return false
		}
	}
	return true
}

// storefrontValid reports whether s is two lowercase ASCII letters.
func storefrontValid(s string) bool {
	return len(s) == 2 && lowerASCII(s[0]) && lowerASCII(s[1])
}

func lowerASCII(b byte) bool { return b >= 'a' && b <= 'z' }

// pathSegmentValid reports whether s is ASCII letters, digits, dots and
// hyphens, and neither "." nor "..".
func pathSegmentValid(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, b := range []byte(s) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.' || b == '-') {
			return false
		}
	}
	return true
}

// paramKeyValid reports whether k is a parameter name Apple uses: ASCII
// letters, digits, dots, hyphens, underscores and brackets (as
// "fields[albums]").
func paramKeyValid(k string) bool {
	if k == "" || len(k) > paramKeyLimit {
		return false
	}
	for _, b := range []byte(k) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.' || b == '-' || b == '_' || b == '[' || b == ']') {
			return false
		}
	}
	return true
}

// get reads one catalog resource into out. Its errors name op and wrap
// a sentinel; they never carry the reply.
func (c *catalog) get(ctx context.Context, op, p string, params map[string]string, out any) error {
	return c.read(ctx, "catalog "+op, p, params, out)
}

// read reads one resource into out; its errors start with what.
func (c *catalog) read(ctx context.Context, what, p string, params map[string]string, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := c.fetch(ctx, p, params)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	// Unknown fields are ignored: Apple adds attributes often.
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: malformed reply: %w", what, ErrCatalogUnavailable)
	}
	return nil
}

func invalidID(kind string) error {
	return fmt.Errorf("catalog %s: %w", kind, ErrInvalidCatalogID)
}

func notFound(op string) error {
	return fmt.Errorf("catalog %s: %w", op, ErrCatalogNotFound)
}

// playlistIDValid reports whether id is a catalog playlist id: "pl." and
// up to 64 ASCII letters, digits and hyphens (as "pl.u-…").
func playlistIDValid(id string) bool {
	name, ok := strings.CutPrefix(id, "pl.")
	if !ok || name == "" || len(name) > playlistIDLimit {
		return false
	}
	for _, b := range []byte(name) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-') {
			return false
		}
	}
	return true
}

// The Apple Music API's JSON, as far as it is read.

type apiNotes struct {
	Standard string `json:"standard"`
	Short    string `json:"short"`
}

// text is the standard notes, or the short ones, as plain text.
func (n *apiNotes) text() string {
	if n == nil {
		return ""
	}
	if n.Standard != "" {
		return plainText(n.Standard)
	}
	return plainText(n.Short)
}

type apiAttributes struct {
	Name             string    `json:"name"`
	ArtistName       string    `json:"artistName"`
	AlbumName        string    `json:"albumName"`
	CuratorName      string    `json:"curatorName"`
	DurationInMillis float64   `json:"durationInMillis"`
	TrackNumber      int       `json:"trackNumber"`
	DiscNumber       int       `json:"discNumber"`
	TrackCount       int       `json:"trackCount"`
	ReleaseDate      string    `json:"releaseDate"`
	GenreNames       []string  `json:"genreNames"`
	RecordLabel      string    `json:"recordLabel"`
	Copyright        string    `json:"copyright"`
	EditorialNotes   *apiNotes `json:"editorialNotes"`
	Description      *apiNotes `json:"description"`
	Origin           string    `json:"origin"`
	BornOrFormed     string    `json:"bornOrFormed"`
}

type apiResource struct {
	ID            string                 `json:"id"`
	Type          string                 `json:"type"`
	Attributes    apiAttributes          `json:"attributes"`
	Relationships map[string]apiDocument `json:"relationships"`
	Views         map[string]apiDocument `json:"views"`
}

// apiDocument is a response, a relationship or a view: resources and the
// path of their next page, if any.
type apiDocument struct {
	Next string        `json:"next"`
	Data []apiResource `json:"data"`
}

// first is the first resource, when it has the type.
func (d apiDocument) first(typ string) (apiResource, bool) {
	if len(d.Data) == 0 || d.Data[0].Type != typ {
		return apiResource{}, false
	}
	return d.Data[0], true
}

type apiSearch struct {
	Results map[string]apiDocument `json:"results"`
}

type apiSuggestions struct {
	Results struct {
		Suggestions []struct {
			Kind       string `json:"kind"`
			SearchTerm string `json:"searchTerm"`
		} `json:"suggestions"`
	} `json:"results"`
}

// convert converts the resources of the type with a valid id, in order;
// nil when there are none.
func convert[T any](rs []apiResource, typ string, valid func(string) bool, to func(apiResource) T) []T {
	var out []T
	for _, r := range rs {
		if r.Type == typ && valid(r.ID) {
			out = append(out, to(r))
		}
	}
	return out
}

func toSong(r apiResource) playback.Song {
	return playback.Song{
		ID:       r.ID,
		Title:    r.Attributes.Name,
		Artist:   r.Attributes.ArtistName,
		Album:    r.Attributes.AlbumName,
		Duration: time.Duration(math.Round(r.Attributes.DurationInMillis * float64(time.Millisecond))),
	}
}

func toArtist(r apiResource) playback.Artist {
	return playback.Artist{ID: r.ID, Name: r.Attributes.Name, Genres: r.Attributes.GenreNames}
}

func toAlbum(r apiResource) playback.Album {
	return playback.Album{
		ID:         r.ID,
		Title:      r.Attributes.Name,
		Artist:     r.Attributes.ArtistName,
		Year:       year(r.Attributes.ReleaseDate),
		TrackCount: r.Attributes.TrackCount,
	}
}

func toPlaylist(r apiResource) playback.CatalogPlaylist {
	return playback.CatalogPlaylist{ID: r.ID, Name: r.Attributes.Name, Curator: r.Attributes.CuratorName}
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// year is the year of a catalog date ("2006", "2006-01" or "2006-01-02");
// 0 when there is none.
func year(date string) int {
	if len(date) < 4 || len(date) > 4 && date[4] != '-' {
		return 0
	}
	y, err := strconv.Atoi(date[:4])
	if err != nil || y <= 0 {
		return 0
	}
	return y
}

// isoDate is date when it is a full calendar date ("2006-01-02"), else
// empty.
func isoDate(date string) string {
	if len(date) != len(time.DateOnly) {
		return ""
	}
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return ""
	}
	return date
}

var (
	htmlBreak  = regexp.MustCompile(`(?i)<br\s*/?>|</p\s*>|</div\s*>`)
	htmlTag    = regexp.MustCompile(`</?[A-Za-z!][^>]*>`)
	htmlEntity = regexp.MustCompile(`&(#[0-9]+|#[xX][0-9A-Fa-f]+|[A-Za-z]+);`)
)

var namedEntities = map[string]string{
	"amp": "&", "lt": "<", "gt": ">", "quot": "\"", "apos": "'", "nbsp": " ",
	"mdash": "—", "ndash": "–", "hellip": "…", "rsquo": "’", "lsquo": "‘",
	"rdquo": "”", "ldquo": "“",
}

// plainText turns editorial notes, which are HTML fragments, into plain
// text for the terminal, as the macOS helper does: line breaks and
// paragraph ends become newlines, other tags are dropped, entities are
// decoded (unknown ones are kept as written), control characters other
// than newlines are removed (control whitespace counts as a space), and
// runs of whitespace collapse to single spaces with blank lines removed.
func plainText(html string) string {
	text := htmlBreak.ReplaceAllString(html, "\n")
	text = htmlTag.ReplaceAllString(text, "")
	text = htmlEntity.ReplaceAllStringFunc(text, func(m string) string {
		if s, ok := decodeEntity(m[1 : len(m)-1]); ok {
			return s
		}
		return m
	})
	text = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || !unicode.IsControl(r):
			return r
		case unicode.IsSpace(r):
			return ' '
		}
		return -1
	}, text)
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// decodeEntity decodes one entity body ("amp", "#39", "#x2014").
func decodeEntity(entity string) (string, bool) {
	digits, numeric := strings.CutPrefix(entity, "#")
	if !numeric {
		s, ok := namedEntities[entity]
		return s, ok
	}
	base := 10
	if hex, ok := strings.CutPrefix(digits, "x"); ok {
		digits, base = hex, 16
	} else if hex, ok := strings.CutPrefix(digits, "X"); ok {
		digits, base = hex, 16
	}
	v, err := strconv.ParseUint(digits, base, 32)
	if err != nil || !utf8.ValidRune(rune(v)) {
		return "", false
	}
	return string(rune(v)), true
}
