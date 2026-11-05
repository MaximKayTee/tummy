package db

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// copyRows is the rows an Inserter buffers before one COPY.
const copyRows = 20000

// copyFlush writes the buffered rows with COPY FROM STDIN in text format, on a
// connection taken from the pool for the statement.
func (ins *Inserter) copyFlush(ctx context.Context) error {
	var buf bytes.Buffer
	n := len(ins.cols)
	for r := 0; r < ins.rows; r++ {
		for i, v := range ins.buf[r*n : (r+1)*n] {
			if i > 0 {
				buf.WriteByte('\t')
			}
			copyText(&buf, v)
		}
		buf.WriteByte('\n')
	}
	q := "COPY " + QuoteTable(ins.db.Dialect, ins.table) + " (" + quoteList(ins.db.Dialect, columnNames(ins.cols)) + ") FROM STDIN"
	conn, err := ins.db.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error {
		pc, ok := dc.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("copy: %T is not a pgx connection", dc)
		}
		_, err := pc.Conn().PgConn().CopyFrom(ctx, &buf, q)
		return err
	})
}

// copyText writes v as one COPY text-format field: \N for null, backslash escapes
// for tab, newline, carriage return and backslash.
func copyText(buf *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		buf.WriteString(`\N`)
	case string:
		copyString(buf, x)
	case []byte:
		buf.WriteString(`\\x`)
		buf.WriteString(hex.EncodeToString(x))
	case bool:
		if x {
			buf.WriteByte('t')
		} else {
			buf.WriteByte('f')
		}
	case int64:
		buf.WriteString(strconv.FormatInt(x, 10))
	case int:
		buf.WriteString(strconv.Itoa(x))
	case int32:
		buf.WriteString(strconv.FormatInt(int64(x), 10))
	case float64:
		copyFloat(buf, x)
	case float32:
		copyFloat(buf, float64(x))
	case time.Time:
		buf.WriteString(x.UTC().Format("2006-01-02 15:04:05.999999999"))
	default:
		copyString(buf, fmt.Sprint(v))
	}
}

func copyFloat(buf *bytes.Buffer, f float64) {
	switch {
	case math.IsNaN(f):
		buf.WriteString("NaN")
	case math.IsInf(f, 1):
		buf.WriteString("Infinity")
	case math.IsInf(f, -1):
		buf.WriteString("-Infinity")
	default:
		buf.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	}
}

func copyString(buf *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			buf.WriteString(`\\`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			buf.WriteByte(c)
		}
	}
}
