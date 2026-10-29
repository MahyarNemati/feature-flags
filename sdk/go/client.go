// Package flagsdk is the Go client for a feature-flags server.
//
// The design rule is that **the SDK must never be able to break the
// application that embeds it**. A flag system is a dependency of every code
// path; if it can throw, hang or crash, it has made the whole application
// less reliable than it was before flags existed. So:
//
//   - Evaluation is local, against an in-memory snapshot. No network call on
//     the hot path, so a flag check cannot add latency or fail.
//   - The snapshot is refreshed in the background by a stream, with polling
//     as a fallback. A dead server means flags go stale, not down.
//   - Every accessor takes a fallback and returns it on any problem. There is
//     no error return on the hot path, because there is no useful action a
//     caller could take other than using the fallback.
//   - Nothing panics. A malformed flag yields the fallback and a log line.
package flagsdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MahyarNemati/feature-flags/internal/eval"
	"github.com/MahyarNemati/feature-flags/internal/flag"
)

// Context identifies the subject being evaluated.
type Context = eval.Context

// Result is a full evaluation, including the reason.
type Result = eval.Result

// Options configure a Client.
type Options struct {
	// BaseURL of the flag server, e.g. "http://localhost:8080".
	BaseURL string
	// Token is the API token.
	Token string
	// PollInterval is how often to re-read the snapshot. Used as a fallback
	// when streaming is unavailable, and as a backstop when it is.
	PollInterval time.Duration
	// Stream enables the SSE connection. Default true.
	Stream *bool
	// HTTPClient overrides the default client.
	HTTPClient *http.Client
	// Logger receives background errors. Default: slog.Default().
	Logger *slog.Logger
	// InitialTimeout bounds the first snapshot fetch during Connect.
	InitialTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.PollInterval <= 0 {
		o.PollInterval = 60 * time.Second
	}
	if o.Stream == nil {
		t := true
		o.Stream = &t
	}
	if o.HTTPClient == nil {
		// No overall Timeout: it would kill the SSE connection. Per-request
		// deadlines are applied with contexts instead.
		o.HTTPClient = &http.Client{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.InitialTimeout <= 0 {
		o.InitialTimeout = 10 * time.Second
	}
	return o
}

// Client evaluates flags locally against a background-refreshed snapshot.
type Client struct {
	opts Options
	// snapshot is swapped atomically, so evaluation never takes a lock.
	snapshot atomic.Pointer[snapshot]
	// ready is closed once the first snapshot has loaded.
	ready     chan struct{}
	readyOnce sync.Once

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type snapshot struct {
	flags    map[string]*flag.Flag
	revision int64
	fetched  time.Time
}

// New returns a Client. Call Connect to start loading flags.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("flagsdk: BaseURL is required")
	}
	c := &Client{opts: opts.withDefaults(), ready: make(chan struct{})}
	c.snapshot.Store(&snapshot{flags: map[string]*flag.Flag{}})
	return c, nil
}

// Connect fetches the first snapshot and starts background refresh.
//
// A failed first fetch is **not** fatal: it logs and keeps retrying in the
// background, so a flag server that is down at deploy time cannot stop the
// application from starting. Callers who need flags before serving traffic
// should use WaitReady.
func (c *Client) Connect(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.cancel = cancel

	initCtx, initCancel := context.WithTimeout(ctx, c.opts.InitialTimeout)
	err := c.refresh(initCtx)
	initCancel()
	if err != nil {
		c.opts.Logger.Warn("flagsdk: initial snapshot failed; serving fallbacks until it succeeds", "err", err)
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.poll(runCtx)
	}()

	if *c.opts.Stream {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.stream(runCtx)
		}()
	}
	return nil
}

