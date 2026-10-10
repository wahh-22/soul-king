// Package playback is the domain core of Nu11Signal: the music types the UI
// works with and the Player port that any playback backend implements.
package playback

import (
	"context"
	"math"
	"time"
)

// Song is a catalog track, or a song of a library playlist.
type Song struct {
	// ID is the catalog id; a song only in the library (LibraryOnly)
	// carries its Apple Music API library id ("i.…") instead.
	ID       string
	Title    string
	Artist   string
	Album    string
	Duration time.Duration
	// LibraryOnly marks a library playlist's song that is not in the
	// Apple Music catalog (an upload, or a song since removed): it cannot
	// be played, and PlayPlaylistFrom refuses to start at it.
	LibraryOnly bool
}

// Artist is a catalog artist.
type Artist struct {
	ID     string
	Name   string
	Genres []string
}

// SearchResults is a mixed catalog search, as Apple Music shows it: term
// suggestions to refine the query, the top results across every kind, then
// matching artists, albums, songs and playlists, each in relevance order.
type SearchResults struct {
	Suggestions []string
	Top         []SearchItem
	Artists     []Artist
	Albums      []Album
	Songs       []Song
	Playlists   []CatalogPlaylist
}

// SearchItemKind names what a SearchItem is.
type SearchItemKind string

// Kinds of top search results.
const (
	ItemArtist   SearchItemKind = "artist"
	ItemAlbum    SearchItemKind = "album"
	ItemSong     SearchItemKind = "song"
	ItemPlaylist SearchItemKind = "playlist"
)

// SearchItem is one top search result: Kind says which of the other fields
// holds it; the rest stay zero.
type SearchItem struct {
	Kind     SearchItemKind
	Artist   Artist
	Album    Album
	Song     Song
	Playlist CatalogPlaylist
}

// Album is a catalog album, single or compilation.
type Album struct {
	ID     string
	Title  string
	Artist string
	// Year is the release year; 0 when unknown.
	Year       int
	TrackCount int
}

// CatalogPlaylist is a catalog playlist, such as an artist's essentials.
type CatalogPlaylist struct {
	ID      string
	Name    string
	Curator string
}

// ArtistAbout is the "About" section of an artist page. Every field is
// empty when the catalog does not know it.
type ArtistAbout struct {
	// Notes is the editorial text, as plain text.
	Notes  string
	Genre  string
	Origin string
	Formed string
}

// ArtistDetail is an artist page: its sections in Apple Music order, each
// empty when the catalog has nothing for it.
type ArtistDetail struct {
	Artist          Artist
	TopSongs        []Song
	EssentialAlbums []Album
	Albums          []Album
	Singles         []Album
	Compilations    []Album
	Playlists       []CatalogPlaylist
	About           ArtistAbout
}

// Track is a song as it appears on an album: the song and its position.
type Track struct {
	Song
	// Number is the track number on its disc; 0 when unknown.
	Number int
	// Disc is the disc number; 0 when unknown.
	Disc int
}

// AlbumDetail is an album page: the album, its tracks in order and the
// facts Apple Music lists under them. Every field but Album may be empty.
type AlbumDetail struct {
	Album  Album
	Tracks []Track
	Genre  string
	// ReleaseDate is the release date as "2006-01-02"; empty when unknown.
	ReleaseDate string
	RecordLabel string
	Copyright   string
	// Notes is the editorial text, as plain text.
	Notes string
}

// PlaylistDetail is a playlist page: the playlist, its songs in order and
// its description as plain text. A library playlist has no curator; its
// songs carry catalog ids, but for the LibraryOnly ones.
type PlaylistDetail struct {
	Playlist CatalogPlaylist
	Tracks   []Song
	Notes    string
}

