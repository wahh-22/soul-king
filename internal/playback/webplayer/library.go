package webplayer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wahh-22/nu11signal/internal/playback"
)

// The library, read through the page's MusicKit API client as the macOS
// helper reads it (its LibraryRead and LibraryEdit): the Apple Music API
// with the signed-in user's token, which MusicKit adds. Only these reads
// reach the page (see libraryPathValid), as documented at
// https://developer.apple.com/documentation/applemusicapi:
//
//	Get All Library Playlists                GET /v1/me/library/playlists
//	Get a Library Playlist                   GET /v1/me/library/playlists/{id}
//	Get a Library Playlist's tracks          GET /v1/me/library/playlists/{id}/tracks
//	Get Multiple Personal Song Ratings       GET /v1/me/ratings/songs?ids=a,b
//	Get Multiple Personal Library Song Ratings
//	                                         GET /v1/me/ratings/library-songs?ids=a,b
//
// Collections come in pages; a page's next is its own path with an
// offset.
const (
	libraryPlaylistsPath = "/v1/me/library/playlists"
	ratingsPrefix        = "/v1/me/ratings/"
	// libraryPageLimit is the most resources the API returns per page.
	libraryPageLimit = 100
	// maxLibraryPlaylists bounds the playlists Playlists lists.
	maxLibraryPlaylists = 500
	// maxLibraryTracks bounds the songs read from one library playlist.
	maxLibraryTracks = 1000
	// maxRatingIDs is the most ids one ratings read asks for.
	maxRatingIDs = 100
	// maxRatedCatalogIDDigits is the longest id read as a catalog song id
	// by a ratings read, as on macOS.
	maxRatedCatalogIDDigits = 12
)

var (
	// ErrInvalidLibraryID is returned, before anything reaches the page,
	// for a playlist id that is not an Apple Music API library id ("p."
	// and a name).
	ErrInvalidLibraryID = errors.New("webplayer: invalid apple music library id")
	// ErrNothingToPlay is a library playlist without a song in the Apple
	// Music catalog: the web player plays catalog songs only.
	ErrNothingToPlay = errors.New("webplayer: the playlist has no songs in the apple music catalog")
)

// libraryPathValid reports whether p is one of the library reads: the
// playlists, a playlist ("p.…") or its tracks, or the ratings of
// catalog or library songs.
func libraryPathValid(p string) bool {
	switch p {
	case libraryPlaylistsPath, ratingsPrefix + "songs", ratingsPrefix + "library-songs":
		return true
	}
	rest, ok := strings.CutPrefix(p, libraryPlaylistsPath+"/")
	if !ok {
		return false
	}
	id, sub, nested := strings.Cut(rest, "/")
	return libraryPlaylistIDValid(id) && (!nested || sub == "tracks")
}

// libraryPlaylistIDValid reports whether id is a library playlist id:
// "p." and up to 64 ASCII letters, digits and dots, without "..".
func libraryPlaylistIDValid(id string) bool {
	name, ok := strings.CutPrefix(id, "p.")
	return ok && name != "" && len(name) <= playlistIDLimit && apiIDSafe(id)
}

// apiIDSafe reports whether id is safe as one API path segment, as the
// helper's APIPathID checks: ASCII letters, digits and dots, neither "."
// nor containing "..".
func apiIDSafe(id string) bool {
	if id == "" || id == "." || strings.Contains(id, "..") {
		return false
	}
	for _, b := range []byte(id) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.') {
			return false
		}
	}
	return true
}

// libraryNextPage converts the next link of a page of current: it must
// be current's own path (a page can never send the reads to another
// resource), and its query becomes the params.
func libraryNextPage(next, current string) (map[string]string, bool) {
	p, params, ok := splitNext(next)
	if !ok || p != current || !libraryPathValid(p) {
		return nil, false
	}
	return params, true
}

// The library API's JSON, as far as it is read.

type libraryAttributes struct {
	apiAttributes
	CanEdit    bool `json:"canEdit"`
	PlayParams struct {
		// CatalogID is kept raw: a value that is not a string is no
		// catalog id, rather than a malformed page.
		CatalogID json.RawMessage `json:"catalogId"`
	} `json:"playParams"`
}

