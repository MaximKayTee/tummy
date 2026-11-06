package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Bookmark is a saved query on one connection: a table or a query, with steps. Nothing
// is written to the connection.
type Bookmark struct {
	Name  string `toml:"-"`
	Conn  string `toml:"conn"`
	Table string `toml:"table"`
	SQL   string `toml:"sql"`
	Steps string `toml:"steps"`
	// Rep and Stream name the stream whose target table the bookmark was made on.
	Rep    string `toml:"rep"`
	Stream string `toml:"stream"`
}

type Bookmarks struct {
	Bookmark map[string]*Bookmark `toml:"bookmark"`
	Path     string               `toml:"-"`
}

// BookmarksPath is HOME/bookmarks.toml.
func BookmarksPath(home string) string { return filepath.Join(home, "bookmarks.toml") }

// LoadBookmarks reads bookmarks.toml. A missing file is an empty set.
func LoadBookmarks(path string) (*Bookmarks, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Bookmarks{Bookmark: map[string]*Bookmark{}, Path: path}, nil
	}
	if err != nil {
		return nil, err
	}
	var b Bookmarks
	md, err := toml.Decode(string(raw), &b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := unknownBookmarkKeys(md); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	b.Path = path
	if b.Bookmark == nil {
		b.Bookmark = map[string]*Bookmark{}
	}
	for name, bm := range b.Bookmark {
		bm.Name = name
		if err := bm.check(); err != nil {
			return nil, fmt.Errorf("%s: bookmark %s: %w", path, name, err)
		}
	}
	return &b, nil
}

func (b *Bookmark) check() error {
	switch {
	case b.Conn == "":
		return errors.New("conn is required")
	case b.Table == "" && b.SQL == "":
		return errors.New("table or sql is required")
	case b.Table != "" && b.SQL != "":
		return errors.New("table and sql are exclusive")
	}
	return nil
}

func unknownBookmarkKeys(md toml.MetaData) error {
	var msgs []string
	reported := map[string]bool{}
	for _, k := range md.Undecoded() {
		switch {
		case len(k) >= 1 && k[0] != "bookmark":
			if !reported[k[0]] {
				reported[k[0]] = true
				msgs = append(msgs, fmt.Sprintf("%s: bookmarks go under [bookmark.NAME]", k[0]))
			}
		case len(k) >= 3 && !reported[strings.Join(k[:3], ".")]:
			reported[strings.Join(k[:3], ".")] = true
			msgs = append(msgs, fmt.Sprintf("bookmark %s: %s", k[1], unknownKeyMsg(k[2], keysOf(Bookmark{}))))
		}
	}
	if len(msgs) > 0 {
		return errors.New(strings.Join(msgs, "\n"))
	}
	return nil
}

// Names lists the bookmarks, sorted.
func (b *Bookmarks) Names() []string {
	names := make([]string, 0, len(b.Bookmark))
	for n := range b.Bookmark {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// For lists the bookmarks on one connection, sorted by name.
func (b *Bookmarks) For(conn string) []*Bookmark {
	var out []*Bookmark
	for _, n := range b.Names() {
		if bm := b.Bookmark[n]; bm.Conn == conn {
			out = append(out, bm)
		}
	}
	return out
}

// Set adds or replaces a bookmark.
func (b *Bookmarks) Set(bm Bookmark) error {
	if !ValidName(bm.Name) {
		return fmt.Errorf("bookmark name %q: letters, digits, _ and - only", bm.Name)
	}
	if err := bm.check(); err != nil {
		return err
	}
	b.Bookmark[bm.Name] = &bm
	return nil
}

// EditBookmarks loads HOME/bookmarks.toml, applies change and writes it back.
func EditBookmarks(home string, change func(*Bookmarks) error) (*Bookmarks, error) {
	b, err := LoadBookmarks(BookmarksPath(home))
	if err != nil {
		return nil, err
	}
	if err := change(b); err != nil {
		return nil, err
	}
	return b, WriteBookmarks(b.Path, b)
}

// WriteBookmarks writes every bookmark, patching the existing file so comments, blank
// lines and ordering are kept. A new file is created 0600.
func WriteBookmarks(path string, b *Bookmarks) error {
	var sb strings.Builder
	for i, n := range b.Names() {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "[bookmark.%s]\n", key(n))
		for _, f := range fieldsOf(reflect.ValueOf(*b.Bookmark[n])) {
			if !isEmpty(f.v) {
				fmt.Fprintf(&sb, "%s = %s\n", f.key, value(f.v))
			}
		}
	}
	text := sb.String()
	var check Bookmarks
	if _, err := toml.Decode(text, &check); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil {
		patched, err := patchRenamed(string(existing), text, nil)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		text = patched
	}
	return writeFile(path, text)
}
