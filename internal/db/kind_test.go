package db

import (
	"strings"
	"testing"
	"time"
)

func TestKindFromTypeName(t *testing.T) {
	cases := map[string]Kind{
		"INT4": Int, "INT8": Int, "BIGINT": Int, "UNSIGNED BIGINT": Int, "UNSIGNED BIG INT": Int, "INTEGER": Int, "SERIAL": Int, "TINYINT": Int,
		"_INT4": String, "_TEXT": String, "INT4RANGE": String, "TSTZRANGE": String, "INT4MULTIRANGE": String, "POINT": String, "INTERVAL": String,
		"TEXT": String, "VARCHAR": String, "NVARCHAR": String, "BPCHAR": String, "UUID": String, "UNIQUEIDENTIFIER": String, "TIME": String, "": String, "16384": String,
		"NUMERIC": Decimal, "DECIMAL": Decimal, "DECIMAL(10,2)": Decimal, "MONEY": Decimal,
		"FLOAT4": Float, "FLOAT8": Float, "DOUBLE": Float, "REAL": Float, "DOUBLE PRECISION": Float,
		"BOOL": Bool, "BOOLEAN": Bool, "BIT": Bool,
		"TIMESTAMP": Timestamp, "TIMESTAMPTZ": Timestamp, "DATETIME": Timestamp, "DATETIME2": Timestamp, "DATETIMEOFFSET": Timestamp, "SMALLDATETIME": Timestamp,
		"DATE": Date, "JSON": JSON, "JSONB": JSON,
		"BYTEA": Bytes, "BLOB": Bytes, "VARBINARY": Bytes, "LONGBLOB": Bytes, "IMAGE": Bytes,
	}
	for name, want := range cases {
		if got := kindFromTypeName(name); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	ms, _ := DialectFor("mssql")
	if ms.KindOf("TIMESTAMP") != Bytes {
		t.Error("mssql timestamp is rowversion")
	}
}

func TestNormalize(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skip(err)
	}
	summer := time.Date(2026, 7, 1, 13, 0, 0, 0, london) // 12:00Z
	got := normalize(summer, Column{Kind: Timestamp}).(time.Time)
	if got.Location() != time.UTC || got.Hour() != 12 {
		t.Fatalf("time: %v", got)
	}
	g := []byte{0x78, 0x56, 0x34, 0x12, 0x34, 0x12, 0x34, 0x12, 0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0}
	if s := normalize(g, Column{Kind: String, Type: "UNIQUEIDENTIFIER"}); s != "12345678-1234-1234-1234-56789ABCDEF0" {
		t.Fatalf("guid: %v", s)
	}
	if b := normalize([]byte{1}, Column{Kind: Bool, Type: "BIT"}); b != true {
		t.Fatalf("bit: %v", b)
	}
	if b := normalize(int64(0), Column{Kind: Bool, Type: "BOOLEAN"}); b != false {
		t.Fatalf("sqlite bool: %v", b)
	}
	if s := normalize([]byte("x"), Column{Kind: String}); s != "x" {
		t.Fatalf("text: %v", s)
	}
	// go-mssqldb reads time as 0001-01-01 plus the time of day; pgx cannot bind it to text
	if s := normalize(time.Date(1, 1, 1, 10, 30, 15, 123456700, time.UTC), Column{Kind: String, Type: "TIME"}); s != "10:30:15.1234567" {
		t.Fatalf("mssql time: %v", s)
	}
}

func TestMySQLDSNHidesPassword(t *testing.T) {
	_, err := mysqlDSN("mysql://u:p%zzsecret@h/db")
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "p%zz") {
		t.Fatalf("%v", err)
	}
	dsn, err := mysqlDSN("mysql://u:pw@h/db?tls=true")
	if err != nil || dsn != "u:pw@tcp(h:3306)/db?parseTime=true&tls=true" {
		t.Fatalf("%q %v", dsn, err)
	}
}