// Playlist is a library playlist; the UI presents it as a radio station.
type Playlist struct {
	// ID is the Apple Music API library id ("p.…"), or a local one
	// (LocalPrefix).
	ID   string
	Name string
	// Editable reports whether songs may be added to it: false for a
	// playlist followed from the catalog, and for local ones.
	Editable bool
	// Source is the backend the playlist comes from; empty is the
	// primary one (Apple Music), as a backend that names none.
	Source Source
}

// Status is the player's playback status.
type Status string

// Playback statuses reported by the player.
const (
	StatusPlaying     Status = "playing"
	StatusPaused      Status = "paused"
	StatusStopped     Status = "stopped"
	StatusInterrupted Status = "interrupted"
	StatusSeeking     Status = "seeking"
)

// RepeatMode is what the player plays after the current song.
type RepeatMode string

// Repeat modes: RepeatOff ends the queue after its last song, RepeatAll
// starts the queue over and RepeatOne starts the current song over.
const (
	RepeatOff RepeatMode = "off"
	RepeatAll RepeatMode = "all"
	RepeatOne RepeatMode = "one"
)

// State is a point-in-time snapshot of the player.
type State struct {
	Status   Status
	Title    string
	Artist   string
	Album    string
	SongID   string
	Duration time.Duration
	Position time.Duration
	// Repeat is the player's repeat mode; RepeatOff when it reports none.
	Repeat RepeatMode
	// VolumeMode is which volume Volume and SetVolume drive; empty when
	// the player does not say.
	VolumeMode VolumeMode
}

// VolumeMode is which volume the player's Volume and SetVolume drive.
type VolumeMode string

// Volume modes: VolumeApp is the player's own volume, independent of the
// system's; VolumeSystem is the system output volume (the fallback when
// the app volume is unavailable).
const (
	VolumeApp    VolumeMode = "app"
	VolumeSystem VolumeMode = "system"
)

// LevelSource is implemented by players that can measure what they play.
// It is separate from Player, and from State, because only some backends
// measure (the helper, in app-volume mode) and readings arrive about 15
// times a second: a UI polls the latest one on its own frame clock instead
// of handling each as a state change.
type LevelSource interface {
	// Levels delivers readings while the player measures (see Spectrum).
	// No readings arrive while paused, stopped, or when the player cannot
	// measure. The channel keeps only the latest reading and is closed
	// when the player shuts down.
	Levels() <-chan Spectrum
}

// Spectrum is one reading of a LevelSource.
type Spectrum struct {
	// Bands are one level per band, 0 (silent) to 1 (loud), bands
	// log-spaced from low to high frequencies.
	Bands []float64
	// Wave is the waveform of the samples measured, oldest first, each
	// point -1 to 1 (full scale); empty when the player sends none.
	Wave []float64
}

// AuthStatus is the outcome of a music library authorization request.
type AuthStatus string

// Authorization outcomes.
const (
	AuthAuthorized    AuthStatus = "authorized"
	AuthDenied        AuthStatus = "denied"
	AuthRestricted    AuthStatus = "restricted"
	AuthNotDetermined AuthStatus = "notDetermined"
)

// LateAuthorizer is implemented by a Player whose authorization can
// complete after Authorize first answers AuthNotDetermined, as a browser
// the user still has to sign in to: when AuthorizesLate reports true,
// asking Authorize again later may answer AuthAuthorized. A Player that
// does not implement it is refused for good by AuthNotDetermined.
type LateAuthorizer interface {
	AuthorizesLate() bool
}

// AuthorizationHinter is implemented by a LateAuthorizer that can tell the
// user how to authorize it while it waits, such as the command that signs
// in. AuthorizationHint is a short lowercase instruction; "" gives none.
type AuthorizationHinter interface {
	AuthorizationHint() string
}

// ClampVolume limits a volume level to 0...1; NaN becomes 0.
func ClampVolume(level float64) float64 {
	if math.IsNaN(level) {
		return 0
	}
	return min(max(level, 0), 1)
}