// WaitReady blocks until the first snapshot has loaded or ctx is done.
func (c *Client) WaitReady(ctx context.Context) error {
	select {
	case <-c.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops background work.
func (c *Client) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
	return nil
}

// ---------- evaluation ----------

// Evaluate returns the full result, including the reason.
func (c *Client) Evaluate(key string, ctx Context, fallback any) Result {
	snap := c.snapshot.Load()
	f := snap.flags[key]
	if f == nil {
		return Result{FlagKey: key, Value: fallback, Reason: eval.ReasonFlagNotFound}
	}
	return eval.Evaluate(f, ctx, fallback)
}

// Bool returns a boolean flag's value, or fallback.
func (c *Client) Bool(key string, ctx Context, fallback bool) bool {
	v, ok := c.Evaluate(key, ctx, fallback).Value.(bool)
	if !ok {
		return fallback
	}
	return v
}

// String returns a string flag's value, or fallback.
func (c *Client) String(key string, ctx Context, fallback string) string {
	v, ok := c.Evaluate(key, ctx, fallback).Value.(string)
	if !ok {
		return fallback
	}
	return v
}

// Int returns an integer flag's value, or fallback. JSON numbers arrive as
// float64, so both are accepted.
func (c *Client) Int(key string, ctx Context, fallback int) int {
	switch v := c.Evaluate(key, ctx, fallback).Value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		if v == float64(int64(v)) {
			return int(v)
		}
		return fallback
	default:
		return fallback
	}
}

// JSON unmarshals a JSON flag's value into dst. It returns false and leaves
// dst untouched on any problem, so a caller's zero value or prior contents
// stand in as the fallback.
func (c *Client) JSON(key string, ctx Context, dst any) bool {
	res := c.Evaluate(key, ctx, nil)
	if res.Value == nil {
		return false
	}
	raw, err := json.Marshal(res.Value)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, dst) == nil
}

// Snapshot reports what the client currently knows, for diagnostics.
func (c *Client) Snapshot() (revision int64, flags int, age time.Duration) {
	snap := c.snapshot.Load()
	if snap.fetched.IsZero() {
		return snap.revision, len(snap.flags), 0
	}
	return snap.revision, len(snap.flags), time.Since(snap.fetched)
}

// ---------- background refresh ----------

func (c *Client) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/api/v1/snapshot"), nil)
	if err != nil {
		return err
	}
	c.auth(req)
	if current := c.snapshot.Load(); current.revision > 0 {
		req.Header.Set("If-None-Match", fmt.Sprintf(`"%d"`, current.revision))
	}

	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("snapshot returned %s", resp.Status)
	}

	var body struct {
		Revision int64                 `json:"revision"`
		Flags    map[string]*flag.Flag `json:"flags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	c.apply(body.Flags, body.Revision)
	return nil
}

func (c *Client) apply(flags map[string]*flag.Flag, revision int64) {
	if flags == nil {
		flags = map[string]*flag.Flag{}
	}
	c.snapshot.Store(&snapshot{flags: flags, revision: revision, fetched: time.Now()})
	c.readyOnce.Do(func() { close(c.ready) })
}

func (c *Client) poll(ctx context.Context) {
	t := time.NewTicker(c.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if err := c.refresh(reqCtx); err != nil && ctx.Err() == nil {
				c.opts.Logger.Warn("flagsdk: poll failed; serving the last known snapshot", "err", err)
			}
			cancel()
		}
	}
}

// stream consumes Server-Sent Events, reconnecting with backoff.
func (c *Client) stream(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for ctx.Err() == nil {
		err := c.streamOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.opts.Logger.Debug("flagsdk: stream ended; reconnecting", "err", err, "in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		// Exponential backoff so a restarting server is not hammered by
		// every client at once.
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (c *Client) streamOnce(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/api/v1/stream"), nil)
	if err != nil {
		return err
	}
	c.auth(req)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream returned %s", resp.Status)
	}

	// SSE frames can be large when there are many flags, so give the scanner
	// a generous buffer rather than failing on a long line.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, ":"):
			// A comment, i.e. a keepalive.
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			// Blank line terminates an event.
			if data.Len() == 0 {
				continue
			}
			var body struct {
				Revision int64                 `json:"revision"`
				Flags    map[string]*flag.Flag `json:"flags"`
			}
			if err := json.Unmarshal([]byte(data.String()), &body); err == nil {
				c.apply(body.Flags, body.Revision)
			} else {
				c.opts.Logger.Warn("flagsdk: malformed stream event", "err", err)
			}
			data.Reset()
		}
	}
	return scanner.Err()
}

func (c *Client) url(path string) string {
	return strings.TrimRight(c.opts.BaseURL, "/") + path
}

func (c *Client) auth(req *http.Request) {
	if c.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.opts.Token)
	}
}
