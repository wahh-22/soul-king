package webplayer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// ErrBrowserGone is wrapped by every error caused by the DevTools pipe
// closing, including calls that were pending when it closed.
var ErrBrowserGone = errors.New("webplayer: browser connection closed")

// CDPError is a failure the browser reported for one command. It carries
// the protocol's message only, never its free-form data.
type CDPError struct {
	Method  string
	Code    int
	Message string
}

func (e *CDPError) Error() string {
	return fmt.Sprintf("cdp %s: %s", e.Method, e.Message)
}

// ScriptError is a JavaScript exception thrown by an evaluated expression.
// Description is the exception's first line, restricted to printable ASCII
// and capped (see sanitizeDescription), so stacks and page text stay in
// the page.
type ScriptError struct {
	Description string
}

func (e *ScriptError) Error() string {
	return "webplayer: page script failed: " + e.Description
}

// Event is a DevTools event. Params is the raw event payload; the caller
// decides what, if anything, to decode from it.
type Event struct {
	Method    string
	SessionID string
	Params    json.RawMessage
}

const (
	// defaultMaxMessage bounds one inbound message; larger ones are
	// dropped (a call waiting for one ends with its ctx).
	defaultMaxMessage = 32 << 20
	readBuffer        = 64 << 10
	// attachPollInterval paces Target.getTargets while no page exists.
	attachPollInterval = 100 * time.Millisecond
	// maxDescription caps a ScriptError description, in bytes.
	maxDescription = 120
)

// Client speaks the Chrome DevTools Protocol over the pipe pair of
// --remote-debugging-pipe: every message, in either direction, is one JSON
// object followed by a NUL byte. Responses are matched to calls by id, so
// calls may run concurrently and complete in any order.
type Client struct {
	w          io.Writer
	onEvent    func(Event)
	maxMessage int

	// writeSlot (capacity 1) serializes writes. It is a channel, not a
	// mutex, so a caller waiting behind a stuck write gives up with its
	// ctx (as in the helper client).
	writeSlot chan struct{}

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan reply
	err     error // why the client is unusable, once the reader stopped

	done chan struct{} // closed when the reader stops
}

type reply struct {
	result json.RawMessage
	err    error
}

type wireMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// NewClient starts reading responses and events from r and writes commands
// to w. onEvent may be nil. It runs on the reader goroutine, so it must
// return quickly and must not wait for a Call: the reply could only be
// read after onEvent returns.
func NewClient(r io.Reader, w io.Writer, onEvent func(Event)) *Client {
	c := &Client{
		w:          w,
		onEvent:    onEvent,
		maxMessage: defaultMaxMessage,
		writeSlot:  make(chan struct{}, 1),
		pending:    make(map[int64]chan reply),
		done:       make(chan struct{}),
	}
	go c.readLoop(r)
	return c
}

// Done is closed once the browser's side of the pipe is gone; every later
// Call fails with ErrBrowserGone.
func (c *Client) Done() <-chan struct{} { return c.done }

// Call sends method with params (nil for none) to sessionID ("" for the
// browser target) and waits for the result or ctx. A command the browser
// rejects returns a *CDPError.
func (c *Client) Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("cdp %s: %w", method, err)
	}
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("cdp %s: encode params: %w", method, err)
		}
		raw = b
	}

	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()

	msg, err := json.Marshal(wireMessage{ID: id, Method: method, SessionID: sessionID, Params: raw})
	if err != nil {
		c.forget(id)
		return nil, fmt.Errorf("cdp %s: encode: %w", method, err)
	}
	if err := c.send(ctx, append(msg, 0)); err != nil {
		c.forget(id)
		return nil, fmt.Errorf("cdp %s: send: %w", method, err)
	}

	select {
	case r := <-ch:
		var ce *CDPError
		if errors.As(r.err, &ce) {
			ce.Method = method // responses do not echo the method
		}
		if r.err != nil {
			return nil, r.err
		}
		return r.result, nil
	case <-ctx.Done():
		c.forget(id)
		return nil, fmt.Errorf("cdp %s: %w", method, ctx.Err())
	}
}

