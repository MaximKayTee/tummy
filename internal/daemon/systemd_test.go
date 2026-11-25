package daemon

import (
	"strings"
	"testing"
)

func TestUnitQuotes(t *testing.T) {
	u := Unit([]string{"/opt/my tools/tummy", "--home", `/h/100%$x"y\z`, "serve"})
	want := `ExecStart="/opt/my tools/tummy" "--home" "/h/100%%$$x\"y\\z" "serve"`
	if !strings.Contains(u, want+"\n") {
		t.Fatalf("unit:\n%s\nwant line:\n%s", u, want)
	}
}
