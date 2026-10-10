package webplayer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePeer stands in for the browser on the other end of the two pipes.
type fakePeer struct {
	t   *testing.T
	in  *bufio.Reader // commands written by the client (the browser's fd 3)
	out *os.File      // responses and events read by the client (fd 4)
}

type peerMsg struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	SessionID string          `json:"sessionId"`
	Params    json.RawMessage `json:"params"`
}

func newPair(t *testing.T, onEvent func(Event)) (*Client, *fakePeer) {
	t.Helper()
	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	evR, evW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		evW.Close()
		cmdW.Close()
		cmdR.Close()
		evR.Close()
	})
	return NewClient(evR, cmdW, onEvent), &fakePeer{t: t, in: bufio.NewReader(cmdR), out: evW}
}

func (p *fakePeer) recv() peerMsg {
	p.t.Helper()
	raw, err := p.in.ReadBytes(0)
	if err != nil {
		p.t.Fatalf("peer read: %v", err)
	}
	var m peerMsg
	if err := json.Unmarshal(raw[:len(raw)-1], &m); err != nil {
		p.t.Fatalf("peer decode %q: %v", raw, err)
	}
	return m
}

// send writes all messages in one chunk so the client must split on NUL.
func (p *fakePeer) send(msgs ...string) {
	p.t.Helper()
	if _, err := p.out.WriteString(strings.Join(msgs, "\x00") + "\x00"); err != nil {
		p.t.Fatalf("peer write: %v", err)
	}
}

type callResult struct {
	raw json.RawMessage
	err error
}

func goCall(ctx context.Context, c *Client, session, method string, params any) <-chan callResult {
	ch := make(chan callResult, 1)
	go func() {
		raw, err := c.Call(ctx, session, method, params)
		ch <- callResult{raw, err}
	}()
	return ch
}

func goEvaluate(p *Page, expr string) <-chan callResult {
	ch := make(chan callResult, 1)
	go func() {
		raw, err := p.Evaluate(context.Background(), expr)
		ch <- callResult{raw, err}
	}()
	return ch
}

func wait(t *testing.T, ch <-chan callResult) callResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("call did not return")
		return callResult{}
	}
}

func TestCallFramingRoundTrip(t *testing.T) {
	c, p := newPair(t, nil)
	ch := goCall(context.Background(), c, "", "Browser.getVersion", nil)

	raw, err := p.in.ReadBytes(0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "\x00") != 1 || raw[len(raw)-1] != 0 {
		t.Fatalf("command not framed by a single trailing NUL: %q", raw)
	}
	var m map[string]any
	if err := json.Unmarshal(raw[:len(raw)-1], &m); err != nil {
		t.Fatal(err)
	}
	if m["id"] != float64(1) || m["method"] != "Browser.getVersion" {
		t.Fatalf("unexpected command %v", m)
	}
	if _, ok := m["sessionId"]; ok {
		t.Fatalf("browser-target command must omit sessionId: %v", m)
	}
	if _, ok := m["params"]; ok {
		t.Fatalf("nil params must be omitted: %v", m)
	}
	p.send(`{"id":1,"result":{"product":"Chrome/1"}}`)

	r := wait(t, ch)
	if r.err != nil || string(r.raw) != `{"product":"Chrome/1"}` {
		t.Fatalf("got %s, %v", r.raw, r.err)
	}
}

