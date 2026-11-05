package db

import (
	"bytes"
	"math"
	"testing"
	"time"
)

func TestCopyText(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{
		{nil, `\N`},
		{"a\tb\nc\r\\d", `a\tb\nc\r\\d`},
		{[]byte{0x01, 0xff}, `\\x01ff`},
		{true, "t"},
		{false, "f"},
		{int64(-7), "-7"},
		{3.25, "3.25"},
		{math.NaN(), "NaN"},
		{math.Inf(-1), "-Infinity"},
		{time.Date(2024, 5, 6, 7, 8, 9, 123000, time.FixedZone("x", 3600)), "2024-05-06 06:08:09.000123"},
		{uint8(3), "3"},
	} {
		var b bytes.Buffer
		copyText(&b, tc.in)
		if b.String() != tc.want {
			t.Errorf("%#v: %q, want %q", tc.in, b.String(), tc.want)
		}
	}
}
