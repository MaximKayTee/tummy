package api

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// do sends one request with auth, rate limiting and retries.
func (c *Client) do(ctx context.Context, method string, u *url.URL, body []byte) (*http.Response, any, keyOrder, error) {
	authed := sameOrigin(u, c.base)
	u = c.withToken(u, authed)
	shown := c.redact(u)
	for attempt := 1; ; attempt++ {
		if err := c.wait(ctx); err != nil {
			return nil, nil, nil, err
		}
		resp, raw, err := c.send(ctx, method, u, body, authed)
		var d time.Duration
		switch {
		case err != nil:
			if attempt == attempts || ctx.Err() != nil || !transient(err) {
				return nil, nil, nil, fmt.Errorf("%s %s: %w", method, shown, err)
			}
			d = backoff(attempt)
		case c.retry(resp):
			herr := c.httpError(method, shown, resp, raw)
			if attempt == attempts {
				return nil, nil, nil, fmt.Errorf("giving up after %d attempts: %w", attempts, herr)
			}
			wait, server := retryAfter(resp.Header, c.now())
			if !server {
				d = backoff(attempt)
				break
			}
			if wait > maxWait {
				return nil, nil, nil, fmt.Errorf("%w: server asks to wait %s", herr, wait.Round(time.Second))
			}
			c.shared.pause(c.now().Add(wait))
			continue
		case resp.StatusCode >= 300:
			return resp, nil, nil, c.httpError(method, shown, resp, raw)
		default:
			if wait, ok := resetWait(resp.Header, c.now()); ok && wait > 0 {
				c.shared.pause(c.now().Add(wait))
			}
			data, order, err := decode(raw)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("%s %s: not JSON: %w", method, shown, err)
			}
			return resp, data, order, nil
		}
		if err := c.sleep(ctx, d); err != nil {
			return nil, nil, nil, err
		}
	}
}

// wait honours pauses set by rate-limit headers, then the rate limit.
func (c *Client) wait(ctx context.Context) error {
	if d := c.shared.pausedFor(c.now()); d > 0 {
		if d > maxWait {
			return fmt.Errorf("conn %s: rate limited for %s more", c.conn.Name, d.Round(time.Second))
		}
		if err := c.sleep(ctx, d); err != nil {
			return err
		}
	}
	if c.shared.limiter != nil {
		return c.shared.limiter.Wait(ctx)
	}
	return nil
}

