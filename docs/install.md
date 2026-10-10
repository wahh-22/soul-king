# Install

[← Back to the README](../README.md) · [Documentation index](../README.md#documentation)

Install a signed, notarized macOS build or a Linux build, and set up the font the UI is designed with. To build from source instead, see [Building from source](building.md).

## Download

Signed, notarized builds (macOS 14 or later, Apple Silicon and Intel) are
published on [GitHub Releases](https://github.com/wahh-22/nu11signal/releases).
No Apple Developer account is needed to run them.

With [Homebrew](https://brew.sh), the cask is the recommended install (it
also installs the Kode Mono font, see [Font](#font)):

```sh
brew install --cask wahh-22/tap/nu11signal
```

The tap's formula installs the same signed, notarized build without the font
(`brew install wahh-22/tap/nu11signal`, which is also what that command
resolves to without `--cask`); install only one of them.

Or manually from a release archive:

```sh
tar -xzf nu11signal-<version>-macos-universal.tar.gz
nu11signal-<version>/bin/nu11signal          # keep bin/ and libexec/ together
nu11signal-<version>/bin/nu11signal --version
```

Symlink `bin/nu11signal` onto your `PATH` if you like; the helper is found
through the symlink. The cask and the formula live in
[wahh-22/homebrew-tap](https://github.com/wahh-22/homebrew-tap) and are
generated from `packaging/homebrew/nu11signal.rb.template` and
`packaging/homebrew/nu11signal-formula.rb.template`. The first launch asks for
Apple Music access.

## Linux

Releases include Linux builds for x86_64 (`amd64`) and ARM64 (`arm64`). On
Linux nu11signal plays Apple Music through Apple's web player (experimental),
beside your [local music files](usage.md#local-files).

Apple Music needs a subscription and Google Chrome or Chromium with Widevine.
Google Chrome ships with Widevine, on x86_64 and ARM64 alike; Chromium needs
it added. Local files work without a browser.

Native browsers also need `libpulse.so.0` for sound: install `libpulse0`
(`sudo apt install libpulse0`) on Debian/Ubuntu, or `pulseaudio-libs`
(`sudo dnf install pulseaudio-libs`) on Fedora. Chrome's `.deb` does not
install it automatically; minimal systems such as WSL may lack it.
Flatpak Chrome includes it in its runtime.

Google Chrome from Flathub (`com.google.Chrome`) is supported on x86_64,
with `flatpak` on `PATH` and its bundled Widevine present. Native browsers
are preferred, then user Flatpak installs, then system Flatpak installs.
Its persistent, owner-only (0700) profile is
`~/.var/app/com.google.Chrome/nu11signal-webplayer`; native browsers use
`~/.config/nu11signal/webplayer` (or `$XDG_CONFIG_HOME/nu11signal/webplayer`).
The first Flatpak start can take tens of seconds. Sign-in allows extra
time; normal startup currently still has a 30-second command deadline.

Snap Chromium and Chromium Flatpaks (including UngoogledChromium) remain
unsupported and have no Widevine bundled. On Ubuntu, install Google
Chrome's `.deb` from [google.com/chrome](https://www.google.com/chrome/) or
its Flathub package instead of Chromium Snap; on ARM64, use a distribution
Chromium with Widevine.

After installing, sign in once:

```sh
nu11signal --apple-music-login
```

Then run `nu11signal`. See [Apple Music on Linux](usage.md#apple-music-on-linux-experimental)
for Widevine setup, browser selection and extra flags, and limitations.

With [Homebrew on Linux](https://docs.brew.sh/Homebrew-on-Linux) (the
formula; the nu11signal cask is macOS-only):

```sh
brew install wahh-22/tap/nu11signal
brew install --cask font-kode-mono   # optional: the font the UI is designed with
```

A formula cannot install a cask, so the font is a separate step; Homebrew on
Linux installs font casks into `~/.local/share/fonts` (see [Font](#font)).

Or from a release archive on
[GitHub Releases](https://github.com/wahh-22/nu11signal/releases) (use
`arm64` on ARM machines):

```sh
curl -LO https://github.com/wahh-22/nu11signal/releases/download/v<version>/nu11signal-<version>-linux-amd64.tar.gz
curl -LO https://github.com/wahh-22/nu11signal/releases/download/v<version>/nu11signal-<version>-linux-amd64.tar.gz.sha256
sha256sum -c nu11signal-<version>-linux-amd64.tar.gz.sha256
tar -xzf nu11signal-<version>-linux-amd64.tar.gz
install -m 755 nu11signal-<version>/bin/nu11signal ~/.local/bin/   # any directory on your PATH
nu11signal --version
```

The archive holds only `bin/nu11signal`, `LICENSE`, and `README.md`; the
binary is self-contained, so it can live anywhere on your `PATH`.

Sound goes through PulseAudio (PipeWire's `pipewire-pulse` counts) or,
without it, ALSA; both are loaded at run time, so the binary needs no audio
packages to start and the build needs no audio headers and no cgo
(`CGO_ENABLED=0` works). Loading them needs a glibc-based distribution (the
binary uses the system's `ld-linux` loader; musl systems such as Alpine are
not supported).

To build from source instead, with Go (see
[Building from source](building.md) for the version):

```sh
git clone https://github.com/wahh-22/nu11signal.git
cd nu11signal
go build ./cmd/nu11signal
./nu11signal
```

| Need | Notes |
|------|-------|
| Go (see `go.mod`), source builds only | Builds the binary; no C compiler, no `libasound2-dev` |
| PulseAudio or PipeWire with `pipewire-pulse` | Found at `$XDG_RUNTIME_DIR/pulse/native`, which desktop sessions set; elsewhere (`su`, some SSH or container shells) point `PULSE_SERVER` at the socket, for example `export PULSE_SERVER="$(pactl info \| sed -n 's/^Server String: //p')"` |
| `libasound.so.2` (ALSA) | Used only when PulseAudio cannot be reached; needs a real sound card, since ALSA's `default` without one stops taking audio after the first fraction of a second |

When the sound output does not work, nu11signal says so instead of
freezing: an output that has not opened after 5 seconds fails the play,
and a song that plays for 5 seconds without the output taking any audio
(ALSA's `default` without a sound card) is stopped. Either way the status
line reads `NO AUDIO OUTPUT // IS PULSEAUDIO OR PIPEWIRE RUNNING?`; start
PulseAudio or PipeWire (or set `PULSE_SERVER`, above), then restart
nu11signal. A paused song is not watched.

Known limitation: fixing the audio setup takes effect only after a restart.
The audio library (oto) allows one audio output per process, opened once, so
an output that failed (or that opened on ALSA without a sound card) stays
failed until nu11signal is restarted.

CI builds and tests on Linux (Ubuntu) and plays a short tone through the
real audio path against a PulseAudio null sink
(`go test -tags audiosmoke -run Smoke ./internal/playback/local/...`).

## Font

The UI is designed with [Kode Mono](https://fonts.google.com/specimen/Kode+Mono),
which the cask installs (`depends_on cask: "font-kode-mono"`, in releases
after v0.2.1; by hand, on macOS or Linux:
`brew install --cask font-kode-mono`, into `~/Library/Fonts` on macOS and
`~/.local/share/fonts` on Linux). A terminal UI cannot choose its font:
the terminal draws every character with the font it is set to, so Kode Mono
shows once your terminal uses it, for example `font-family = Kode Mono` in
Ghostty's config or the profile font in Terminal.app or iTerm2. Any
monospaced font works; the frames, blocks and rain glyphs look the same in
all of them.
