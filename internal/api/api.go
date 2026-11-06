// Package api reads records from REST endpoints.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"tummy/internal/config"
)

const (
	maxPages       = 100000
	attempts       = 6
	maxBackoff     = 30 * time.Second
	maxWait        = 15 * time.Minute // longest Retry-After or rate-limit reset honoured
	defaultTimeout = 2 * time.Minute
	defaultMaxBody = 128 << 20
)

// ErrNoValue: a placeholder field is missing from vars, or empty in the path.
var ErrNoValue = errors.New("no value")

var (
	errRedirect  = errors.New("redirect to another host refused")
	errBodyLimit = errors.New("response too large")
)

// HTTPError is a final non-2xx response.
type HTTPError struct {
	Method string
	URL    string // query token masked
	Status int
	Body   string // first 300 bytes, control characters replaced
}

func (e *HTTPError) Error() string {
	s := fmt.Sprintf("%s %s: HTTP %d", e.Method, e.URL, e.Status)
	if e.Body != "" {
		s += ": " + e.Body
	}
	return s
}

// Client sends the requests of one connection.
type Client struct {
	conn    *config.Conn
	base    *url.URL
	http    *http.Client
	shared  *shared
	retryOn map[int]bool
	headers http.Header
	maxBody int64
	sleep   func(context.Context, time.Duration) error
	now     func() time.Time

	// Defaults Fetch passes as Options. FetchWith ignores them.
	OnPage    func(records int)
	StopAfter func(page []Record) bool
	MaxPages  int
}

// Options are per-call settings for FetchWith.
type Options struct {
	// Vars fill {name.field} placeholders. "watermark" fills {watermark};
	// without it, query params using {watermark} are left out.
	Vars map[string]string
	// OnPage is called after every page with the records fetched so far.
	OnPage func(records int)
	// OnSkip is called when a status in the stream's skip_on ends the fetch.
	OnSkip func(status int)
	// StopAfter is asked after every non-empty page whether to stop paging.
	StopAfter func(page []Record) bool
	// MaxPages caps the requests of one call; 0 means no cap.
	MaxPages int
	// Keys, when set, gets the record keys appended in the order the responses first
	// have them. Pass the same slice to several calls to merge their keys.
	Keys *[]string
}

// shared is per connection and process-wide: rate limit and pauses.
type shared struct {
	limiter *rate.Limiter
	mu      sync.Mutex
	until   time.Time
}

var (
	sharedMu sync.Mutex
	sharedBy = map[string]*shared{}
)

func sharedFor(c *config.Conn) (*shared, error) {
	var lim rate.Limit
	if c.RateLimit != "" {
		l, err := ParseRate(c.RateLimit)
		if err != nil {
			return nil, fmt.Errorf("conn %s: rate_limit: %w", c.Name, err)
		}
		lim = l
	}
	key := c.Name + "\x00" + c.BaseURL + "\x00" + c.RateLimit
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if s, ok := sharedBy[key]; ok {
		return s, nil
	}
	s := &shared{}
	if lim > 0 {
		s.limiter = rate.NewLimiter(lim, 1)
	}
	sharedBy[key] = s
	return s, nil
}

func (s *shared) pause(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.After(s.until) {
		s.until = t
	}
}

func (s *shared) pausedFor(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.until.Sub(now)
}

func NewClient(c *config.Conn) (*Client, error) {
	if c.BaseURL == "" {
		return nil, fmt.Errorf("conn %s: base_url is required", c.Name)
	}
	base, err := url.Parse(c.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("conn %s: base_url must be an http or https URL", c.Name)
	}
	switch c.Auth {
	case "", "none", "bearer", "basic", "header":
	case "query":
		if c.Param == "" {
			return nil, fmt.Errorf("conn %s: auth = query needs param", c.Name)
		}
	default:
		return nil, fmt.Errorf("conn %s: unknown auth %q", c.Name, c.Auth)
	}
	timeout := defaultTimeout
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("conn %s: timeout: want a duration like 30s or 2m", c.Name)
		}
		timeout = d
	}
	headers := http.Header{}
	for k, v := range c.Headers {
		if !validHeader(k, v) {
			return nil, fmt.Errorf("conn %s: headers: invalid header %q", c.Name, k)
		}
		headers.Set(k, v)
	}
	sh, err := sharedFor(c)
	if err != nil {
		return nil, err
	}
	cl := &Client{
		conn:    c,
		base:    base,
		http:    &http.Client{Timeout: timeout, CheckRedirect: checkRedirect},
		shared:  sh,
		retryOn: map[int]bool{},
		headers: headers,
		maxBody: defaultMaxBody,
		sleep:   sleepCtx,
		now:     time.Now,
	}
	codes := c.RetryOn
	if len(codes) == 0 {
		codes = []int{429, 502, 503, 504}
	}
	for _, code := range codes {
		cl.retryOn[code] = true
	}
	return cl, nil
}

