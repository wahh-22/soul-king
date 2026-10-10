package webplayer

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// balanced reports whether the brackets of src, outside its string and
// template literals and comments, are balanced and properly nested. It is
// a structural check, not a parser: it catches a lost brace or quote.
func balanced(src string) (bool, string) {
	var stack []rune
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	runes := []rune(src)
	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; r {
		case '"', '\'', '`':
			j := i + 1
			for ; j < len(runes) && runes[j] != r; j++ {
				if runes[j] == '\\' {
					j++
				} else if runes[j] == '\n' && r != '`' {
					return false, "newline in a string literal at " + strconv.Itoa(j)
				}
			}
			if j >= len(runes) {
				return false, "unterminated literal at " + strconv.Itoa(i)
			}
			i = j
		case '/':
			if i+1 < len(runes) && runes[i+1] == '/' {
				for i < len(runes) && runes[i] != '\n' {
					i++
				}
			}
		case '(', '[', '{':
			stack = append(stack, r)
		case ')', ']', '}':
			if len(stack) == 0 || stack[len(stack)-1] != pairs[r] {
				return false, "unbalanced " + string(r) + " at " + strconv.Itoa(i)
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) != 0 {
		return false, "unclosed " + string(stack)
	}
	return true, ""
}

func TestBootstrapIsStructurallySound(t *testing.T) {
	if ok, why := balanced(bootstrap); !ok {
		t.Errorf("bootstrap: %s", why)
	}
	expr, err := callExpr("play", []string{"111"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := balanced(expr); !ok {
		t.Errorf("call expression: %s", why)
	}
	for _, bad := range []string{"(()", "{]", `"open`, "a}"} {
		if ok, _ := balanced(bad); ok {
			t.Errorf("balanced(%q) = true; the check is too weak", bad)
		}
	}
	if !strings.HasPrefix(bootstrap, "(() => {") || !strings.HasSuffix(bootstrap, "})()") {
		t.Error("bootstrap is not one immediately invoked function")
	}
	if !strings.Contains(bootstrap, "window.__nu11signal = {") || !strings.HasPrefix(namespace, "window.") {
		t.Errorf("bootstrap does not install %s", namespace)
	}
	if v := "const version = " + strconv.Itoa(scriptVersion) + ";"; !strings.Contains(bootstrap, v) {
		t.Errorf("bootstrap lacks %q: its version and the Go side's disagree", v)
	}
}

func TestBootstrapDefinesEveryCall(t *testing.T) {
	for _, fn := range []string{"status", "play", "pause", "resume", "next", "previous", "stop", "seek", "repeat", "volume", "now", "api", "write", "hide"} {
		if !strings.Contains(bootstrap, "\n  "+fn+"(") {
			t.Errorf("bootstrap does not define %s()", fn)
		}
	}
	// The MusicKit surface the spikes proved; renaming any is a live risk.
	for _, use := range []string{
		`"getInstance"`, `"isAuthorized"`, "setQueue({songs: ids})", "setQueue({song: ids[0]})",
		"changeToMediaAtIndex(start)", "mk.play()", "mk.pause()", "skipToNextItem()", "skipToPreviousItem()",
		"mk.stop()", "seekToTime(s)", `"PlayerRepeatMode"`, `"PlaybackStates"`, "mk.repeatMode = c", "mk.volume = level",
		`"nowPlayingItem"`, `"currentPlaybackTime"`, `"currentPlaybackDuration"`, `"playbackState"`,
		`setProperty("display", "none", "important")`, "getAnimations()",
		`read(mk, "api")`, `read(client, "music")`, "music.call(client, path, params)", `read(r, "data")`,
		`const catalogPrefix = "` + catalogPrefix + storefrontPlaceholder + `/";`,
	} {
		if !strings.Contains(bootstrap, use) {
			t.Errorf("bootstrap lacks %s", use)
		}
	}
}

// The page refuses what the Go side refuses: the same exact library
// reads, and nothing else under /v1/me.
func TestBootstrapLibraryAllowlistMatchesGo(t *testing.T) {
	for _, use := range []string{
		`const libraryPlaylists = "` + libraryPlaylistsPath + `";`,
		`const ratingPaths = ["` + ratingsPrefix + `songs", "` + ratingsPrefix + `library-songs"];`,
		`const libraryIDPattern = /^p\.[A-Za-z0-9.]{1,` + strconv.Itoa(playlistIDLimit) + `}$/;`,
		`if (p === libraryPlaylists || ratingPaths.includes(p)) return true;`,
		`(parts.length === 1 || parts.length === 2 && parts[1] === "tracks") && libraryIDPattern.test(parts[0]) && !parts[0].includes("..")`,
		`const pathValid = (p) => typeof p === "string" && (catalogValid(p) || libraryValid(p));`,
	} {
		if !strings.Contains(bootstrap, use) {
			t.Errorf("bootstrap lacks %s", use)
		}
	}
}

func TestBootstrapNeverTouchesSecrets(t *testing.T) {
	lower := strings.ToLower(bootstrap)
	for _, word := range []string{
		"developertoken", "musicusertoken", "token", "cookie", "localstorage", "sessionstorage", "storage",
		"indexeddb", "fetch", "xmlhttprequest", "authorize(", "eval(", "function(",
	} {
		if strings.Contains(lower, word) {
			t.Errorf("bootstrap mentions %q", word)
		}
	}
}

// The api reply on a rejection is a status and a fixed category: the
// error's message, text or the error itself never reaches Go.
func TestBootstrapAPIRepliesNeverCarryTheError(t *testing.T) {
	start := strings.Index(bootstrap, "\n  api(")
	if start < 0 {
		t.Fatal("bootstrap does not define api()")
	}
	body := bootstrap[start:]
	body = body[:strings.Index(body, "\n  },")]
	if !strings.Contains(body, `(e) => ({status: httpStatus(e), error: "request failed"})`) {
		t.Errorf("api() rejection reply changed:\n%s", body)
	}
	for _, leak := range []string{"message", "String(e", "error: e", "stack", "toString", "JSON.stringify"} {
		if strings.Contains(bootstrap, leak) {
			t.Errorf("bootstrap mentions %q", leak)
		}
	}
	if !strings.Contains(bootstrap, `for (const key of ["status", "errorCode"])`) {
		t.Error("httpStatus does not read only status and errorCode")
	}
}

func TestCallExpressionEncodesArguments(t *testing.T) {
	hostile := []string{
		`"); window.__nu11signal.stop(); ("`,
		`'); stop(); //`,
		"`${stop()}`",
		"</script><script>stop()</script>",
		"line\u2028separator\u2029paragraph",
		`back\slash`,
		"\x00\x1b[2J",
	}
	for _, s := range hostile {
		expr, err := callExpr("seek", s, []string{s}, 1.5)
		if err != nil {
			t.Fatalf("callExpr(%q): %v", s, err)
		}
		if strings.Contains(expr, "\u2028") || strings.Contains(expr, "\u2029") {
			t.Errorf("callExpr(%q) carries a raw line separator", s)
		}
		if ok, why := balanced(expr); !ok {
			t.Errorf("callExpr(%q): %s", s, why)
		}
		m := callPattern.FindStringSubmatch(expr)
		if m == nil || m[1] != "seek" {
			t.Fatalf("callExpr(%q) = %s: no single seek call", s, expr)
		}
		var str string
		var list []string
		var num float64
		if err := json.Unmarshal([]byte("["+m[2]+"]"), &[]any{&str, &list, &num}); err != nil {
			t.Fatalf("callExpr(%q): arguments are not JSON: %v", s, err)
		}
		if str != s || len(list) != 1 || list[0] != s || num != 1.5 {
			t.Errorf("callExpr(%q) arguments decode to %q, %q, %v", s, str, list, num)
		}
		// s is passed twice; the call is the only other occurrence.
		if strings.Count(expr, namespace+".") != 1+2*strings.Count(s, namespace+".") {
			t.Errorf("callExpr(%q) calls the namespace more than once", s)
		}
	}
	expr, _ := callExpr("pause")
	want := `if (!n || n.version !== ` + strconv.Itoa(scriptVersion) + `) return {missing: true};`
	if !strings.Contains(expr, want) || !strings.Contains(expr, namespace+".pause()") {
		t.Errorf("callExpr(pause) = %s", expr)
	}
	if _, err := callExpr("seek", func() {}); err == nil {
		t.Error("callExpr with an unencodable argument: no error")
	}
}

// fnBody is the source of the namespace function fn.
func fnBody(t *testing.T, fn string) string {
	t.Helper()
	start := strings.Index(bootstrap, "\n  "+fn+"(")
	if start < 0 {
		t.Fatalf("bootstrap does not define %s()", fn)
	}
	body := bootstrap[start:]
	return body[:strings.Index(body, "\n  },")]
}

// The page refuses the writes the Go side refuses (see writeValid): the
// same methods, paths and ids, and bodies of the same shape. api, the
// reads, never sends a method or a body.
func TestBootstrapWriteAllowlistMatchesGo(t *testing.T) {
	for _, use := range []string{
		`const ratingPattern = /^\/v1\/me\/ratings\/(songs|library-songs)\/([^\/]+)$/;`,
		`const catalogSongPattern = /^\d{1,` + strconv.Itoa(maxRatedCatalogIDDigits) + `}$/;`,
		`const librarySongPattern = /^i\.[A-Za-z0-9.]+$/;`,
		`if (p === libraryPlaylists) return method === "POST" ? "create" : "";`,
		`return method === "POST" && parts.length === 2 && parts[1] === "tracks" && libraryIDPattern.test(parts[0]) && !parts[0].includes("..") ? "add" : "";`,
		`return method === "PUT" ? "love" : method === "DELETE" ? "unlove" : "";`,
		`if (kind === "unlove") return b === null;`,
		`o.type === "rating" && shaped(o.attributes, ["value"], []) && o.attributes.value === 1;`,
		`shaped(o, ["attributes"], ["relationships"]) && shaped(o.attributes, ["name"], ["description"])`,
		`o.attributes.name.trim() !== ""`,
		`t.data.length > 0`,
	} {
		if !strings.Contains(bootstrap, use) {
			t.Errorf("bootstrap lacks %s", use)
		}
	}
	write := fnBody(t, "write")
	for _, use := range []string{
		`const kind = writeKind(method, path);`,
		`if (!kind) throw new Error("write failed: invalid request");`,
		`if (!bodyValid(kind, body)) throw new Error("write failed: invalid body");`,
		// MusicKit's api.music(path, params, options) passes options.method
		// and options.body (an object it serializes) to its request, as
		// observed in music.apple.com; there is no fetchOptions.
		`const options = body === null ? {method} : {method, body: JSON.parse(body)};`,
		`music.call(client, path, {}, options)`,
		`(r) => ({status: okStatus(r), body: read(r, "data")})`,
		`(e) => ({status: httpStatus(e), error: "request failed"})`,
	} {
		if !strings.Contains(write, use) {
			t.Errorf("write() lacks %s:\n%s", use, write)
		}
	}
	// The write is checked before MusicKit is touched.
	if strings.Index(write, "bodyValid") > strings.Index(write, "need()") {
		t.Error("write() reaches MusicKit before checking its request")
	}
	api := fnBody(t, "api")
	if !strings.Contains(api, "music.call(client, path, params))") || strings.Contains(api, "method") || strings.Contains(api, "Options") {
		t.Errorf("api() may send more than a read:\n%s", api)
	}
}
