// Package webplayer plays Apple Music on Linux through Apple's own web
// player (option D): nu11signal starts Google Chrome or Chromium with a
// Widevine CDM (Discover), headless and in an owner-only profile of its own
// (ProfileDir), opens music.apple.com there and drives the page's MusicKit
// over the Chrome DevTools Protocol on a pipe (Launch, Client, Attach,
// Page.Evaluate). No debugging port is opened, so no other local process
// can attach to the browser.
//
// There is no developer token, key or companion server: the user signs in
// to Apple Music once in that browser, and the session stays in the
// profile, as it would in any browser. nu11signal never reads developer or
// user tokens, cookies or storage; it only evaluates player calls in the
// page and takes back plain JSON values, and page exceptions reach it as
// short sanitized descriptions (ScriptError).
//
// Player is the playback.Player over that page: it installs a small script
// in the page (again after a reload) and calls its functions with
// JSON-encoded arguments, so no text reaches the page as code.
package webplayer
