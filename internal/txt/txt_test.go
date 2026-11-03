package txt

import (
	"testing"
	"time"
)

func TestClean(t *testing.T) {
	cases := map[string]string{
		"plain":                "plain",
		"a\x1b[8mb\u009bc\td":  `a\x1b[8mb\x9bc d`,
		"x\x1b]52;c;aGk=\x07y": `x\x1b]52;c;aGk=\x07y`,
		"two\nlines":           "two↵lines",
	}
	for in, want := range cases {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanText("a\r\nb\x1bc"); got != "a\nb\\x1bc" {
		t.Errorf("CleanText: %q", got)
	}
	if got := OneLine("  a \n\t b\x1b[2J "); got != `a b\x1b[2J` {
		t.Errorf("OneLine: %q", got)
	}
	if FirstLine("a\nb") != "a" || FirstLine("a") != "a" {
		t.Error("FirstLine")
	}
}

func TestDurThousands(t *testing.T) {
	if Dur(1500*time.Microsecond) != "2ms" || Dur(2340*time.Millisecond) != "2.3s" || Dur(61*time.Second+400*time.Millisecond) != "1m1s" {
		t.Errorf("Dur: %s %s %s", Dur(1500*time.Microsecond), Dur(2340*time.Millisecond), Dur(61*time.Second+400*time.Millisecond))
	}
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1234: "-1,234"} {
		if got := Thousands(n); got != want {
			t.Errorf("Thousands(%d) = %q, want %q", n, got, want)
		}
	}
}
