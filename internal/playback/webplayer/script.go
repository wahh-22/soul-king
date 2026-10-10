package webplayer

import (
	"encoding/json"
	"strconv"
	"strings"
)

// namespace is where bootstrap installs the player's functions in the
// page.
const namespace = "window.__nu11signal"

// scriptVersion tells a namespace this bootstrap installed from an older
// one (or anything else at that name): a call that finds another version
// reports the namespace missing, and the player installs it again.
const scriptVersion = 4

// bootstrap installs the namespace in the page, replacing any earlier
// one; it is idempotent, and installed again after the page reloads. Its
// functions read and drive only the public MusicKit instance
// (MusicKit.getInstance()) and return plain JSON. Statuses and repeat
// modes are matched against MusicKit's own named constants, never assumed
// values. Failures throw short messages of ours (an SDK error contributes
// only its errorCode or name), so page text does not leak through
// ScriptError. api asks MusicKit's own API client for a catalog path or
// one of the library reads (see libraryPathValid; the Go side validates
// it too) and answers {status: 200, body: data},
// or {status, error} with the HTTP status the client's error carries (0
// without one) and a fixed category, never the error itself. write sends
// one of the library edits (see writeValid; it is checked here too: the
// method and path, and the body's shape) through the same client, as
// music.apple.com's MusicKit takes it (observed live: api.music(path,
// params, {method, body}) with the body an object it serializes), so
// MusicKit adds the credentials. It answers as api does, with the 2xx
// status the client reports (200 when it reports none). The api reads
// never accept a write's method or path. It never
// reads tokens, cookies or storage.
const bootstrap = `(() => {
const version = 4;
const songPattern = /^\d{1,20}$/;
const textLimit = 200;
const statuses = [
  ["playing", "playing"], ["paused", "paused"],
  ["stopped", "stopped"], ["ended", "stopped"], ["completed", "stopped"], ["none", "stopped"],
  ["seeking", "seeking"], ["loading", "loading"], ["waiting", "loading"], ["stalled", "loading"]
];
const repeats = [["off", "none"], ["one", "one"], ["all", "all"]];
const catalogPrefix = "/v1/catalog/{{storefrontId}}/";
const libraryPlaylists = "/v1/me/library/playlists";
const ratingPaths = ["/v1/me/ratings/songs", "/v1/me/ratings/library-songs"];
const libraryIDPattern = /^p\.[A-Za-z0-9.]{1,64}$/;
const segmentPattern = /^[A-Za-z0-9.-]+$/;
const keyPattern = /^[A-Za-z0-9._\[\]-]{1,64}$/;
const statusPattern = /^\d{3}$/;
const ratingPattern = /^\/v1\/me\/ratings\/(songs|library-songs)\/([^\/]+)$/;
const catalogSongPattern = /^\d{1,12}$/;
const librarySongPattern = /^i\.[A-Za-z0-9.]+$/;
const read = (object, key) => { try { return object == null ? undefined : object[key]; } catch (e) { return undefined; } };
const text = (object, key) => { const v = read(object, key); return typeof v === "string" ? v : undefined; };
const clip = (v) => typeof v === "string" ? v.slice(0, textLimit) : "";
const seconds = (v) => typeof v === "number" && Number.isFinite(v) && v >= 0 ? v : null;
const instance = () => {
  const kit = read(window, "MusicKit");
  const get = read(kit, "getInstance");
  if (typeof get !== "function") return null;
  try { return get.call(kit) || null; } catch (e) { return null; }
};
const need = () => { const mk = instance(); if (!mk) throw new Error("musickit not ready"); return mk; };
const fail = (op, e) => {
  const code = read(e, "errorCode"), name = read(e, "name");
  const why = typeof code === "string" || typeof code === "number" ? String(code) : typeof name === "string" ? name : "error";
  return new Error(op + " failed: " + why);
};
const run = async (op, act) => { const mk = need(); try { await act(mk); } catch (e) { throw fail(op, e); } return {ok: true}; };
const constant = (group, name) => { const v = read(read(read(window, "MusicKit"), group), name); return v === undefined || v === null ? undefined : v; };
const repeatOf = (value) => { for (const [mode, name] of repeats) { const c = constant("PlayerRepeatMode", name); if (c !== undefined && c === value) return mode; } return ""; };
const catalogValid = (p) => p.startsWith(catalogPrefix) &&
  p.slice(catalogPrefix.length).split("/").every((s) => segmentPattern.test(s) && s !== "." && s !== "..");
const libraryValid = (p) => {
  if (p === libraryPlaylists || ratingPaths.includes(p)) return true;
  if (!p.startsWith(libraryPlaylists + "/")) return false;
  const parts = p.slice(libraryPlaylists.length + 1).split("/");
  return (parts.length === 1 || parts.length === 2 && parts[1] === "tracks") && libraryIDPattern.test(parts[0]) && !parts[0].includes("..");
};
const pathValid = (p) => typeof p === "string" && (catalogValid(p) || libraryValid(p));
const paramsValid = (q) => q !== null && typeof q === "object" && !Array.isArray(q) &&
  Object.keys(q).every((k) => { const v = q[k]; return keyPattern.test(k) && (typeof v === "string" || typeof v === "number" && Number.isFinite(v)); });
const songIDValid = (type, id) => typeof id === "string" && !id.includes("..") &&
  (type === "songs" ? catalogSongPattern.test(id) : type === "library-songs" && librarySongPattern.test(id));
const writeKind = (method, p) => {
  if (typeof method !== "string" || typeof p !== "string") return "";
  if (p === libraryPlaylists) return method === "POST" ? "create" : "";
  if (p.startsWith(libraryPlaylists + "/")) {
    const parts = p.slice(libraryPlaylists.length + 1).split("/");
    return method === "POST" && parts.length === 2 && parts[1] === "tracks" && libraryIDPattern.test(parts[0]) && !parts[0].includes("..") ? "add" : "";
  }
  const m = ratingPattern.exec(p);
  if (!m || !songIDValid(m[1], m[2])) return "";
  return method === "PUT" ? "love" : method === "DELETE" ? "unlove" : "";
};
const shaped = (o, required, optional) => o !== null && typeof o === "object" && !Array.isArray(o) &&
  Object.keys(o).every((k) => required.includes(k) || optional.includes(k)) && required.every((k) => read(o, k) !== undefined);
const tracksValid = (t) => shaped(t, ["data"], []) && Array.isArray(t.data) && t.data.length > 0 &&
  t.data.every((s) => shaped(s, ["id", "type"], []) && songIDValid(s.type, s.id));
const bodyValid = (kind, b) => {
  if (kind === "unlove") return b === null;
  if (typeof b !== "string") return false;
  let o;
  try { o = JSON.parse(b); } catch (e) { return false; }
  if (kind === "love") return shaped(o, ["attributes", "type"], []) && o.type === "rating" && shaped(o.attributes, ["value"], []) && o.attributes.value === 1;
  if (kind === "add") return tracksValid(o);
  return kind === "create" && shaped(o, ["attributes"], ["relationships"]) && shaped(o.attributes, ["name"], ["description"]) &&
    typeof o.attributes.name === "string" && o.attributes.name.trim() !== "" &&
    (o.attributes.description === undefined || typeof o.attributes.description === "string") &&
    (o.relationships === undefined || shaped(o.relationships, ["tracks"], []) && tracksValid(o.relationships.tracks));
};
const httpStatus = (e) => {
  for (const key of ["status", "errorCode"]) {
    const v = read(e, key);
    const n = typeof v === "number" ? v : typeof v === "string" && statusPattern.test(v) ? Number(v) : NaN;
    if (Number.isInteger(n) && n >= 100 && n <= 599) return n;
  }
  return 0;
};
const okStatus = (r) => {
  const n = read(r, "status");
  return Number.isInteger(n) && n >= 200 && n <= 299 ? n : 200;
};
const statusOf = (value) => { for (const [name, status] of statuses) { const c = constant("PlaybackStates", name); if (c !== undefined && c === value) return status; } return "unknown"; };
window.__nu11signal = {
  version,
  status() { const mk = instance(); return {ready: !!mk, authorized: !!mk && read(mk, "isAuthorized") === true}; },
  play(ids, start) {
    if (!Array.isArray(ids) || ids.length === 0 || !ids.every((id) => typeof id === "string" && songPattern.test(id))) throw new Error("play failed: invalid queue");
    if (!Number.isInteger(start) || start < 0 || start >= ids.length) throw new Error("play failed: invalid start");
    return run("play", async (mk) => {
      if (ids.length === 1) {
        await mk.setQueue({song: ids[0]});
      } else {
        await mk.setQueue({songs: ids});
        if (start > 0) await mk.changeToMediaAtIndex(start);
      }
      await mk.play();
    });
  },
  pause() { return run("pause", (mk) => mk.pause()); },
  resume() { return run("resume", (mk) => mk.play()); },
  next() { return run("next", (mk) => mk.skipToNextItem()); },
  previous() { return run("previous", (mk) => mk.skipToPreviousItem()); },
  stop() { return run("stop", (mk) => mk.stop()); },
  seek(s) {
    if (typeof s !== "number" || !Number.isFinite(s) || s < 0) throw new Error("seek failed: invalid position");
    return run("seek", (mk) => mk.seekToTime(s));
  },
  repeat(mode) {
    const name = (repeats.find(([m]) => m === mode) || [])[1];
    const c = name === undefined ? undefined : constant("PlayerRepeatMode", name);
    if (c === undefined) throw new Error("repeat failed: unknown mode");
    return run("repeat", (mk) => { mk.repeatMode = c; });
  },
  volume(level) {
    if (typeof level !== "number" || !Number.isFinite(level) || level < 0 || level > 1) throw new Error("volume failed: invalid level");
    return run("volume", (mk) => { mk.volume = level; });
  },
  now() {
    const mk = instance();
    if (!mk) return {status: "unknown"};
    const item = read(mk, "nowPlayingItem"), attributes = read(item, "attributes");
    const id = text(item, "id");
    const volume = read(mk, "volume");
    return {
      status: statusOf(read(mk, "playbackState")),
      title: clip(text(item, "title") || text(attributes, "name")),
      artist: clip(text(item, "artistName") || text(attributes, "artistName")),
      album: clip(text(item, "albumName") || text(attributes, "albumName")),
      songID: id !== undefined && songPattern.test(id) ? id : "",
      positionS: seconds(read(mk, "currentPlaybackTime")),
      durationS: seconds(read(mk, "currentPlaybackDuration")),
      repeat: repeatOf(read(mk, "repeatMode")),
      volume: typeof volume === "number" && Number.isFinite(volume) && volume >= 0 && volume <= 1 ? volume : null
    };
  },
  api(path, params) {
    if (!pathValid(path)) throw new Error("api failed: invalid path");
    if (!paramsValid(params)) throw new Error("api failed: invalid params");
    const mk = need(), client = read(mk, "api"), music = read(client, "music");
    if (typeof music !== "function") throw new Error("api failed: no api client");
    return Promise.resolve().then(() => music.call(client, path, params)).then(
      (r) => ({status: 200, body: read(r, "data")}),
      (e) => ({status: httpStatus(e), error: "request failed"}));
  },
  write(method, path, body) {
    const kind = writeKind(method, path);
    if (!kind) throw new Error("write failed: invalid request");
    if (!bodyValid(kind, body)) throw new Error("write failed: invalid body");
    const mk = need(), client = read(mk, "api"), music = read(client, "music");
    if (typeof music !== "function") throw new Error("write failed: no api client");
    const options = body === null ? {method} : {method, body: JSON.parse(body)};
    return Promise.resolve().then(() => music.call(client, path, {}, options)).then(
      (r) => ({status: okStatus(r), body: read(r, "data")}),
      (e) => ({status: httpStatus(e), error: "request failed"}));
  },
  hide() {
    const root = document.documentElement;
    if (root) root.style.setProperty("display", "none", "important");
    try { for (const a of document.getAnimations()) a.cancel(); } catch (e) {}
    return {hidden: true};
  }
};
return {installed: true};
})()`

// callExpr is the expression that calls fn of the namespace with args,
// each JSON-encoded (JSON is valid JavaScript, and json.Marshal escapes
// U+2028 and U+2029), so no argument can change the expression. It
// answers {"missing": true} when the page has no namespace of this
// version (it reloaded), and {"value": …} with fn's answer otherwise.
func callExpr(fn string, args ...any) (string, error) {
	encoded := make([]string, len(args))
	for i, a := range args {
		b, err := json.Marshal(a)
		if err != nil {
			return "", err
		}
		encoded[i] = string(b)
	}
	return `(() => { const n = ` + namespace + `; if (!n || n.version !== ` + strconv.Itoa(scriptVersion) + `) return {missing: true}; ` +
		`return Promise.resolve(` + namespace + `.` + fn + `(` + strings.Join(encoded, ",") + `)).then((v) => ({value: v === undefined ? null : v})); })()`, nil
}
