# Usage

[← Back to the README](../README.md) · [Documentation index](../README.md#documentation)

Browsing, editing your library, local music files, every key and mouse binding, the settings, and the update check.

## Browsing the catalog

Views stack like Apple Music's: PLAYLISTS → SEARCH → RESULTS → ARTIST → ALBUM,
SONG, or PLAYLIST. `esc` goes back one view, `tab` returns to the playlists. Leaving
with `tab` keeps the search branch as it was: `/` or `tab` from the playlists
brings back the same view (an artist or album page included) with its cursor.

`enter` on a library playlist opens its PLAYLIST page over the PLAYLISTS
root: `▶ PLAY` plays it from the start, and `enter` on a track plays the
playlist from that track (the dial then shows the playlist on air). The songs
the page loaded are queued by their catalog ids (`playSongs`), so playing
never reads the playlist again. Songs
that are only in your library (uploads, or songs no longer in the Apple Music
catalog) are listed muted and skipped: `enter` on one only shows a notice. `esc` or
the `PLAYLISTS` tab goes back to the list; `tab` or `/` goes to SEARCH (the
page is closed).

A song picked from a list plays with the rest of that list queued around it,
so playback goes on into the list:

| Picked from | Queue |
|-------------|-------|
| SEARCH song row | The songs listed under the input, from the picked one |
| RESULTS, SONGS | The SONGS section, from the picked song |
| RESULTS, TOP RESULTS song | The SONGS section, from the song's copy there; a top song missing from SONGS plays first, the section after it |
| ARTIST, TOP SONGS | The top songs, from the picked one |
| ALBUM, SONG | The album, from the picked track (a SONG view whose album failed to load has only the song) |
| PLAYLIST | The playlist, from the picked track (a library playlist without its library-only songs) |

On SEARCH and RESULTS, `g` on a song row opens its SONG view (the album
holding it) instead.

| View | Shows |
|------|-------|
| SEARCH | RECENT searches while the input is empty; once you type 2+ characters, live suggestions, then matching artists (with their genre) and songs |
| RESULTS | The full search for a submitted term, non-empty sections only: TOP RESULTS (tagged ARTIST, ALBUM, SONG, or PLAYLIST), ARTISTS, ALBUMS (with artist and year), SONGS (with artist), PLAYLISTS (with curator) |
| ARTIST | Its non-empty sections in Apple Music order: TOP SONGS, ESSENTIAL ALBUMS, ALBUMS, ARTIST PLAYLISTS, SINGLES & EPS, COMPILATIONS, then ABOUT (editorial notes folded behind MORE, FROM, FORMED, GENRE) |
| ALBUM | The tracks (by disc when there are several), release date, song count and length, copyright, record label, and notes |
| SONG | The album holding the song (`g` on a SEARCH or RESULTS song row), with the cursor on the song; if the album cannot be loaded, the song alone, still playable |
| PLAYLIST | The tracks with their artists, song count and length, curator, and notes; a library playlist starts with `▶ PLAY` and names its frequency |

Recent searches are the last 10 terms you submitted with `enter` (typed, a
suggestion, or a recent term) or opened an artist or song from, stored in
`nu11signal/recent.json` under `os.UserConfigDir()`
(`~/Library/Application Support/nu11signal/recent.json` on macOS). Demo mode
keeps them in memory only. Delete one with `ctrl+d` / `delete` or its `✕`.

On ALBUM, SONG, and PLAYLIST pages, `▶` marks the track the player is on
(playing or paused), and moves with it; no track is marked when the player
is on a song from elsewhere.

Limitations:

- No artwork: artists, albums, and playlists are text rows.
- FROM and FORMED rely on an undocumented Apple Music API field; they are
  left out when it is absent.
- Relationships (an artist's albums, singles, playlists, and so on) show the
  first page the catalog returns, not the full list.
- Playlists cannot be renamed or deleted, nor songs removed from them: the
  Apple Music API does not offer it. A page already open does not show songs
  added from the picker until it is opened again.

## Editing the library

- **Love** a song (Apple Music's favorite): `l` on a song row (a track, a
  top song, a song among the results or the search rows), or with no song
  row selected (the playlists, the player), the song playing. The selected
  song row ends in `-- +` (`<3 +` once loved, the `<3` lit); a loved song
  keeps its `<3` on any row, and NOW PLAYING shows a `[--]` / `[<3]` button
  beside the title. A page reads the state of all its songs in one
  `favorites` call as it loads (album, song, playlist, artist top songs,
  results, search rows), so its marks show at once; states are cached per song. The song playing, when
  it is not on the page, and the selected song, when the page read failed,
  are read on their own when the selection rests (on the next animation
  tick). A change shows at once: a refused one is reported on the status
  line and read back, and a failed single read is tried again 30 seconds
  later.
  One change per song is sent at a time; pressing `l` again meanwhile only
  moves the heart, and the latest state is sent once the first answers.
- **Add to a playlist**: `a` (or the row's `+`) opens ADD TO PLAYLIST over
  the list: `+ NEW PLAYLIST`, then the playlists songs can be added to
  (ones followed from the catalog, such as "Canciones favoritas", are left
  out). `enter` adds the selected song and closes the picker; a refusal keeps
  it open. An add that timed out may still be applied, so the picker closes
  and the status line asks you to check the playlist before trying again.
  `esc` or `◀ BACK` cancels.
- **New playlist**: the `+ NEW PLAYLIST` row over the playlists (for the keys
  and the mouse alike), or the picker's row (the new playlist then holds the
  song).
  Type the name, `enter` (or `CREATE`) creates it, `esc` (or `CANCEL`)
  cancels (back to the picker when it came from there). The new playlist is
  listed and selected at once; the list is read again, and a playlist the
  API does not list yet is kept. A create that timed out closes the name
  input too (check the playlists before trying again); the list is read
  again and selects the new playlist if it shows up.
- One add or create is in flight at a time: until it answers, another one
  shows `WRITING…` and is not sent.

Songs only in your library cannot be loved or added (the ratings and
playlist endpoints take catalog ids): `l` and `a` show a notice.

## Local files

nu11signal also plays the music files on your computer, beside Apple Music
(through the helper on macOS, the web player on Linux, see
[Apple Music on Linux](#apple-music-on-linux-experimental)), or on their
own without it or with `--local`. The folders come from `"music_dirs"` in `config.json` (see
[Settings](#settings)), `~` meaning your home; without it, `~/Music`:

```json
{"music_dirs": ["~/Music", "/Volumes/Archive/music"]}
```

- **Formats:** mp3, flac, ogg vorbis (`.ogg`, `.oga`) and wav. Titles,
  artists and albums come from the tags; a file without a title is named
  after the file, without an album after its folder. Hidden files and
  folders are skipped.
- **Playlists:** every folder that holds audio files is a playlist, its
  songs in disc and track order, and so is every `.m3u`/`.m3u8` file. They
  are listed under a `LOCAL` header in PLAYLISTS, after the Apple Music
  ones, and play like them.
- **Startup:** the folders are scanned in the background; the local
  playlists appear once the scan ends, startup never waits for it. A
  folder that does not exist is skipped.
- **Playback:** the files are decoded in nu11signal and played through the
  system output (CoreAudio on macOS; PulseAudio, PipeWire's PulseAudio
  server or ALSA on Linux), with the app's own volume and the rain driven
  by the music itself (`SPECTRUM LIVE`). Playing a local song stops Apple
  Music, and the other way round; a queue never mixes the two.
- **Not available for local songs:** search (there is no catalog; without
  Apple Music the SEARCH tab is hidden), artist and album pages, loving
  (`<3`) and adding to a playlist (no `<3` or `+` on their rows), and
  creating playlists (no `+ NEW PLAYLIST` without Apple Music).

When Apple Music access is refused, nu11signal says so on the status line
and keeps playing the local files; grant access (System Settings › Privacy
& Security › Media & Apple Music) and restart to get the catalog back.

## Apple Music on Linux (experimental)

On Linux, nu11signal plays Apple Music through Apple's own web player
(music.apple.com), in a Google Chrome or Chromium it starts hidden, with
no window, and drives over a private pipe (no debugging port is opened).
You sign in once, and the session stays in nu11signal's own browser
profile. nu11signal never reads your password, tokens or cookies.

**Requirements:** Google Chrome or Chromium, either one, with the Widevine
CDM that plays protected music. Google Chrome ships with it. Chromium
usually does not: add it (for example, copy Google Chrome's
`WidevineCdm` directory next to the Chromium binary, as in
`/usr/lib/chromium/WidevineCdm`). Google Chrome is available for Linux on
both x86_64 and ARM64. The browser is looked up
as `google-chrome`, `google-chrome-stable`, `chromium` and
`chromium-browser` on `PATH`, then in the usual install directories;
`NU11SIGNAL_BROWSER` (an absolute path) chooses one instead. If none is
usable, Google Chrome from Flathub (`com.google.Chrome`, x86_64 only) is
supported too: `flatpak` must be on `PATH` and its bundled Widevine must
be present. User installs are preferred over system installs. Snap
Chromium and Chromium Flatpaks (including UngoogledChromium) remain
unsupported and have no Widevine bundled. On Ubuntu, use Google Chrome's
`.deb` from [google.com/chrome](https://www.google.com/chrome/) or its
Flathub package instead of Chromium Snap; on ARM64, use a distribution
Chromium with Widevine.

For containers and unusual setups, `NU11SIGNAL_BROWSER_FLAGS` adds
space-separated flags to the browser's command line, for both
`nu11signal` and `--apple-music-login`, for example
`NU11SIGNAL_BROWSER_FLAGS="--no-sandbox --ozone-platform=wayland"`. Each
must be a `--flag` or `--flag=value`; `--remote-debugging-*` and
`--user-data-dir` are refused, and any other value stops startup with
the reason. `--no-sandbox` weakens the browser's isolation from the rest
of your system: use it only where the sandbox cannot run, such as some
containers.

1. Sign in once:

   ```sh
   nu11signal --apple-music-login
   ```

   A browser window opens at music.apple.com; sign in to Apple Music
   there. The window closes by itself once you are signed in, and the
   command says `Signed in. Run nu11signal to play.` (or that you already
   were). `ctrl+c` cancels it and closes the window. If no usable browser
   is found, it says what is missing.
2. Run `nu11signal`. Apple Music joins the local files, as the helper
   does on macOS.

The profile lives in `~/.config/nu11signal/webplayer` (under
`$XDG_CONFIG_HOME` when set). For Flathub Chrome it lives instead in
`~/.var/app/com.google.Chrome/nu11signal-webplayer`, so the sandbox can
see it and keep the session between runs. Both profiles are owner-only
(0700); nu11signal refuses symlinked profiles or a profile directory open
to other users and says how to fix it. Flatpak's first start can be slow
(tens of seconds); the web player allows 60 seconds for Flatpak browser
readiness and another 60 seconds for MusicKit. The command's current
startup deadline still caps normal `nu11signal` startup at 30 seconds;
`--apple-music-login` does not have that cap.
Only one browser can use the profile at a time, so quit nu11signal before
`--apple-music-login` and close the sign-in window before `nu11signal`;
otherwise startup stops with `the Apple Music profile is in use; close
the other nu11signal or login window`.

Without a Chrome or Chromium with Widevine, nu11signal plays the local
files alone, without a message, as before (`--apple-music-login` tells
you what is missing). A browser that is found but cannot start, or a
music.apple.com that does not load within 30 seconds (offline, for
example), leaves the local files playing alone too, and the status line
says why once: `apple music unavailable (browser did not start) // local
files only` or `apple music unavailable (web player did not load) //
local files only`. `nu11signal --local` plays the local files alone
without starting the browser. `--demo` does not start it either. On macOS, `--apple-music-login` is an error: Apple Music signs
in through the helper there.

Until the profile is signed in, the status line says `apple music waiting
for authorization // quit and run nu11signal --apple-music-login`, and the
local files play as usual; nu11signal asks again every few seconds and
says `apple music ready` once it is signed in.

- **Works:** search and the artist, album and playlist pages; playing
  from a page or an album as a queue; your library playlists in
  PLAYLISTS, their pages and playing them (songs that are not in the
  Apple Music catalog, such as uploads, are listed but skipped, and
  cannot be started from); pause, resume, stop, next and
  previous, seeking, the loop (repeat) mode and the volume (the web
  player's own: `VOL`); favorites (seeing and marking loved songs, `l`),
  creating playlists and adding songs to them (`a`), as in
  [Editing the library](#editing-the-library). Once a song was played,
  NOW PLAYING shows what the web player reports: the title, artist and
  album, playing or paused, the progress and the loop mode, asked every
  second. Quitting nu11signal
  stops the playback and closes the hidden browser. A browser that exits
  while nu11signal runs is reported on the status line, and its music
  shows as stopped.
- **Resources:** the hidden browser is far heavier than the macOS helper:
  on the test machine (ARM64, no GPU) it took roughly 10 % of one core
  and about 750 MB of memory while playing, and less while paused.

## Keys

PLAYLISTS (and anywhere the key is not taken by the view):

| Key | Action |
|-----|--------|
| `↑`/`↓` | Move the cursor; `↑` on the top row (`+ NEW PLAYLIST`) moves the focus to the nav tabs (see below) |
| `enter` | Open the playlist's page; on `+ NEW PLAYLIST`, name a new playlist |
| `l` | Love the song playing, or unlove it |
| `a` | Add the song playing to a playlist |
| `o` | Cycle the loop (repeat) mode: `OFF`, `ALL` (the queue starts over), `ONE` (the song starts over) |
| `space` | Play / pause |
| `n` / `p` | Next / previous track |
| `shift+←` / `shift+→` or `,` / `.` | Seek -10 s / +10 s |
| `k` / `j`, `+` / `=` and `-`, or `shift+↑` / `shift+↓` | Volume up / down by 5% |
| `→` | Move the focus to the player (see below) |
| `f` / `ctrl+f` | Expand the player to the full width, or restore it |
| `/` | Back to the search left with `tab` (same view and cursor); otherwise open SEARCH with an empty input |
| `tab` | Same as `/` |
| `esc` | Back one view |
| `r` | Retry loading the playlists after a failure |
| `x` | Turn the signal effects off or on (see [Signal effects](effects.md#signal-effects)) |
| `q` / `ctrl+c` | Quit, after asking (see below) |
| `?` | Open or close KEYS, the list of every key (see below) |
| `s` | Open or close SETTINGS, the color themes (see [Settings](#settings)) |

SEARCH (typing goes to the input, so letter shortcuts are off):

| Key | Action |
|-----|--------|
| `↑`/`↓` | Move between the input and the rows; `↑` on the input moves the focus to the nav tabs |
| `←`/`→` | On the input, move the text cursor; on a row, `→` moves the focus to the player |
| `shift+←` / `shift+→` | Seek -10 s / +10 s (`,` and `.` are typed) |
| `shift+↑` / `shift+↓` | Volume up / down (`k`, `j`, `+`, `=` and `-` are typed) |
| `ctrl+f` | Expand or restore the player (`f` is typed) |
| `enter` | On the input, a recent term, or a suggestion, open the RESULTS for that term (the input keeps it); on an artist, open its page; on a song, play it with the other songs listed queued (SEARCH stays open) |
| `ctrl+d` / `delete` | On a recent term, delete it (on the input, they edit the text) |
| `l` / `a` / `g` | On a song row, love it / add it to a playlist / open its SONG view (on the input or another row, they are typed) |
| `tab` | Back to the playlists (the search is kept for the next `/` or `tab`) |
| `esc` | Back one view (closing the search: the next `/` starts empty) |
| `ctrl+c` | Quit, after asking (`q` is typed) |

RESULTS, ARTIST, ALBUM, SONG, and PLAYLIST:

| Key | Action |
|-----|--------|
| `↑`/`↓` | Move the cursor; `↑` on the first row moves the focus to the nav tabs |
| `enter` | On RESULTS, open the selected artist, album, or playlist, or play the selected song with the rest of its list (see above); elsewhere, play the top songs or tracks from the selected one, open an album or playlist, or fold the notes (MORE/LESS) |
| `g` | On a RESULTS song, open its SONG view |
| `esc` | Back one view |
| `tab` | Back to the playlists (the page and the ones below it are kept for the next `/` or `tab`); from a library playlist page, over to SEARCH |
| `/` | Back to the SEARCH input, with the term kept for editing (the pages above it are closed) |
| `r` | Retry after the page failed to load |
| `l` | Love the selected song (a track, a top song, a song result), or unlove it; on another row, the song playing |
| `a` | Add the selected song to a playlist; on another row, the song playing |
| `space`, `n` / `p`, seek, volume and loop keys, `→`, `f` / `ctrl+f`, `x`, `q` | As on the playlists |

ADD TO PLAYLIST and NEW PLAYLIST (over the list):

| Key | Action |
|-----|--------|
| `↑`/`↓` | Move between `+ NEW PLAYLIST` and the playlists (picker) |
| `enter` | Add the song to the selected playlist, or on `+ NEW PLAYLIST` name a new one (picker); create the playlist (name) |
| `esc` | Cancel (from a name opened in the picker, back to the picker) |
| `space`, `n` / `p`, seek and volume keys, `→`, `q` | As on the playlists (picker only: the name types every key but `enter` and `esc`; `ctrl+c` asks to quit) |

Player (after `→` from the list, or a click on its panel; the lit
panel frame shows which side has the focus):

| Key | Action |
|-----|--------|
| `←` / `→` | Walk a row of buttons as drawn: `PREV`, `PLAY`/`PAUSE`, `NEXT`, `LOOP`, `VOL-`, `VOL+`, `EXPAND` when the player is wide enough for one row, else `PREV` to `EXPAND` on the transport row and `VOL-`, `VOL+` on the volume row below; the `[<3]` of the song playing is alone on its row; `←` from the first button of a row goes back to the list |
| `↑` / `↓` | Move between the `[<3]` of the song playing (while there is one), the progress bar (when the song can seek), the transport row and, with two rows, the volume row (`VOL-` under `PREV`/`PLAY`, `VOL+` under `NEXT`, `LOOP` and `EXPAND`); `↑` from the `[<3]` (or from what is under it, with no song) moves the focus to the nav tabs |
| `←` / `→` on the progress bar | Seek -10 s / +10 s |
| `enter` | Press the focused button (as a click) |
| `l` / `a` | Love / add to a playlist the song playing |
| `esc` | Back to the list (restoring an expanded player) |
| `space`, `n` / `p`, seek, volume and loop keys, `f` / `ctrl+f` | As on the playlists |

Nav tabs (after `↑` past the top of the list, the SEARCH input, or the
player; the focused tab shows a `▸`):

| Key | Action |
|-----|--------|
| `←` / `→` | Walk `PLAYLISTS`, `SEARCH` and, on a page, `◀ BACK` |
| `enter` | Press the focused tab (as a click); the list takes the focus |
| `↓` / `esc` | Back where the focus came from (the same row, the SEARCH input, or the player's bar or button) |
| `space`, `n` / `p`, seek, volume and loop keys | Act on the player; the tabs keep the focus (on SEARCH too: nothing is typed) |

On the tabs, any other key goes back where the focus came from and acts
there. On the player, any other key goes back to the list and acts there (on
SEARCH, a letter is typed). The expanded player hides the list, so it holds the focus: `←` from
`PREV` stays put, and going back to the list (`esc`, another key, `f`,
`ctrl+f`, or `RESTORE`) restores it. Expanding, by key or button, focuses
the player on the control it already had (`PLAY` from the list, `EXPAND`
once its button is pressed); restoring always gives the focus back to the
list, as it was left (on SEARCH, the same row or the input).

The footer names the keys of the side and view in focus; when it does not
fit, it keeps the essential ones first (on the playlists: `enter`, `/`,
`space`, `→`) and always `[?] KEYS` and quit (where `?` is typed, on SEARCH
and in the NEW PLAYLIST name, only quit). `[S] SETTINGS` shows only where
the whole footer fits.

`?` opens KEYS, every binding grouped (PLAYBACK, NAVIGATION, VIEW, SEARCH,
APP) in a panel over the body, wherever `q` asks to quit (on SEARCH and in
the NEW PLAYLIST name `?` is typed). `?` or `esc` (or a click) closes it;
while it is open the other keys do nothing, `q` included, and `ctrl+c` asks
to quit.

Quitting always asks first, so a stray `q` never ends the session: `q`
(wherever it is not typed) and `ctrl+c` open a small `QUIT NU11SIGNAL?`
panel over whatever is on screen (the overlays and the access error
included; over the boot splash `q` skips the boot first) while the music
keeps playing. `y` or `enter` quits,
and so does `q` or `ctrl+c` pressed again (the double press is the fast way
out); `n` or `esc` closes it and gives the screen back as it was. The other
keys do nothing while it asks. Its `[ Y QUIT ]` and `[ N STAY ]` buttons,
HUD keys like the player's (QUIT filled, as `enter`'s action; STAY in cyan
brackets), are clickable, and a click outside the panel closes it.

A confirmed quit shuts down like the boot in reverse: the panel closes and
the body shows the Braille logo again (its head alone on a terminal too
narrow for the whole logo, the line alone where even that does not fit) with
`SHUTTING DOWN...`, spaced out, under it (glitching like the boot with the effects on, still without them)
while the player closes and the music fades out; nu11signal exits once the
player has closed (or the close timed out) and the splash has shown for at
least 1 s. During it the footer is blank and every key and click is ignored
but `ctrl+c`, which exits at once.

The HUD names live state: the nav bar ends in the city net node the
radio is patched through (`NODE 7F // NU-GRID`, flavor, fixed for a session
by its seed, cut to fit); the NOW PLAYING frame says where the rain's levels come from
(`SPECTRUM LIVE` from the player's readings, `SPECTRUM SIM` animated,
`SPECTRUM HOLD` paused or stopped); and with no message the status line
names the song `n` moves to (`UP NEXT // RESONANCE · HOME`) when the song
playing is in the list nu11signal queued (after the last one, the first
while the loop repeats), else the volume driven and the effects
(`APP VOLUME // FX ON`).

The list panel keeps one width in every view (the browse pages' width),
leaving NOW PLAYING at least 30 columns.

## Mouse

The mouse does what the keys do; the keys keep working. Every control is
one control for both: each button the mouse can press, the keyboard focus
reaches too (or, for the controls ending a row, the row's own keys: `l`,
`a`, `delete`), and there is no mouse-only duplicate of a row.

| Click | Action |
|-------|--------|
| A row (playlist, search row, page row, `▶ PLAY`, MORE/LESS) | Select it and act as `enter` |
| The SEARCH input | Select the input |
| `✕` at the end of a recent term | Delete that term (as `ctrl+d` / `delete`) |
| `--` / `<3` and `+` at the end of the selected song row | Love or unlove it (as `l`); add it to a playlist (as `a`) |
| `[--]` / `[<3]` beside the title (NOW PLAYING) | Love or unlove the song playing; the focus moves to the button |
| `+ NEW PLAYLIST` (row over the playlists) | Name a new playlist |
| A picker row, `CREATE`, `CANCEL` | As `enter` on the row; create; cancel |
| A `[R] RETRY` notice | Retry, as `r` |
| `PLAYLISTS` / `SEARCH` tabs (header rule) | `PLAYLISTS` as `tab` (from a library playlist page, back to the list); `SEARCH` as `/`; the lit tab is the branch shown |
| `◀ BACK` (on RESULTS, ARTIST, ALBUM, SONG, and PLAYLIST, and over ADD TO PLAYLIST and NEW PLAYLIST) | As `esc` |
| `[◀◀]`, `[ ▶ PLAY ]` / `[ ❚❚ PAUSE ]`, `[▶▶]` (NOW PLAYING) | As `p`, `space`, `n`; the focus moves to the button |
| `[⤢]` EXPAND / `[⤡]` RESTORE (NOW PLAYING, at the right edge) | Expand the player to the full width, or restore it |
| `[−]` / `[+]` around the `VOL` meter (NOW PLAYING) | Volume down / up by 5%, as `j` / `k`; the focus moves to the button |
| `[↻ OFF]` / `[↻ ALL]` / `[↻ ONE]` (NOW PLAYING, after `NEXT`) | Cycle the loop mode, as `o`; the focus moves to the button |
| Anywhere else in a panel (its frame and empty space included) | The panel takes the focus: the list keeps its cursor (nothing opens); NOW PLAYING focuses `PLAY`, or keeps the button it had |
| The progress bar | Seek to that point of the song; the focus moves to the bar |

Other clicks on the nav bar or the header do nothing. The wheel moves the
cursor like `↑`/`↓`, stopping at the top of the list (it never reaches the
nav tabs; nothing while the player is expanded). When NOW PLAYING is wide
enough (the expanded player, for one), every control takes one row:

```
[◀◀]  [ ❚❚ PAUSE ]  [▶▶]  [↻ OFF]  VOL [−] ▮▮▮▮▮▮▮▮ [+] 90%  [⤢]
```

with the meter as wide as fits; narrower, the volume row goes under the
transport row. Narrow layouts shorten `PLAY`/`PAUSE` to its glyph, tighten
the gaps and leave out buttons that do not fit; the tiny layout has none.
The compact layout puts the volume row beside the transport buttons when it
fits (the buttons as glyphs), and leaves it out otherwise. In the compact
layout, expanding hides the list under the player.

`LOOP` shows the mode asked for at once and keeps it until the player
reports it (for at most 3 seconds); a refused change is reported on the status line and the button
shows the player's mode again. A mode changed elsewhere (the Music app)
shows with the next state.

The `VOL` readout shows Nu11Signal's own volume, independent of the system
volume (see [App volume](audio.md#app-volume)); `SYS` in its place means it shows (and changes) the system output
volume instead, the fallback. It is read at startup (`VOL --` until then, or
when the output device has no settable volume; a refused change is reported
on the status line) and again when the mode changes. Changes show at once;
rapid presses are coalesced, so only the latest level is sent once the
previous change answers.

## Settings

`s` opens SETTINGS, a panel over the body like KEYS, wherever `?` opens
KEYS (on SEARCH and in the NEW PLAYLIST name `s` is typed). Its THEMES
section lists the color themes, the active one marked `◉`:

| Theme | Look |
|-------|------|
| `BLUESHIFT` | The default: REDSHIFT recolored from the gentleman-blue palette, with as many colors: electric blue where REDSHIFT is red, violet where it is yellow, cyan where it is cyan, blue-grey shades behind |
| `REDSHIFT` | Neon red frames and text, cyan and yellow highlights |
| `MATRIX` | REDSHIFT recolored in the greens of falling code, with as many colors: mid green where REDSHIFT is red, code green where it is cyan, a pale glow where it is yellow, dark greens behind |
| `ROSE` | Soft pink highlights, peach headings, mint success, and muted frames and rain drawn from Gentleman Cute Pi colors |
| `NEON ROSE` | Vivid pink highlights, peach headings, pearl success, and muted frames and rain drawn from Gentleman Sexy Pi colors |

Both pink themes choose Pi's background ink, text, selection, muted, accent,
active pink, warning, and success colors: exactly eight across every radio role,
including frames, rain, and noise. Distinct Pi border, champagne, error, and
violet colors are omitted to stay
within that limit; these are adaptations, not exact copies of the Pi themes.
Themes style the UI without changing the terminal's background.

`↑`/`↓` move, `enter` (or a click on a row) applies the theme at once, the
whole UI recolored (frames, text, buttons, the rain, the signal effects),
and the panel stays open to compare; `s` or `esc` (or a click off the rows)
closes it. The other keys do nothing while it is open; `ctrl+c` asks to quit.

The choice is saved to `"theme"` in `nu11signal/config.json` under
`os.UserConfigDir()` and applied at the next start:

```json
{"theme": "BLUESHIFT"}
```

The file is written only when a theme is chosen, atomically (a temporary
file renamed over it), private (`0600`, its directory `0700`), keeping the
fields it does not know and the ones only you write (`"update_check"`,
`"music_dirs"`, see [Local files](#local-files)). Obsolete settings are ignored
on load and preserved as unknown fields when saving. A file that is not valid JSON is left alone and
the choice is not saved (the status line says so); a theme name nu11signal
does not know starts `BLUESHIFT` silently. A former theme name still applies
its theme (`"BLUE"` is `BLUESHIFT`, `"NIGHT CITY"` is `REDSHIFT`), saved
under the current name the next time a theme is chosen.

`BLUESHIFT` arrived in 0.7.0. A version before it does not know the name, so
after a downgrade it starts its own default theme (`REDSHIFT` in 0.6) and
leaves the saved `"BLUESHIFT"` in place until a theme is chosen there, so it
applies again after an upgrade; current versions also accept the older
`"BLUE"` and `"NIGHT CITY"`.

When a newer release is known (see [Update check](#update-check)), an
`UPDATE` line heads the panel with the version and how to upgrade.

## Update check

A release build asks GitHub once at launch, in the background, whether a
newer release exists. When one does, the status line over the key hints
reads, until you upgrade:

```text
◢◤◢◤ UPDATE v0.3.1 AVAILABLE // brew upgrade --cask nu11signal
```

with the Homebrew command when the binary lives under Homebrew, else the
release page's URL. The binary's resolved path decides which: a formula keg
(`Cellar/nu11signal/<version>/bin/nu11signal`, under any Homebrew prefix)
gets `brew upgrade nu11signal`; the cask (a `Caskroom`, or any other path
under `/opt/homebrew`) gets `brew upgrade --cask nu11signal`. SETTINGS
repeats it on its first line. A status message still takes the line while
it shows. There is nothing to dismiss, and a failed check (no network, a
rate limit) shows nothing. Startup never waits for it.

The GitHub API (`/repos/wahh-22/nu11signal/releases/latest`, 3 s timeout)
is asked at every launch, so a release published since the last one shows
at once. The last answer is kept in `update.json` beside `config.json`
(`~/Library/Application Support/nu11signal/update.json` on macOS), written
atomically and private (`0600`), and stands in when GitHub cannot be reached;
a missing or corrupt cache just means no answer offline. Pre-releases are
never offered.

No check is made:

- with `NU11SIGNAL_NO_UPDATE_CHECK=1` in the environment;
- with `"update_check": false` in `config.json` (kept when SETTINGS saves a
  theme):

  ```json
  {"theme": "BLUESHIFT", "update_check": false}
  ```

- in `--demo`, which stays offline;
- in a build without a release version (`dev`, a plain `go build`).
