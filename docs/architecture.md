# Architecture

[← Back to the README](../README.md) · [Documentation index](../README.md#documentation)

How the Go UI, the macOS Swift MusicKit helper and the Linux web player fit together, how the binary finds the helper, and the JSON lines protocol between them.

## Overview

```text
┌──────────────────────┐  JSON lines on stdin/stdout  ┌──────────────────────────┐
│ bin/nu11signal (Go)  │ ───── commands ────────────▶ │ Nu11SignalHelper.app     │
│ Bubble Tea radio UI  │ ◀──── responses, events ──── │ (Swift, MusicKit,        │
│ Player port + adapter│                              │  ApplicationMusicPlayer) │
└──────────────────────┘                              └──────────────────────────┘
```

| Part | Where | Role |
|------|-------|------|
| Radio UI | `internal/radio` | Model/update/view; depends only on the `Player` port |
| Player port | `internal/playback` | Domain types, the `Player` interface, sources and capabilities |
| Helper adapter | `internal/helper` | Starts the macOS helper, correlates requests, streams state |
| Web player | `internal/playback/webplayer` | Drives Apple's web player in a hidden Chrome or Chromium on Linux |
| Local player | `internal/playback/local` | Scans, decodes and plays the computer's music files |
| Composite player | `internal/playback/composite` | Joins Apple Music and the local files, routing by id namespace (`local:`) |
| Demo player | `internal/playback/demo` | In-process simulated player for `--demo` |
| Helper | `helper/` | SwiftPM package; `build.sh` bundles and signs the `.app` |

MusicKit exposes no audio samples, but in [app volume](audio.md#app-volume) mode the
helper renders the music itself, so the rain plays its real spectrum (see
[Spectrum](audio.md#spectrum) and [Rain](effects.md#rain)). Otherwise (system volume,
`--demo`, macOS before 15) the rain is decorative: it follows the playback
state, not the sound.

## Linux web player (experimental)

On Linux, `internal/playback/webplayer` starts a headless Chrome or Chromium
with Widevine and drives music.apple.com's page MusicKit over CDP (Chrome
DevTools Protocol), using `--remote-debugging-pipe`, not a debugging port.
Calls use JSON-encoded arguments and return plain JSON values; nu11signal
never reads tokens, cookies or browser storage. There is no developer token,
key or companion server.

The user signs in once with `nu11signal --apple-music-login`; the session
stays in nu11signal's own owner-only browser profile. The web player is the
composite player's Apple Music primary beside the local player. Local files
remain available while authorization is pending; authorization is checked
again every few seconds, so Apple Music can join later. If the browser is
unavailable or cannot start or load the page, playback falls back to local
files. See [Apple Music on Linux](usage.md#apple-music-on-linux-experimental)
for setup and limitations.

## Helper lookup

`nu11signal` never looks in the working directory. It uses the first match:

1. `$NU11SIGNAL_HELPER`: an absolute path to the helper executable (relative paths are rejected).
2. `<binary dir>/Nu11SignalHelper.app/Contents/MacOS/nu11signal-helper`
3. `<binary dir>/../libexec/Nu11SignalHelper.app/...` (packaged installs)
4. `<binary dir>/../build/Nu11SignalHelper.app/...` (this repo: `bin/` + `build/`)

The binary directory is resolved through symlinks. The error lists every path tried.

## Protocol

One JSON object per line.

| Direction | Shape |
|-----------|-------|
| Request | `{"id":"<string>","cmd":"<name>", ...args}` |
| Response | `{"id":"<id>","ok":true,"result":{...}}` or `{"id":"<id>","ok":false,"error":"<msg>"}` |
| Event | `{"event":"ready"}`, `{"event":"state","state":{...}}`, `{"event":"error","message":"<msg>"}`, `{"event":"levels","bands":[...],"wave":[...]}` (app volume mode only; see [Spectrum](audio.md#spectrum)) |

| Command | Args | Result |
|---------|------|--------|
| `authorize` | none | `{"status":...}`: `authorized`, `denied`, `restricted`, or `notDetermined` |
| `searchCatalog` | `term` (not blank), `limit` (clamped to 1-25 per type; suggestions to 10, top results to 6) | `{"suggestions":[...],"top":[{"kind":"artist","artist":{...}},...],"artists":[...],"albums":[...],"songs":[...],"playlists":[...]}`; `kind` is `artist`, `album`, `song`, or `playlist` (other top result kinds are left out) |
| `artist` | `artistId` | `{"artist":{...},"topSongs":[...],"essentialAlbums":[...],"albums":[...],"singles":[...],"compilations":[...],"playlists":[...],"about":{"notes","genre","origin","formed"}}` |
| `album` | `albumId` | `{"album":{...},"tracks":[...],"genre","releaseDate","recordLabel","copyright","notes"}` |
| `songAlbum` | `songId` | Same as `album`, for the album holding the song |
| `catalogPlaylist` | `playlistId` | `{"playlist":{...},"tracks":[...],"notes"}` |
| `playlists` | none | `{"playlists":[{"id","name","editable"}]}`: alphabetical, with Apple Music API library ids (`p.…`); `editable` is false for playlists followed from the catalog |
| `libraryPlaylist` | `playlistId` (an API library id, `p.…`) | `{"playlist":{"id","name"},"tracks":[...],"notes"}`; songs only, with their catalog ids; a song not in the catalog keeps its library id (`i.…`) and carries `"libraryOnly":true` |
| `playSongs` | `ids`, `startIndex` | `{}`, or any of `{"missing":[...],"skipped":[...],"startedAlone":true}` (see [Queue preparation](#queue-preparation)) |
| `playPlaylist` | `playlistId`, optional `startIndex` (an index into the `libraryPlaylist` tracks) | Same as `playSongs`; the playlist's catalog songs are queued (library-only songs are skipped; starting at one is an error). The UI plays library playlists with `playSongs` from the tracks it loaded instead |
| `pause`, `resume`, `next`, `previous`, `stop` | none | `{}` |
| `setRepeat` | `mode`: `off`, `all` (the queue), or `one` (the current song) | `{}`; `state` events report the mode as `repeat` (`off` when the player has none; the UI reads any mode it does not know as `off`) |
| `seek` | `seconds` (>= 0) | `{}`, followed by a `state` event |
| `volume` | none | `{"level":...,"mode":"app"\|"system"}`: the app volume (`app`) or, as a fallback, the system output volume (`system`), 0 to 1 (runs concurrently); `state` events carry the mode as `volumeMode`; see [App volume](audio.md#app-volume) |
| `setVolume` | `level` (clamped to 0-1) | `{"level":...,"mode":...}` as `volume` reports after the change; in `app` mode the system volume is never touched; an error when the system output device's volume cannot be changed (runs concurrently) |
| `createPlaylist` | `name` (not blank), optional `description`, optional `songIds` (in order) | `{"id","name"}`: the new playlist, with its Apple Music API library id (`p.…`) |
| `addToPlaylist` | `playlistId` (an API library id, `p.…`), `songIds` (not empty) | `{}`, or an error such as `playlist is not editable` |
| `favorite` | `songId` | `{"favorite":true\|false}`: whether the song is loved (no rating is `false`; an unreadable ratings answer fails the command) |
| `favorites` | `songIds` (possibly empty) | `{"favorites":{"<id>":true\|false,...}}`: every requested id, loved or not; one ratings read per 100 ids of each kind (runs concurrently); an unreadable ratings answer fails the command |
| `setFavorite` | `songId`, `on` (a boolean) | `{}`: `true` loves the song, `false` removes its rating (a song without one included) |

Playback commands (`setRepeat` included) run one at a time in arrival order, each bounded by 10 s
(a hung one is answered with a timeout error); `authorize`, `playlists`, and
the catalog commands run concurrently. Catalog commands stay within their own
time budget (`CatalogBudget` in `helper/Sources/Nu11SignalProtocol/Catalog.swift`),
below the Go client's deadline; an artist page section that fails or hangs is
left empty instead of failing the page. The library edits (`createPlaylist`,
`addToPlaylist`, `favorite`, `setFavorite`) and the `favorites` read run
concurrently too, one Apple Music API request each (`/v1/me/library/playlists`,
`/v1/me/ratings/...`; `favorites` sends one `GET /v1/me/ratings/songs?ids=...`
or `.../library-songs?ids=...` per batch) through
MusicKit's `MusicDataRequest`, as MusicKit's own library editing is unavailable on
macOS. `playlists` and `libraryPlaylist` read the library through the same API
(`GET /v1/me/library/playlists` and `.../{id}/tracks`, following pages up to 500
playlists or 1000 songs), so every id they return is one the edits accept: song
ids are catalog ids or API library ids (`i.…`), playlist ids are `p.…`. Ids are
checked (letters, digits, and dots only) before they go into a request path. A
`createPlaylist` or `addToPlaylist` that times out may still be applied: its
error says the outcome is unknown, and it is not retried. At stdin EOF the helper finishes in-flight work
(up to 3 s), stops playback, and exits.

### Queue preparation

On macOS, `ApplicationMusicPlayer` refuses a queue whose first song comes from
the catalog when the queue also holds a song of this Mac's local library
(`MPMusicPlayerControllerErrorDomain` code 6, "Failed to prepare to play" or
"Prepare queue failed with unexpected start item"), although each song prepares
on its own. So `playSongs` and `playPlaylist` look the queued songs up in the
local library first (`MusicLibraryRequest`, up to 1.5 s): when the chosen song
is in it, every local song is queued as its library copy; otherwise the local
songs are left out and listed as `"skipped"`. If the player still cannot prepare
the queue, the chosen song is queued on its own (`"startedAlone":true`) and,
once it plays, the songs after it are appended to the queue, so the list
still plays on (a failed append is only logged: the song plays alone); if
even the song alone fails, the error names the song. These steps are logged
to the helper's stderr. `state` events name a song queued as its library
copy by the catalog id asked for, so the UI's `▶` and favorite marks match.