func validHeader(k, v string) bool {
	if k == "" || strings.ContainsAny(v, "\r\n\x00") {
		return false
	}
	for _, r := range k {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune(`"(),/:;<=>?@[\]{}`, r) {
			return false
		}
	}
	return true
}

// ParseRate reads "100/m", "10/s", "1000/h".
func ParseRate(s string) (rate.Limit, error) {
	n, unit, ok := strings.Cut(s, "/")
	if !ok {
		return 0, errors.New("want N/s, N/m or N/h")
	}
	count, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
	if err != nil || count <= 0 {
		return 0, errors.New("want N/s, N/m or N/h")
	}
	switch strings.TrimSpace(unit) {
	case "s":
		return rate.Limit(count), nil
	case "m":
		return rate.Limit(count / 60), nil
	case "h":
		return rate.Limit(count / 3600), nil
	}
	return 0, errors.New("want N/s, N/m or N/h")
}

// Fetch runs all requests for one stream with the Client's OnPage, StopAfter and MaxPages.
func (c *Client) Fetch(ctx context.Context, s *config.Stream, vars map[string]string) ([]Record, error) {
	return c.FetchWith(ctx, s, Options{Vars: vars, OnPage: c.OnPage, StopAfter: c.StopAfter, MaxPages: c.MaxPages})
}

// FetchWith runs all requests for one stream and returns its records.
// A status in s.SkipOn ends the fetch with the records so far and no error.
func (c *Client) FetchWith(ctx context.Context, s *config.Stream, o Options) ([]Record, error) {
	recs, err := c.fetch(ctx, s, o)
	return recs, c.scrub(err)
}

func (c *Client) fetch(ctx context.Context, s *config.Stream, o Options) ([]Record, error) {
	cl, err := c.newCall(s, o.Vars)
	if err != nil {
		return nil, err
	}
	var all []Record
	for n := 0; n < maxPages; n++ {
		u, body, err := cl.request()
		if err != nil {
			return nil, err
		}
		resp, data, order, err := c.do(ctx, cl.method, u, body)
		if err != nil {
			var he *HTTPError
			if errors.As(err, &he) && slices.Contains(s.SkipOn, he.Status) {
				if o.OnSkip != nil {
					o.OnSkip(he.Status)
				}
				return all, nil
			}
			return nil, err
		}
		v, err := cl.pick(data)
		if err != nil {
			return nil, err
		}
		recs, keys, err := toRecords(v, order)
		if err != nil {
			return nil, err
		}
		if o.Keys != nil {
			*o.Keys = mergeKeys(*o.Keys, keys)
		}
		if repeat, err := cl.repeated(n, v, len(recs)); err != nil {
			return nil, err
		} else if repeat {
			return all, nil
		}
		all = append(all, recs...)
		if o.OnPage != nil {
			o.OnPage(len(all))
		}
		if o.StopAfter != nil && len(recs) > 0 && o.StopAfter(recs) {
			return all, nil
		}
		if o.MaxPages > 0 && n+1 >= o.MaxPages {
			return all, nil
		}
		more, err := cl.advance(resp, u, data, recs, len(all))
		if err != nil {
			return nil, err
		}
		if !more {
			return all, nil
		}
	}
	return nil, fmt.Errorf("gave up after %d pages", maxPages)
}

// mergeKeys appends the keys of add missing from have.
func mergeKeys(have, add []string) []string {
	seen := make(map[string]bool, len(have))
	for _, k := range have {
		seen[k] = true
	}
	for _, k := range add {
		if !seen[k] {
			seen[k] = true
			have = append(have, k)
		}
	}
	return have
}