func (c *Client) send(ctx context.Context, method string, u *url.URL, body []byte, authed bool) (*http.Response, []byte, error) {
	var rb io.Reader
	if body != nil {
		rb = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rb)
	if err != nil {
		return nil, nil, stripURL(err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tummy")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authed {
		for k, v := range c.headers {
			req.Header[k] = v
		}
		switch c.conn.Auth {
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+c.conn.Token)
		case "basic":
			req.SetBasicAuth(c.conn.User, c.conn.Password)
		case "header":
			req.Header.Set(cmp.Or(c.conn.Header, "X-Api-Key"), c.conn.Token)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, stripURL(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(raw)) > c.maxBody {
		return nil, nil, fmt.Errorf("%w: over %d bytes", errBodyLimit, c.maxBody)
	}
	return resp, raw, nil
}

func (c *Client) retry(resp *http.Response) bool {
	if c.retryOn[resp.StatusCode] {
		return true
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		_, limited := retryAfter(resp.Header, c.now())
		return limited
	}
	return false
}

func (c *Client) httpError(method, shown string, resp *http.Response, raw []byte) *HTTPError {
	return &HTTPError{Method: method, URL: shown, Status: resp.StatusCode, Body: c.scrubText(clean(truncate(string(raw), 300)))}
}

// keyOrder is the key order of each decoded JSON object, by map identity.
type keyOrder map[uintptr][]string

func (o keyOrder) of(m map[string]any) []string {
	if o == nil {
		return nil
	}
	return o[reflect.ValueOf(m).Pointer()]
}

const maxDepth = 10000

// decode reads the first JSON value of raw, numbers as json.Number, and the order of
// each object's keys.
func decode(raw []byte) (any, keyOrder, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	order := keyOrder{}
	v, err := decodeValue(dec, order, 0)
	if err != nil {
		return nil, nil, err
	}
	return v, order, nil
}

func decodeValue(dec *json.Decoder, order keyOrder, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("nested too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch d {
	case '{':
		m := map[string]any{}
		var keys []string
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, _ := kt.(string)
			v, err := decodeValue(dec, order, depth+1)
			if err != nil {
				return nil, err
			}
			if _, dup := m[k]; !dup {
				keys = append(keys, k)
			}
			m[k] = v
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		order[reflect.ValueOf(m).Pointer()] = keys
		return m, nil
	case '[':
		a := []any{}
		for dec.More() {
			v, err := decodeValue(dec, order, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return a, nil
	}
	return nil, fmt.Errorf("unexpected %v", d)
}

// withToken adds the query token for base_url's host and removes it elsewhere.
func (c *Client) withToken(u *url.URL, authed bool) *url.URL {
	if c.conn.Auth != "query" {
		return u
	}
	r := *u
	q := r.Query()
	if authed {
		q.Set(c.conn.Param, c.conn.Token)
	} else if q.Has(c.conn.Param) {
		q.Del(c.conn.Param)
	} else {
		return u
	}
	r.RawQuery = q.Encode()
	return &r
}

// redact masks the query token and any password in u.
func (c *Client) redact(u *url.URL) string {
	r := *u
	if c.conn.Auth == "query" {
		q := r.Query()
		if q.Has(c.conn.Param) {
			q.Set(c.conn.Param, "xxxxx")
			r.RawQuery = q.Encode()
		}
	}
	return r.Redacted()
}

// scrub masks the token and password anywhere in err's text.
func (c *Client) scrub(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if out := c.scrubText(msg); out != msg {
		return &scrubbed{msg: out, err: err}
	}
	return err
}

func (c *Client) scrubText(s string) string {
	for _, secret := range []string{c.conn.Token, c.conn.Password} {
		if len(secret) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, secret, "xxxxx")
		if e := url.QueryEscape(secret); e != secret {
			s = strings.ReplaceAll(s, e, "xxxxx")
		}
	}
	return s
}

type scrubbed struct {
	msg string
	err error
}

func (e *scrubbed) Error() string { return e.msg }
func (e *scrubbed) Unwrap() error { return e.err }

// stripURL drops the URL that *url.Error carries; callers print a redacted one.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !sameOrigin(req.URL, via[0].URL) {
		return fmt.Errorf("%w: %s://%s", errRedirect, req.URL.Scheme, req.URL.Host)
	}
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(hostPort(a), hostPort(b))
}

func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[strings.ToLower(u.Scheme)]
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// transient reports whether a transport error is worth retrying.
func transient(err error) bool {
	var dns *net.DNSError
	var verify *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	switch {
	case errors.As(err, &dns) && dns.IsNotFound,
		errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &host),
		errors.As(err, &invalid), errors.As(err, &record),
		errors.Is(err, errRedirect), errors.Is(err, errBodyLimit):
		return false
	}
	return true
}

// retryAfter reads Retry-After (seconds, fractional seconds or HTTP date),
// else a rate-limit reset with nothing remaining.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
			return seconds(secs), true
		}
		if t, err := http.ParseTime(v); err == nil {
			return max(t.Sub(now), 0), true
		}
	}
	return resetWait(h, now)
}

// resetWait reads X-RateLimit-Remaining 0 with X-RateLimit-Reset as epoch
// seconds, epoch milliseconds or seconds from now.
func resetWait(h http.Header, now time.Time) (time.Duration, bool) {
	for _, p := range []string{"X-RateLimit-", "X-Rate-Limit-", "RateLimit-"} {
		if strings.TrimSpace(h.Get(p+"Remaining")) != "0" {
			continue
		}
		r, err := strconv.ParseFloat(strings.TrimSpace(h.Get(p+"Reset")), 64)
		if err != nil || r < 0 || r > 1e15 {
			continue
		}
		switch {
		case r > 1e12:
			return max(time.UnixMilli(int64(r)).Sub(now), 0), true
		case r > 1e9:
			return max(time.Unix(int64(r), 0).Sub(now), 0), true
		default:
			return seconds(r), true
		}
	}
	return 0, false
}

// seconds converts with a ceiling of one day.
func seconds(s float64) time.Duration {
	return time.Duration(min(s, 86400) * float64(time.Second))
}

// backoff doubles from 1s up to maxBackoff, with jitter in [d/2, d).
func backoff(attempt int) time.Duration {
	d := min(time.Second<<min(attempt-1, 10), maxBackoff)
	return d/2 + rand.N(d/2)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// clean replaces control characters so error text cannot drive a terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
