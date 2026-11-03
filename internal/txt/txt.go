// Package txt formats values for terminal output: control characters, durations, counts.
package txt

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Clean makes one line safe for a terminal: tabs become spaces, newlines ↵, other
// control characters (C0, DEL, C1) print as \xNN.
func Clean(s string) string {
	ok := true
	for _, r := range s {
		if IsControl(r) {
			ok = false
			break
		}
	}
	if ok {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteByte(' ')
		case r == '\n':
			b.WriteString("↵")
		case IsControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// CleanText applies Clean to every line, keeping the line breaks.
func CleanText(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = Clean(l)
	}
	return strings.Join(lines, "\n")
}

// IsControl reports a C0, DEL or C1 control character.
func IsControl(r rune) bool { return r < 0x20 || (r >= 0x7f && r <= 0x9f) }

// OneLine joins all whitespace runs into single spaces and cleans the result.
func OneLine(s string) string { return Clean(strings.Join(strings.Fields(s), " ")) }

// FirstLine returns s up to the first newline.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Dur rounds a duration to what matters at its size.
func Dur(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// Thousands formats n with comma separators.
func Thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if len(s) > 3 {
		var b strings.Builder
		pre := len(s) % 3
		if pre > 0 {
			b.WriteString(s[:pre])
		}
		for i := pre; i < len(s); i += 3 {
			if b.Len() > 0 {
				b.WriteByte(',')
			}
			b.WriteString(s[i : i+3])
		}
		s = b.String()
	}
	if neg {
		return "-" + s
	}
	return s
}
