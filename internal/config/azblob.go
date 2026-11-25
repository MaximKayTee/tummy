package config

import (
	"errors"
	"net/url"
	"strings"
)

// Auth values of an azblob connection. Managed identity is the default.
const (
	BlobManagedIdentity = "managed_identity"
	BlobAzCLI           = "azcli"
	BlobSAS             = "sas"
)

// BlobAuthModes are the auth values of an azblob connection.
var BlobAuthModes = []string{BlobManagedIdentity, BlobAzCLI, BlobSAS}

func (c *Conn) checkBlob(fail func(string, ...any) error, set func(string) bool) error {
	if c.URL == "" {
		return fail("url is required: the container, https://ACCOUNT.blob.core.windows.net/CONTAINER")
	}
	if set(c.URL) {
		if _, err := ContainerURL(c.URL); err != nil {
			return fail("url: %v", err)
		}
	}
	if set(c.Auth) && !contains(BlobAuthModes, c.Auth) {
		return fail("auth %q: want %s", c.Auth, orList(BlobAuthModes))
	}
	if c.Auth == BlobSAS && c.Token == "" {
		return fail("auth = sas needs token")
	}
	return nil
}

// ContainerURL parses an azblob url: https, a host and one path segment, no query.
func ContainerURL(s string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("want https://ACCOUNT.blob.core.windows.net/CONTAINER")
	}
	if u.RawQuery != "" {
		return nil, errors.New("no query: a SAS goes in token")
	}
	p := strings.Trim(u.Path, "/")
	if p == "" || strings.Contains(p, "/") {
		return nil, errors.New("want the container as the only path: https://ACCOUNT.blob.core.windows.net/CONTAINER")
	}
	u.Path = "/" + p
	u.RawPath = ""
	return u, nil
}
