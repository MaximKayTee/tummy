// Package blob lists and downloads the files of one Azure Blob Storage container.
package blob

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"tummy/internal/config"
)

const (
	apiVersion = "2021-08-06"
	scope      = "https://storage.azure.com/.default"
	attempts   = 3
)

// Blob is one file in the container.
type Blob struct {
	Name     string
	Size     int64
	Modified time.Time
}

// Client reads one container.
type Client struct {
	name      string // connection, for errors
	container *url.URL
	sas       url.Values
	cred      azcore.TokenCredential
	http      *http.Client

	mu  sync.Mutex
	tok azcore.AccessToken
}

// New makes a client for an expanded azblob connection.
func New(c *config.Conn) (*Client, error) {
	u, err := config.ContainerURL(c.URL)
	if err != nil {
		return nil, fmt.Errorf("conn %s: url: %w", c.Name, err)
	}
	cl := &Client{name: c.Name, container: u, http: &http.Client{}}
	switch c.Auth {
	case "", config.BlobManagedIdentity:
		cl.cred, err = azidentity.NewManagedIdentityCredential(nil)
	case config.BlobAzCLI:
		cl.cred, err = azidentity.NewAzureCLICredential(nil)
	case config.BlobSAS:
		cl.sas, err = url.ParseQuery(strings.TrimPrefix(strings.TrimSpace(c.Token), "?"))
		if err == nil && cl.sas.Get("sig") == "" {
			err = errors.New("token: not a SAS, no sig=")
		}
	default:
		err = fmt.Errorf("auth %q: want %s, %s or %s", c.Auth, config.BlobManagedIdentity, config.BlobAzCLI, config.BlobSAS)
	}
	if err != nil {
		return nil, fmt.Errorf("conn %s: %w", c.Name, err)
	}
	return cl, nil
}

// WithToken replaces the credential with a fixed bearer token source, for tests.
func (c *Client) WithToken(get func(context.Context) (string, error)) *Client {
	c.cred, c.sas = tokenFunc(get), nil
	return c
}

type tokenFunc func(context.Context) (string, error)

func (f tokenFunc) GetToken(ctx context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	t, err := f(ctx)
	return azcore.AccessToken{Token: t, ExpiresOn: time.Now().Add(time.Hour)}, err
}

func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok.Token != "" && time.Until(c.tok.ExpiresOn) > 5*time.Minute {
		return c.tok.Token, nil
	}
	t, err := c.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{scope}})
	if err != nil {
		return "", fmt.Errorf("sign-in: %w", err)
	}
	c.tok = t
	return t.Token, nil
}

// List returns the blobs whose names start with prefix, in name order.
func (c *Client) List(ctx context.Context, prefix string) ([]Blob, error) {
	var out []Blob
	marker := ""
	for {
		q := url.Values{"restype": {"container"}, "comp": {"list"}, "maxresults": {"5000"}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if marker != "" {
			q.Set("marker", marker)
		}
		resp, err := c.get(ctx, "", q)
		if err != nil {
			return nil, err
		}
		var page listResult
		err = xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("conn %s: list: %w", c.name, err)
		}
		for _, b := range page.Blobs {
			m, _ := http.ParseTime(b.Modified)
			out = append(out, Blob{Name: b.Name, Size: b.Size, Modified: m.UTC()})
		}
		if marker = page.NextMarker; marker == "" {
			return out, nil
		}
	}
}

type listResult struct {
	Blobs []struct {
		Name     string `xml:"Name"`
		Size     int64  `xml:"Properties>Content-Length"`
		Modified string `xml:"Properties>Last-Modified"`
	} `xml:"Blobs>Blob"`
	NextMarker string `xml:"NextMarker"`
}

// Download writes the blob to w.
func (c *Client) Download(ctx context.Context, name string, w io.Writer) error {
	resp, err := c.get(ctx, name, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("conn %s: %s: %w", c.name, name, err)
	}
	return nil
}

// get sends a GET for the container (name "") or a blob, retrying 429, 5xx and
// network errors.
func (c *Client) get(ctx context.Context, name string, q url.Values) (*http.Response, error) {
	u := *c.container
	if name != "" {
		segs := strings.Split(name, "/")
		for i, s := range segs {
			segs[i] = url.PathEscape(s)
		}
		u.Path = strings.TrimSuffix(u.Path, "/") + "/" + name
		u.RawPath = strings.TrimSuffix(c.container.EscapedPath(), "/") + "/" + strings.Join(segs, "/")
	}
	all := url.Values{}
	for k, v := range q {
		all[k] = v
	}
	for k, v := range c.sas {
		all[k] = v
	}
	u.RawQuery = all.Encode()
	what := "list"
	if name != "" {
		what = name
	}
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("conn %s: %s: %w", c.name, what, err)
		}
		req.Header.Set("x-ms-version", apiVersion)
		req.Header.Set("User-Agent", "tummy")
		if c.cred != nil {
			t, err := c.token(ctx)
			if err != nil {
				return nil, fmt.Errorf("conn %s: %w", c.name, err)
			}
			req.Header.Set("Authorization", "Bearer "+t)
		}
		resp, err := c.http.Do(req)
		retry := false
		switch {
		case err != nil:
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err // the URL can hold the SAS
			}
			if ctx.Err() != nil || attempt == attempts {
				return nil, fmt.Errorf("conn %s: %s: %w", c.name, what, err)
			}
			retry = true
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			if attempt == attempts {
				return nil, c.httpError(what, resp)
			}
			resp.Body.Close()
			retry = true
		case resp.StatusCode >= 300:
			return nil, c.httpError(what, resp)
		}
		if !retry {
			return resp, nil
		}
		t := time.NewTimer(time.Duration(attempt) * time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// httpError reads Azure's error code and message.
func (c *Client) httpError(what string, resp *http.Response) error {
	defer resp.Body.Close()
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
	code := e.Code
	if code == "" {
		code = resp.Header.Get("x-ms-error-code")
	}
	msg, _, _ := strings.Cut(e.Message, "\n") // the rest is RequestId and Time
	parts := []string{resp.Status}
	for _, s := range []string{code, strings.TrimSpace(msg)} {
		if s != "" {
			parts = append(parts, clean(s))
		}
	}
	return &HTTPError{Status: resp.StatusCode, Code: code, Err: fmt.Errorf("conn %s: %s: %s", c.name, what, strings.Join(parts, ": "))}
}

// HTTPError is a refused request.
type HTTPError struct {
	Status int
	Code   string
	Err    error
}

func (e *HTTPError) Error() string { return e.Err.Error() }

// clean replaces control characters so error text cannot drive a terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
}