type libraryResource struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Attributes libraryAttributes `json:"attributes"`
}

type libraryDocument struct {
	Next string            `json:"next"`
	Data []libraryResource `json:"data"`
}

type ratingsDocument struct {
	Data []struct {
		ID         string `json:"id"`
		Attributes struct {
			Value float64 `json:"value"`
		} `json:"attributes"`
	} `json:"data"`
}

// libraryGet reads one library resource into out. A document without a
// data array is malformed, as on macOS.
func (c *catalog) libraryGet(ctx context.Context, op, p string, params map[string]string, out *libraryDocument) error {
	if err := c.read(ctx, "library "+op, p, params, out); err != nil {
		return err
	}
	if out.Data == nil {
		return fmt.Errorf("library %s: malformed reply: %w", op, ErrCatalogUnavailable)
	}
	return nil
}

// collect reads the collection at p and its next pages, converting their
// resources in order, up to limit items. It stops at the last page, at
// the limit (no page is read past it), at a page sent empty, or at a next
// whose offset was already read, so a next pointing back can neither
// loop nor list a resource twice.
func collect[T any](ctx context.Context, c *catalog, op, p string, limit int, to func(libraryResource) (T, bool)) ([]T, error) {
	var items []T
	read := map[int]bool{}
	params := map[string]string{"limit": strconv.Itoa(libraryPageLimit)}
	for len(items) < limit {
		offset, _ := strconv.Atoi(params["offset"])
		if read[offset] {
			break
		}
		read[offset] = true
		var doc libraryDocument
		if err := c.libraryGet(ctx, op, p, params, &doc); err != nil {
			return nil, err
		}
		for _, r := range doc.Data {
			if item, ok := to(r); ok {
				items = append(items, item)
			}
		}
		if len(doc.Data) == 0 || doc.Next == "" {
			break
		}
		next, ok := libraryNextPage(doc.Next, p)
		if !ok {
			return nil, fmt.Errorf("library %s: malformed next page: %w", op, ErrCatalogUnavailable)
		}
		params = next
	}
	return items[:min(len(items), limit)], nil
}

// playlists lists the library playlists in the API's order, which is
// alphabetical, up to maxLibraryPlaylists. Items without a usable id are
// left out.
func (c *catalog) playlists(ctx context.Context) ([]playback.Playlist, error) {
	lists, err := collect(ctx, c, "playlists", libraryPlaylistsPath, maxLibraryPlaylists, func(r libraryResource) (playback.Playlist, bool) {
		if !libraryPlaylistIDValid(r.ID) {
			return playback.Playlist{}, false
		}
		return playback.Playlist{ID: r.ID, Name: r.Attributes.Name, Editable: r.Attributes.CanEdit}, true
	})
	if err != nil {
		return nil, err
	}
	if lists == nil {
		lists = []playback.Playlist{}
	}
	return lists, nil
}

// libraryTracks reads the songs of a library playlist, in order, up to
// maxLibraryTracks (see librarySong). LibraryPlaylist and the plays share
// it, so a start index means the same song.
func (c *catalog) libraryTracks(ctx context.Context, playlistID string) ([]playback.Song, error) {
	if !libraryPlaylistIDValid(playlistID) {
		return nil, fmt.Errorf("library playlist: %w", ErrInvalidLibraryID)
	}
	return collect(ctx, c, "playlist "+playlistID, libraryPlaylistsPath+"/"+playlistID+"/tracks", maxLibraryTracks, librarySong)
}

// librarySong converts a song of a library playlist, catalog or library
// one; music videos, and songs without a usable id, are left out. Its id
// is the catalog id (playParams.catalogId for a library song) when it has
// one; otherwise it keeps its library id and is LibraryOnly.
func librarySong(r libraryResource) (playback.Song, bool) {
	if !apiIDSafe(r.ID) {
		return playback.Song{}, false
	}
	var catalog string
	switch r.Type {
	case "songs":
		catalog = r.ID
	case "library-songs":
		_ = json.Unmarshal(r.Attributes.PlayParams.CatalogID, &catalog)
	default:
		return playback.Song{}, false
	}
	a := r.Attributes
	s := playback.Song{ID: catalog, Title: a.Name, Artist: a.ArtistName, Album: a.AlbumName}
	if !catalogID(catalog) {
		s.ID, s.LibraryOnly = r.ID, true
	}
	if ms := a.DurationInMillis; ms > 0 && !math.IsInf(ms, 0) {
		s.Duration = time.Duration(math.Round(ms * float64(time.Millisecond)))
	}
	return s, true
}