// send writes one framed message without letting a browser that stops
// reading block the caller past ctx. The write runs in a goroutine that
// keeps the slot until the message is fully written or the pipe fails, so
// an abandoned write never interleaves with the next one; closing the pipe
// (Browser.Close) wakes it.
func (c *Client) send(ctx context.Context, msg []byte) error {
	select {
	case c.writeSlot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	written := make(chan error, 1)
	go func() {
		defer func() { <-c.writeSlot }()
		_, err := c.w.Write(msg)
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			c.mu.Lock()
			gone := c.err
			c.mu.Unlock()
			if gone != nil {
				return gone
			}
			return fmt.Errorf("%w: %v", ErrBrowserGone, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// readLoop is the only reader of the browser's output. When it stops, it
// fails every pending call and closes done.
func (c *Client) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, readBuffer)
	var err error
	for {
		var msg []byte
		if msg, err = c.readMessage(br); err != nil {
			break
		}
		if msg != nil {
			c.dispatch(msg)
		}
	}
	gone := ErrBrowserGone
	if !errors.Is(err, io.EOF) {
		gone = fmt.Errorf("%w: read: %v", ErrBrowserGone, err)
	}

	c.mu.Lock()
	c.err = gone
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- reply{err: gone}
	}
	close(c.done)
}

// readMessage returns the next message without its NUL, or nil when the
// message exceeded maxMessage and was skipped.
func (c *Client) readMessage(br *bufio.Reader) ([]byte, error) {
	var msg []byte
	tooBig := false
	for {
		chunk, err := br.ReadSlice(0)
		if !tooBig {
			msg = append(msg, chunk...)
			if len(msg) > c.maxMessage+1 {
				tooBig, msg = true, nil
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err != nil:
			return nil, err
		case tooBig:
			return nil, nil
		}
		return msg[:len(msg)-1], nil
	}
}

func (c *Client) dispatch(raw []byte) {
	var m wireMessage
	if json.Unmarshal(raw, &m) != nil {
		return // not a message we can route; nothing waits on it by name
	}
	if m.ID == 0 {
		if m.Method != "" && c.onEvent != nil {
			c.onEvent(Event{Method: m.Method, SessionID: m.SessionID, Params: m.Params})
		}
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[m.ID]
	delete(c.pending, m.ID)
	c.mu.Unlock()
	if !ok { // the caller already gave up (ctx done)
		return
	}
	if m.Error != nil {
		ch <- reply{err: &CDPError{Code: m.Error.Code, Message: m.Error.Message}}
		return
	}
	ch <- reply{result: m.Result}
}

// Page is a flat-mode DevTools session attached to one page target.
type Page struct {
	client    *Client
	sessionID string
	targetID  string
}

// SessionID is the flat-mode session the page's commands are sent to.
func (p *Page) SessionID() string { return p.sessionID }

// TargetID is the attached page target.
func (p *Page) TargetID() string { return p.targetID }

// Client is the connection the page belongs to.
func (p *Page) Client() *Client { return p.client }

// Attach waits for the first "page" target, polling until one exists or
// ctx ends, and attaches to it in flat mode.
func Attach(ctx context.Context, c *Client) (*Page, error) {
	for {
		raw, err := c.Call(ctx, "", "Target.getTargets", nil)
		if err != nil {
			return nil, err
		}
		var r struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
			} `json:"targetInfos"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("cdp Target.getTargets: decode: %w", err)
		}
		for _, ti := range r.TargetInfos {
			if ti.Type == "page" {
				return attach(ctx, c, ti.TargetID)
			}
		}
		timer := time.NewTimer(attachPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("webplayer: no page target: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func attach(ctx context.Context, c *Client, targetID string) (*Page, error) {
	raw, err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true})
	if err != nil {
		return nil, err
	}
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.SessionID == "" {
		return nil, errors.New("cdp Target.attachToTarget: no sessionId in result")
	}
	return &Page{client: c, sessionID: r.SessionID, targetID: targetID}, nil
}

// Evaluate runs expr in the page, awaiting a returned promise, and returns
// its value as JSON. undefined and values JSON cannot carry (NaN, Infinity,
// -0, BigInt) come back as null. A thrown exception is a *ScriptError.
func (p *Page) Evaluate(ctx context.Context, expr string) (json.RawMessage, error) {
	raw, err := p.client.Call(ctx, p.sessionID, "Runtime.evaluate", map[string]any{
		"expression": expr, "awaitPromise": true, "returnByValue": true,
	})
	if err != nil {
		return nil, err
	}
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string          `json:"description"`
				Value       json.RawMessage `json:"value"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("cdp Runtime.evaluate: decode: %w", err)
	}
	if d := r.ExceptionDetails; d != nil {
		desc := d.Text
		if e := d.Exception; e != nil {
			var thrown string
			switch {
			case e.Description != "":
				desc = e.Description
			case json.Unmarshal(e.Value, &thrown) == nil && thrown != "":
				desc = thrown
			}
		}
		return nil, &ScriptError{Description: sanitizeDescription(desc)}
	}
	if len(r.Result.Value) == 0 {
		return json.RawMessage("null"), nil
	}
	return r.Result.Value, nil
}

// sanitizeDescription keeps the first line of a page-provided description,
// restricted to printable ASCII and capped at maxDescription bytes, so
// stacks and other page text do not leak into errors and logs.
func sanitizeDescription(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r >= 0x7f {
			continue
		}
		if b.Len() >= maxDescription {
			b.WriteString("...")
			break
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "error"
	}
	return b.String()
}
