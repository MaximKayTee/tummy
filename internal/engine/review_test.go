package engine

import (
	"context"
	"strings"
	"testing"

	"tummy/internal/db"
)

func TestSharedTargetFailsRunAndPlan(t *testing.T) {
	f := setup(t)
	r := f.replication(`
source = "SRC"
target = "TGT"
[streams.orders]
object = "Same"
[streams.customers]
object = "same"
`)
	if _, err := f.engine().Run(context.Background(), r, nil); err == nil || !strings.Contains(err.Error(), "orders and customers") {
		t.Fatalf("run: %v", err)
	}
	if f.count("SELECT count(*) FROM sqlite_master WHERE name = 'Same'") != 0 {
		t.Fatal("run wrote before failing")
	}
	if _, err := f.engine().Plan(context.Background(), r); err == nil || !strings.Contains(err.Error(), "orders and customers") {
		t.Fatalf("plan: %v", err)
	}
}

func TestTableKeyPostgresTruncatesAndKeepsCase(t *testing.T) {
	pg, _ := db.DialectFor("postgres")
	d := &db.DB{Dialect: pg, Schema: "out"}
	long := strings.Repeat("x", 63)
	if d.TableKey(db.Table{Name: long + "_2019"}) != d.TableKey(db.Table{Schema: "out", Name: long + "_2020"}) {
		t.Fatal("names past 63 bytes should be one table")
	}
	if d.TableKey(db.Table{Name: "Audit"}) == d.TableKey(db.Table{Name: "audit"}) {
		t.Fatal("quoted postgres names keep case")
	}
	if got := d.TableKey(db.Table{Name: strings.Repeat("x", 62) + "é"}); strings.HasSuffix(got, "\xc3") {
		t.Fatal("cut inside a character")
	}
	ms, _ := db.DialectFor("mssql")
	m := &db.DB{Dialect: ms}
	if m.TableKey(db.Table{Name: "Audit"}) != m.TableKey(db.Table{Schema: "DBO", Name: "audit"}) {
		t.Fatal("mssql names ignore case")
	}
}