// QueueReport is what a play left out of the queue it started. The zero
// value is a clean play: every requested song was queued.
type QueueReport struct {
	// Missing are the requested ids the catalog did not return.
	Missing []string
	// Skipped are the ids found but left out because the player cannot
	// queue them with the start song (songs of the local library).
	Skipped []string
	// StartedAlone reports that only the start song plays: the player
	// refused the rest of the queue.
	StartedAlone bool
}

// Clean reports whether nothing was left out.
func (r QueueReport) Clean() bool {
	return len(r.Missing) == 0 && len(r.Skipped) == 0 && !r.StartedAlone
}

// Player is the port through which the application drives playback.
//
// Methods are safe for concurrent use. States delivers snapshots as they
// change and Errors delivers asynchronous backend failures; both channels are
// closed when the player shuts down, whether through Close or a backend crash.
type Player interface {
	Authorize(ctx context.Context) (AuthStatus, error)
	SearchCatalog(ctx context.Context, term string, limit int) (SearchResults, error)
	Artist(ctx context.Context, artistID string) (ArtistDetail, error)
	// Album loads a catalog album page.
	Album(ctx context.Context, albumID string) (AlbumDetail, error)
	// SongAlbum loads the page of the album that contains the catalog song.
	SongAlbum(ctx context.Context, songID string) (AlbumDetail, error)
	CatalogPlaylist(ctx context.Context, playlistID string) (PlaylistDetail, error)
	// Playlists lists the library playlists, alphabetically.
	Playlists(ctx context.Context) ([]Playlist, error)
	// LibraryPlaylist loads a library playlist page: its songs, in order.
	LibraryPlaylist(ctx context.Context, playlistID string) (PlaylistDetail, error)
	// PlaySongs queues the catalog songs and plays from ids[start]; the
	// report lists the songs the player left out of that queue.
	PlaySongs(ctx context.Context, ids []string, start int) (QueueReport, error)
	// PlayPlaylist plays a library playlist from its first song. Songs
	// that are LibraryOnly are skipped.
	PlayPlaylist(ctx context.Context, id string) error
	// PlayPlaylistFrom plays a library playlist starting at the song at
	// index start of its LibraryPlaylist tracks, skipping LibraryOnly
	// songs; starting at one of them is an error.
	PlayPlaylistFrom(ctx context.Context, playlistID string, start int) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Next(ctx context.Context) error
	Previous(ctx context.Context) error
	Stop(ctx context.Context) error
	Seek(ctx context.Context, position time.Duration) error
	// SetRepeat sets the repeat mode; States reports it as State.Repeat.
	SetRepeat(ctx context.Context, mode RepeatMode) error
	// Volume reports the output volume, from 0 (silent) to 1 (full): the
	// one State.VolumeMode names.
	Volume(ctx context.Context) (float64, error)
	// SetVolume sets the output volume; level is clamped with ClampVolume.
	SetVolume(ctx context.Context, level float64) error
	// CreatePlaylist creates a library playlist holding the songs, in
	// order; description and songIDs may be empty. The returned playlist's
	// ID is what AddToPlaylist takes. A new playlist may take a moment to
	// appear in Playlists.
	CreatePlaylist(ctx context.Context, name, description string, songIDs []string) (Playlist, error)
	// AddToPlaylist appends the songs, in order, to a library playlist.
	AddToPlaylist(ctx context.Context, playlistID string, songIDs []string) error
	// Favorite reports whether the song is a favorite.
	Favorite(ctx context.Context, songID string) (bool, error)
	// Favorites reports, for each of the songs, whether it is a favorite:
	// one call for a whole page of songs. Every id is a key of the map;
	// no ids answer an empty map without asking the backend.
	Favorites(ctx context.Context, songIDs []string) (map[string]bool, error)
	// SetFavorite marks the song as a favorite, or clears the mark.
	SetFavorite(ctx context.Context, songID string, on bool) error
	States() <-chan State
	Errors() <-chan error
	Close() error
}