// libraryPlaylist loads a library playlist page: its songs in order and
// its description. The playlist and its songs are read concurrently; the
// playlist failing ends the songs' read.
func (c *catalog) libraryPlaylist(ctx context.Context, playlistID string) (playback.PlaylistDetail, error) {
	if !libraryPlaylistIDValid(playlistID) {
		return playback.PlaylistDetail{}, fmt.Errorf("library playlist: %w", ErrInvalidLibraryID)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var tracks []playback.Song
	var tracksErr error
	var wg sync.WaitGroup
	wg.Go(func() { tracks, tracksErr = c.libraryTracks(ctx, playlistID) })
	op := "playlist " + playlistID
	var doc libraryDocument
	err := c.libraryGet(ctx, op, libraryPlaylistsPath+"/"+playlistID, nil, &doc)
	if err == nil && len(doc.Data) == 0 {
		err = notFoundIn("library", op)
	}
	if err != nil {
		cancel()
		wg.Wait()
		return playback.PlaylistDetail{}, err
	}
	wg.Wait()
	if tracksErr != nil {
		return playback.PlaylistDetail{}, tracksErr
	}
	a := doc.Data[0].Attributes
	return playback.PlaylistDetail{
		Playlist: playback.CatalogPlaylist{ID: playlistID, Name: a.Name},
		Tracks:   tracks,
		Notes:    a.Description.text(),
	}, nil
}

func notFoundIn(scope, op string) error {
	return fmt.Errorf("%s %s: %w", scope, op, ErrCatalogNotFound)
}

// libraryQueue is what a library playlist plays, as the helper's
// LibraryQueuePlan: the catalog ids of its songs, in order, and the
// position in them of the song at start (with from) or of the first one.
// LibraryOnly songs are skipped; starting at one is an error. Errors
// never name a song: titles are Apple's text.
func libraryQueue(tracks []playback.Song, start int, from bool) (ids []string, at int, err error) {
	if len(tracks) == 0 {
		return nil, 0, fmt.Errorf("no songs: %w", ErrNothingToPlay)
	}
	if from {
		if start < 0 || start >= len(tracks) {
			return nil, 0, fmt.Errorf("start %d out of range: the playlist has %d songs: %w", start, len(tracks), ErrInvalidArgument)
		}
		if tracks[start].LibraryOnly {
			return nil, 0, fmt.Errorf("start %d is not in the apple music catalog: %w", start, ErrInvalidArgument)
		}
	}
	for i, s := range tracks {
		if s.LibraryOnly {
			continue
		}
		if from && i < start {
			at++
		}
		ids = append(ids, s.ID)
	}
	if len(ids) == 0 {
		return nil, 0, ErrNothingToPlay
	}
	return ids, at, nil
}

// ratedType is the ratings resource of a song id, as the helper's
// songType: "library-songs" for a library id ("i.…"), "songs" for a
// catalog id of up to 12 digits; ok is false for anything else.
func ratedType(id string) (string, bool) {
	if !apiIDSafe(id) {
		return "", false
	}
	if strings.HasPrefix(id, "i.") && len(id) > 2 {
		return "library-songs", true
	}
	if len(id) <= maxRatedCatalogIDDigits && catalogID(id) {
		return "songs", true
	}
	return "", false
}

// favorites reports, for each id, whether the song is loved (rating 1):
// one ratings read per kind of id and batch of maxRatingIDs, without
// repeats, run concurrently. A read answered 404 rated none of its songs;
// any other failure, or an id of neither kind, fails the call.
func (c *catalog) favorites(ctx context.Context, ids []string) (map[string]bool, error) {
	var types []string
	byType := map[string][]string{}
	seen := map[string]bool{}
	for _, id := range ids {
		typ, ok := ratedType(id)
		if !ok {
			return nil, fmt.Errorf("library favorites: not a song id: %w", ErrInvalidArgument)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := byType[typ]; !ok {
			types = append(types, typ)
		}
		byType[typ] = append(byType[typ], id)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		loved    = map[string]bool{}
		firstErr error
		wg       sync.WaitGroup
	)
	for _, typ := range types {
		group := byType[typ]
		for start := 0; start < len(group); start += maxRatingIDs {
			batch := group[start:min(start+maxRatingIDs, len(group))]
			wg.Go(func() {
				rated, err := c.loved(ctx, typ, batch)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					return
				}
				for _, id := range rated {
					loved[id] = true
				}
			})
		}
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	answer := make(map[string]bool, len(ids))
	for _, id := range ids {
		answer[id] = loved[id]
	}
	return answer, nil
}

// loved reads the ratings of one batch of songs of typ and returns the
// loved ones.
func (c *catalog) loved(ctx context.Context, typ string, ids []string) ([]string, error) {
	var doc ratingsDocument
	err := c.read(ctx, "library favorites", ratingsPrefix+typ, map[string]string{"ids": strings.Join(ids, ",")}, &doc)
	switch {
	case errors.Is(err, ErrCatalogNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	case doc.Data == nil:
		// Read as none rated, it would report every song not loved.
		return nil, fmt.Errorf("library favorites: malformed reply: %w", ErrCatalogUnavailable)
	}
	var out []string
	for _, r := range doc.Data {
		if r.Attributes.Value == 1 {
			out = append(out, r.ID)
		}
	}
	return out, nil
}

// The library edits, sent as the helper's LibraryEdit sends them, and
// only these (see writeValid; the page checks them too):
//
//	Create a New Library Playlist            POST   /v1/me/library/playlists
//	Add Tracks to a Library Playlist         POST   /v1/me/library/playlists/{id}/tracks
//	Add a Personal (Library) Song Rating     PUT    /v1/me/ratings/{songs|library-songs}/{id}
//	Delete a Personal (Library) Song Rating  DELETE /v1/me/ratings/{songs|library-songs}/{id}
//
// Bodies are JSON with sorted keys, as the helper sends them.

var (
	// ErrPlaylistNotEditable is Apple refusing to add songs to a
	// playlist (403): one the user follows rather than owns.
	ErrPlaylistNotEditable = errors.New("webplayer: the playlist is not editable")
	// ErrEditOutcomeUnknown is a playlist creation or addition that timed
	// out: it may still be applied, so it is never retried; check the
	// library before trying again. Its OutcomeUnknown method marks it as
	// the helper's own such errors are marked, for the UI.
	ErrEditOutcomeUnknown error = outcomeUnknown{}
)

type outcomeUnknown struct{}

func (outcomeUnknown) Error() string {
	return "webplayer: the library edit may or may not have been applied"
}

// OutcomeUnknown reports that the edit may still be applied.
func (outcomeUnknown) OutcomeUnknown() bool { return true }

// The edits' bodies. Fields are declared in key order, so they encode
// with sorted keys.
type (
	editTrack struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	editTracks struct {
		Data []editTrack `json:"data"`
	}
	newPlaylistAttributes struct {
		Description string `json:"description,omitempty"`
		Name        string `json:"name"`
	}
	newPlaylistTracks struct {
		Tracks editTracks `json:"tracks"`
	}
	newPlaylist struct {
		Attributes    newPlaylistAttributes `json:"attributes"`
		Relationships *newPlaylistTracks    `json:"relationships,omitempty"`
	}
	ratingValue struct {
		Value int `json:"value"`
	}
	songRating struct {
		Attributes ratingValue `json:"attributes"`
		Type       string      `json:"type"`
	}
)

// loveRating is the rating that loves a song: what Apple Music shows as
// Favorite.
var loveRating = songRating{Attributes: ratingValue{Value: 1}, Type: "rating"}

// editKind is what a write does, from its method and path; "" for any
// request that is not one of the library edits.
func editKind(method, p string) string {
	if p == libraryPlaylistsPath {
		if method == "POST" {
			return "create"
		}
		return ""
	}
	if rest, ok := strings.CutPrefix(p, libraryPlaylistsPath+"/"); ok {
		id, sub, _ := strings.Cut(rest, "/")
		if method == "POST" && sub == "tracks" && libraryPlaylistIDValid(id) {
			return "add"
		}
		return ""
	}
	rest, ok := strings.CutPrefix(p, ratingsPrefix)
	if !ok {
		return ""
	}
	typ, id, _ := strings.Cut(rest, "/")
	if rated, ok := ratedType(id); !ok || rated != typ {
		return ""
	}
	switch method {
	case "PUT":
		return "love"
	case "DELETE":
		return "unlove"
	}
	return ""
}

// writeValid reports whether method, p and body are one of the library
// edits: an allowed method and path, and exactly the body that edit
// sends (none for a DELETE), as editJSON encodes it, with valid song
// ids and a non-blank playlist name.
func writeValid(method, p string, body []byte) bool {
	kind := editKind(method, p)
	switch kind {
	case "":
		return false
	case "unlove":
		return body == nil
	case "love":
		var r songRating
		return canonical(body, &r) && r == loveRating
	case "add":
		var t editTracks
		return canonical(body, &t) && tracksValid(t)
	}
	var n newPlaylist
	return canonical(body, &n) && strings.TrimSpace(n.Attributes.Name) != "" &&
		(n.Relationships == nil || tracksValid(n.Relationships.Tracks))
}

// canonical decodes body into out and reports whether it is exactly what
// editJSON encodes for out: no unknown, repeated or differently cased
// key, nothing after the value.
func canonical(body []byte, out any) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(out) != nil || dec.More() {
		return false
	}
	again, err := editJSON(out)
	return err == nil && bytes.Equal(again, body)
}

// tracksValid reports whether t lists songs, each with the ratings type
// of its id (see ratedType).
func tracksValid(t editTracks) bool {
	if len(t.Data) == 0 {
		return false
	}
	for _, s := range t.Data {
		if typ, ok := ratedType(s.ID); !ok || typ != s.Type {
			return false
		}
	}
	return true
}

// editJSON encodes an edit's body as the helper does: sorted keys (the
// bodies' field order), without escaping HTML characters.
func editJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// songTracks are the songs of an edit, in order, as the helper's tracks:
// a catalog or library id each (see ratedType), repeats kept.
func songTracks(op string, ids []string) ([]editTrack, error) {
	tracks := make([]editTrack, 0, len(ids))
	for _, id := range ids {
		typ, ok := ratedType(id)
		if !ok {
			return nil, fmt.Errorf("library %s: not a song id: %w", op, ErrInvalidArgument)
		}
		tracks = append(tracks, editTrack{ID: id, Type: typ})
	}
	return tracks, nil
}

// write sends one library edit (see writeValid) through the page's own
// MusicKit API client (the bootstrap's write), bounded by the catalog
// timeout, and returns the reply's JSON, nil for none. An edit is never
// retried: a timed out creation or addition is ErrEditOutcomeUnknown.
// Statuses map as the helper's failureMessage reads them: 401 and 403
// are ErrCatalogUnauthorized (a 403 adding songs is
// ErrPlaylistNotEditable), 404 ErrCatalogNotFound and any other failure
// ErrCatalogUnavailable; the page's text never reaches the error.
func (p *Player) write(ctx context.Context, op, method, path string, body []byte) (json.RawMessage, error) {
	if !writeValid(method, path, body) {
		return nil, fmt.Errorf("library %s: invalid request: %w", op, ErrInvalidArgument)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var text *string
	if body != nil {
		s := string(body)
		text = &s
	}
	rctx, cancel := context.WithTimeout(ctx, p.catalogTimeout)
	defer cancel()
	raw, err := p.call(rctx, "write", method, path, text)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case rctx.Err() != nil && (op == "create playlist" || op == "add to playlist"):
		return nil, fmt.Errorf("library %s: timed out: %w: %w", op, ErrEditOutcomeUnknown, ErrCatalogUnavailable)
	case rctx.Err() != nil:
		return nil, fmt.Errorf("library %s: timed out: %w", op, ErrCatalogUnavailable)
	case errors.Is(err, ErrBrowserGone):
		return nil, fmt.Errorf("library %s: %w: %w", op, ErrCatalogUnavailable, ErrBrowserGone)
	default:
		// The page's error is left out: it is page text.
		return nil, fmt.Errorf("library %s: %w", op, ErrCatalogUnavailable)
	}
	var r struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil, fmt.Errorf("library %s: malformed reply: %w", op, ErrCatalogUnavailable)
	}
	switch {
	case r.Status >= 200 && r.Status <= 299:
	case r.Status == 403 && op == "add to playlist":
		return nil, fmt.Errorf("library %s: status 403: %w", op, ErrPlaylistNotEditable)
	case r.Status == 401 || r.Status == 403:
		return nil, fmt.Errorf("library %s: status %d: %w", op, r.Status, ErrCatalogUnauthorized)
	case r.Status == 404:
		return nil, notFoundIn("library", op)
	default:
		return nil, fmt.Errorf("library %s: status %d: %w", op, r.Status, ErrCatalogUnavailable)
	}
	if len(r.Body) == 0 || string(r.Body) == "null" {
		return nil, nil
	}
	return r.Body, nil
}

// setFavorite loves a song (a catalog or library id, see ratedType) or
// clears its rating, as the helper's setFavorite. Clearing the rating of
// a song without one is answered 404: it is then not a favorite, so that
// succeeds.
func (p *Player) setFavorite(ctx context.Context, songID string, on bool) error {
	typ, ok := ratedType(songID)
	if !ok {
		return fmt.Errorf("library favorite: not a song id: %w", ErrInvalidArgument)
	}
	path := ratingsPrefix + typ + "/" + songID
	if !on {
		_, err := p.write(ctx, "favorite", "DELETE", path, nil)
		if errors.Is(err, ErrCatalogNotFound) {
			return nil
		}
		return err
	}
	body, err := editJSON(loveRating)
	if err != nil {
		return err
	}
	_, err = p.write(ctx, "favorite", "PUT", path, body)
	return err
}

// createPlaylist creates a library playlist holding the songs, in order,
// as the helper's createPlaylist: a blank name is ErrInvalidArgument, an
// empty description or song list is left out. The reply's first resource
// is the new playlist; its name falls back to the requested one.
func (p *Player) createPlaylist(ctx context.Context, name, description string, songIDs []string) (playback.Playlist, error) {
	const op = "create playlist"
	if strings.TrimSpace(name) == "" {
		return playback.Playlist{}, fmt.Errorf("library %s: blank name: %w", op, ErrInvalidArgument)
	}
	tracks, err := songTracks(op, songIDs)
	if err != nil {
		return playback.Playlist{}, err
	}
	req := newPlaylist{Attributes: newPlaylistAttributes{Name: name, Description: description}}
	if len(tracks) > 0 {
		req.Relationships = &newPlaylistTracks{Tracks: editTracks{Data: tracks}}
	}
	body, err := editJSON(req)
	if err != nil {
		return playback.Playlist{}, err
	}
	reply, err := p.write(ctx, op, "POST", libraryPlaylistsPath, body)
	if err != nil {
		return playback.Playlist{}, err
	}
	var doc libraryDocument
	if json.Unmarshal(reply, &doc) != nil || len(doc.Data) == 0 || !apiIDSafe(doc.Data[0].ID) {
		return playback.Playlist{}, fmt.Errorf("library %s: the reply does not name the new playlist: %w", op, ErrCatalogUnavailable)
	}
	created := playback.Playlist{ID: doc.Data[0].ID, Name: doc.Data[0].Attributes.Name}
	if created.Name == "" {
		created.Name = name
	}
	return created, nil
}

// addToPlaylist appends the songs, in order and in one request, to a
// library playlist ("p.…"), as the helper's addToPlaylist; no songs is
// ErrInvalidArgument.
func (p *Player) addToPlaylist(ctx context.Context, playlistID string, songIDs []string) error {
	const op = "add to playlist"
	if !libraryPlaylistIDValid(playlistID) {
		return fmt.Errorf("library %s: %w", op, ErrInvalidLibraryID)
	}
	if len(songIDs) == 0 {
		return fmt.Errorf("library %s: no songs: %w", op, ErrInvalidArgument)
	}
	tracks, err := songTracks(op, songIDs)
	if err != nil {
		return err
	}
	body, err := editJSON(editTracks{Data: tracks})
	if err != nil {
		return err
	}
	_, err = p.write(ctx, op, "POST", libraryPlaylistsPath+"/"+playlistID+"/tracks", body)
	return err
}