func TestEventsAndOutOfOrderResponses(t *testing.T) {
	var mu sync.Mutex
	var events []Event
	c, p := newPair(t, func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	first := goCall(context.Background(), c, "", "A.one", nil)
	firstID := p.recv().ID
	second := goCall(context.Background(), c, "S", "B.two", map[string]int{"x": 1})
	m := p.recv()
	if m.ID != firstID+1 || m.SessionID != "S" || string(m.Params) != `{"x":1}` {
		t.Fatalf("unexpected second command %+v", m)
	}

	p.send(`{"method":"Page.loadEventFired","params":{"timestamp":1}}`,
		`{"id":2,"result":{"n":2}}`,
		`not json`,
		`{"id":99,"result":{}}`,
		`{"method":"Target.detachedFromTarget","sessionId":"S","params":{}}`,
		`{"id":1,"result":{"n":1}}`)

	if r := wait(t, second); r.err != nil || string(r.raw) != `{"n":2}` {
		t.Fatalf("second: %s, %v", r.raw, r.err)
	}
	if r := wait(t, first); r.err != nil || string(r.raw) != `{"n":1}` {
		t.Fatalf("first: %s, %v", r.raw, r.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 ||
		events[0].Method != "Page.loadEventFired" || string(events[0].Params) != `{"timestamp":1}` ||
		events[1].Method != "Target.detachedFromTarget" || events[1].SessionID != "S" {
		t.Fatalf("events: %+v", events)
	}
}

func TestCDPErrorBecomesGoError(t *testing.T) {
	c, p := newPair(t, nil)
	ch := goCall(context.Background(), c, "", "Target.attachToTarget", nil)
	p.recv()
	p.send(`{"id":1,"error":{"code":-32602,"message":"No target with given id found","data":"extra detail"}}`)
	r := wait(t, ch)
	var ce *CDPError
	if !errors.As(r.err, &ce) || ce.Code != -32602 || ce.Method != "Target.attachToTarget" ||
		ce.Message != "No target with given id found" {
		t.Fatalf("want CDPError, got %#v", r.err)
	}
	if strings.Contains(r.err.Error(), "extra detail") {
		t.Fatalf("error must carry the message only: %q", r.err)
	}
}

func TestCallTimeoutForgetsPending(t *testing.T) {
	c, p := newPair(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ch := goCall(ctx, c, "", "Slow.call", nil)
	p.recv()
	r := wait(t, ch)
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", r.err)
	}
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending not cleaned up: %d", n)
	}
	// A late response for the abandoned id is ignored without blocking.
	p.send(`{"id":1,"result":{}}`)
	next := goCall(context.Background(), c, "", "Next.call", nil)
	if m := p.recv(); m.ID != 2 {
		t.Fatalf("ids must keep increasing, got %d", m.ID)
	}
	p.send(`{"id":2,"result":{"ok":true}}`)
	if r := wait(t, next); r.err != nil {
		t.Fatal(r.err)
	}
}

func TestCallWithDoneContextSendsNothing(t *testing.T) {
	c, _ := newPair(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Call(ctx, "", "X.y", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
}

func TestReaderEOFFailsPendingAndLaterCalls(t *testing.T) {
	c, p := newPair(t, nil)
	ch := goCall(context.Background(), c, "", "Pending.call", nil)
	p.recv()
	p.out.Close()
	if r := wait(t, ch); !errors.Is(r.err, ErrBrowserGone) {
		t.Fatalf("pending call: want ErrBrowserGone, got %v", r.err)
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed after EOF")
	}
	if _, err := c.Call(context.Background(), "", "After.close", nil); !errors.Is(err, ErrBrowserGone) {
		t.Fatalf("later call: want ErrBrowserGone, got %v", err)
	}
}

func TestOversizedMessageIsDropped(t *testing.T) {
	c, p := newPair(t, nil)
	c.maxMessage = 64
	ch := goCall(context.Background(), c, "", "Big.call", nil)
	p.recv()
	p.send(`{"id":1,"result":{"pad":"`+strings.Repeat("x", 70000)+`"}}`, `{"id":1,"result":{"ok":1}}`)
	if r := wait(t, ch); r.err != nil || string(r.raw) != `{"ok":1}` {
		t.Fatalf("got %s, %v", r.raw, r.err)
	}
}

func TestEvaluateExceptionIsSanitized(t *testing.T) {
	c, p := newPair(t, nil)
	ch := goEvaluate(&Page{client: c, sessionID: "S1"}, "boom()")
	p.recv()
	long := strings.Repeat("x", 500)
	p.send(`{"id":1,"result":{"result":{"type":"object","subtype":"error"},` +
		`"exceptionDetails":{"text":"Uncaught","exception":{"className":"TypeError",` +
		`"description":"TypeError: musickit not ready\u00e9\u0007 ` + long + `\n    at secretPageFunction (https://music.apple.com/x.js:1:2)"}}}}`)
	r := wait(t, ch)
	var se *ScriptError
	if !errors.As(r.err, &se) {
		t.Fatalf("want ScriptError, got %v", r.err)
	}
	if !strings.HasPrefix(se.Description, "TypeError: musickit not ready xxx") || !strings.HasSuffix(se.Description, "...") {
		t.Fatalf("unexpected description: %q", se.Description)
	}
	if strings.Contains(se.Description, "secretPageFunction") || len(se.Description) > 130 {
		t.Fatalf("description leaks page text or is too long (%d): %q", len(se.Description), se.Description)
	}
}

func TestEvaluateExceptionFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name, details, want string
	}{
		{"thrown string", `{"text":"Uncaught","exception":{"type":"string","value":"bad\nsecond line"}}`, "bad"},
		{"text only", `{"text":"Uncaught SyntaxError"}`, "Uncaught SyntaxError"},
		{"nothing printable", `{"text":"\u0001"}`, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p := newPair(t, nil)
			ch := goEvaluate(&Page{client: c, sessionID: "S1"}, "x")
			p.recv()
			p.send(`{"id":1,"result":{"result":{"type":"object"},"exceptionDetails":` + tc.details + `}}`)
			var se *ScriptError
			if r := wait(t, ch); !errors.As(r.err, &se) || se.Description != tc.want {
				t.Fatalf("want %q, got %v", tc.want, r.err)
			}
		})
	}
}

func TestEvaluateUndefinedIsNull(t *testing.T) {
	for _, result := range []string{`{"type":"undefined"}`, `{"type":"number","unserializableValue":"NaN"}`} {
		c, p := newPair(t, nil)
		ch := goEvaluate(&Page{client: c, sessionID: "S1"}, "void 0")
		p.recv()
		p.send(`{"id":1,"result":{"result":` + result + `}}`)
		if r := wait(t, ch); r.err != nil || string(r.raw) != "null" {
			t.Fatalf("%s: got %s, %v", result, r.raw, r.err)
		}
	}
}

func TestAttachThenEvaluateUsesSession(t *testing.T) {
	c, p := newPair(t, nil)
	type attachResult struct {
		page *Page
		raw  json.RawMessage
		err  error
	}
	ch := make(chan attachResult, 1)
	go func() {
		page, err := Attach(context.Background(), c)
		if err != nil {
			ch <- attachResult{err: err}
			return
		}
		raw, err := page.Evaluate(context.Background(), "1+1")
		ch <- attachResult{page, raw, err}
	}()

	// No page yet: Attach must poll again.
	m := p.recv()
	if m.Method != "Target.getTargets" || m.SessionID != "" {
		t.Fatalf("want getTargets, got %+v", m)
	}
	p.send(`{"id":1,"result":{"targetInfos":[{"targetId":"B","type":"browser"}]}}`)
	m = p.recv()
	if m.Method != "Target.getTargets" {
		t.Fatalf("want second getTargets, got %+v", m)
	}
	p.send(`{"id":2,"result":{"targetInfos":[{"targetId":"W","type":"service_worker"},{"targetId":"P1","type":"page"},{"targetId":"P2","type":"page"}]}}`)

	m = p.recv()
	var ap struct {
		TargetID string `json:"targetId"`
		Flatten  bool   `json:"flatten"`
	}
	if err := json.Unmarshal(m.Params, &ap); err != nil || m.Method != "Target.attachToTarget" || ap.TargetID != "P1" || !ap.Flatten {
		t.Fatalf("unexpected attach %+v (%v)", m, err)
	}
	p.send(`{"method":"Target.attachedToTarget","params":{"sessionId":"SESS"}}`, `{"id":3,"result":{"sessionId":"SESS"}}`)

	m = p.recv()
	var ep struct {
		Expression    string `json:"expression"`
		AwaitPromise  bool   `json:"awaitPromise"`
		ReturnByValue bool   `json:"returnByValue"`
	}
	if err := json.Unmarshal(m.Params, &ep); err != nil || m.Method != "Runtime.evaluate" || m.SessionID != "SESS" ||
		ep.Expression != "1+1" || !ep.AwaitPromise || !ep.ReturnByValue {
		t.Fatalf("unexpected evaluate %+v (%v)", m, err)
	}
	p.send(`{"id":4,"sessionId":"SESS","result":{"result":{"type":"number","value":2}}}`)

	select {
	case r := <-ch:
		if r.err != nil || string(r.raw) != "2" {
			t.Fatalf("got %s, %v", r.raw, r.err)
		}
		if r.page.SessionID() != "SESS" || r.page.TargetID() != "P1" {
			t.Fatalf("page ids: %q %q", r.page.SessionID(), r.page.TargetID())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attach/evaluate did not return")
	}
}

func TestAttachStopsWithContext(t *testing.T) {
	c, p := newPair(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() {
		_, err := Attach(ctx, c)
		ch <- err
	}()
	p.recv()
	p.send(`{"id":1,"result":{"targetInfos":[]}}`)
	cancel()
	select {
	case err := <-ch:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Attach did not stop")
	}
}
