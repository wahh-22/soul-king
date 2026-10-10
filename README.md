<a id="top"></a>

<div align="center">
  <img src="docs/assets/brand/logo/nu11signal-blueshift.svg" width="900" alt="NU11SIGNAL logo: a masked head with headphones and LED-grid eyes beside the NU11SIGNAL wordmark">
</div>

<h1 align="center">nu11signal</h1>

<p align="center"><strong>A terminal-native music player for Apple Music and local files.</strong></p>

<p align="center">
  <a href="https://github.com/wahh-22/nu11signal/releases/latest"><img src="https://img.shields.io/github/v/release/wahh-22/nu11signal?style=for-the-badge&labelColor=0A0A0A&color=FF5F57" alt="Latest release"></a>
  <a href="docs/install.md"><img src="https://img.shields.io/badge/macOS%2014%2B%20%C2%B7%20Linux-5EF6FF?style=for-the-badge&labelColor=0A0A0A" alt="macOS 14 or later, and Linux"></a>
  <a href="docs/install.md"><img src="https://img.shields.io/badge/brew-wahh--22%2Ftap-FCEE0A?style=for-the-badge&labelColor=0A0A0A&logo=homebrew&logoColor=FCEE0A" alt="Homebrew cask wahh-22/tap/nu11signal"></a>
  <a href="https://github.com/wahh-22/nu11signal/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/wahh-22/nu11signal/ci.yml?branch=main&style=for-the-badge&labelColor=0A0A0A&label=CI" alt="CI status"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/wahh-22/nu11signal?style=for-the-badge&labelColor=0A0A0A&color=FF5F57" alt="MIT license"></a>
  <a href="https://github.com/wahh-22/nu11signal/stargazers"><img src="https://img.shields.io/github/stars/wahh-22/nu11signal?style=for-the-badge&labelColor=0A0A0A&color=FF5F57" alt="GitHub stars"></a>
  <a href="https://github.com/wahh-22/nu11signal/commits/main"><img src="https://img.shields.io/github/last-commit/wahh-22/nu11signal?style=for-the-badge&labelColor=0A0A0A&color=9A3B37" alt="Last commit"></a>
</p>

<p align="center">
  <a href="https://github.com/sponsors/wahh-22"><img src="https://img.shields.io/badge/sponsor-%E2%99%A5-FF5F57?style=for-the-badge&labelColor=0A0A0A&logo=githubsponsors&logoColor=FF5F57" alt="Sponsor on GitHub"></a>
  <a href="https://buymeacoffee.com/wahh.dev"><img src="https://img.shields.io/badge/buy%20me%20a%20coffee-%E2%98%95-FCEE0A?style=for-the-badge&labelColor=0A0A0A&logo=buymeacoffee&logoColor=FCEE0A" alt="Buy me a coffee"></a>
</p>

<p align="center">
  <strong>
    <a href="https://nu11signal.wahh.dev/">Website</a>
    &nbsp;·&nbsp;
    <a href="#install">Install</a>
    &nbsp;·&nbsp;
    <a href="#documentation">Docs</a>
    &nbsp;·&nbsp;
    <a href="https://github.com/wahh-22/nu11signal/releases">Releases</a>
    &nbsp;·&nbsp;
    <a href="#support-the-signal">Support</a>
  </strong>
</p>

<br>

<p align="center">Built for the terminal: instant search, keyboard and mouse control, and beautiful visuals.<br>Browse and play without leaving your shell.</p>

<p align="center">
  <img src="docs/assets/demo/overview.gif" width="900" alt="nu11signal in action: launching from the shell, the boot splash, opening a playlist, playing a song, and the data rain reacting to the music">
</p>

> macOS and Linux. Apple Music needs a subscription; Linux support is experimental and uses Apple's web player in a hidden Chrome or Chromium with Widevine. Local files work on both. Not affiliated with Apple.

## What it does

- **Apple Music in the terminal.** On macOS it plays through a tiny windowless MusicKit helper (near 0% CPU), not a browser or the Music app. On Linux (experimental), it uses Apple's web player in a hidden Chrome or Chromium with Widevine, a heavier backend. [Architecture →](docs/architecture.md) · [Linux setup →](docs/usage.md#apple-music-on-linux-experimental)
- **Your own music files.** mp3, flac, ogg and wav from `~/Music` play beside Apple Music on macOS and Linux, or on their own without a browser. [Local files →](docs/usage.md#local-files)
- **Search and browse the catalog.** Live suggestions, artists, albums and playlists; love songs and edit playlists on the way. [Browsing →](docs/usage.md#browsing-the-catalog)

<img width="100%" src="docs/assets/demo/search.gif" alt="SEARCH in action: recent searches, live suggestions while typing, then an artist page with its top songs, albums and playlists, and a song playing">

## Themes and keys

Five color themes in SETTINGS (`s`), BLUESHIFT by default. Everything works
from the keyboard or the mouse, and `?` lists every key.
[Settings →](docs/usage.md#settings) · [Keys →](docs/usage.md#keys) · [Mouse →](docs/usage.md#mouse)

<img width="100%" src="docs/assets/demo/themes.gif" alt="SETTINGS in action: switching between the BLUESHIFT, REDSHIFT and NEON ROSE themes, then quitting through the QUIT NU11SIGNAL? confirmation and the shutdown splash">

## Install

macOS 14+ (Apple Silicon and Intel), signed and notarized, with [Homebrew](https://brew.sh):

```sh
brew install --cask wahh-22/tap/nu11signal
nu11signal
```

Linux (x86_64 or ARM64, Apple Music experimental and local files), with [Homebrew on Linux](https://docs.brew.sh/Homebrew-on-Linux):

```sh
brew install wahh-22/tap/nu11signal
```

Release archives, the Kode Mono font the UI is designed with, and building
from source are covered in [Install](docs/install.md) and
[Building from source](docs/building.md).

## Documentation

| Guide | What it covers |
|-------|----------------|
| [Install](docs/install.md) | Homebrew (cask and formula), release archives, Linux, the Kode Mono font |
| [Usage](docs/usage.md) | The catalog, the library, local files, keys, mouse, settings, update check |
| [Signal effects and rain](docs/effects.md) | Glitches, boot and shutdown splashes, intros, the rain |
| [App volume and spectrum](docs/audio.md) | The independent `VOL` and the live spectrum |
| [Architecture](docs/architecture.md) | Go UI, Swift helper, Linux web player, JSON lines protocol |
| [Building from source](docs/building.md) | Requirements, signing setup, make targets |
| [Releasing](docs/releasing.md) | Signed releases, Linux archives, the Homebrew tap |
| [Troubleshooting](docs/troubleshooting.md) | Common failures and fixes |
| [Contributing](docs/contributing.md) | Development checks and repository notes |

## Support the signal

nu11signal is free and open source. To keep it on air,
[sponsor on GitHub](https://github.com/sponsors/wahh-22) or
[buy me a coffee](https://buymeacoffee.com/wahh.dev).

## Contributing

Issues and pull requests are welcome: start with `make demo` and run
`make test` before opening one. See [Contributing](docs/contributing.md).

## License

MIT, see [LICENSE](LICENSE).

<p align="right"><a href="#top">Back to top ↑</a></p>
