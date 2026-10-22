//go:build integration

package types_test

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzir/quackdriver/internal/livetest"
)

// TestConstantVectors checks literals, which DuckDB ships as CONSTANT
// vectors, for every kind of type, across chunk boundaries.
func TestConstantVectors(t *testing.T) {
	e := newEnv(t)
	exprs := append([]string{}, nestedElementExprs...)
	exprs = append(exprs, "[1, NULL]", "{'a': [1], 'b': NULL}", "MAP {'k': 1}", "row(1, 'x')", "NULL::INTEGER", "NULL::VARCHAR[]",
		"union_value(s := 'x')::UNION(n INTEGER, s VARCHAR)", "array_value(1, 2)")
	var cols []string
	for i, x := range exprs {
		cols = append(cols, fmt.Sprintf("%s AS c%d", x, i))
	}
	q := "SELECT " + strings.Join(cols, ", ") + " FROM range(5000)"
	e.reset()
	rows := e.rows(t, q)
	if len(rows) != 5000 {
		t.Fatalf("got %d rows", len(rows))
	}
	kinds := e.columnKinds(t)
	if kinds["constantColumn"] == 0 {
		t.Errorf("expected CONSTANT vectors, got %v", kinds)
	}
	first := e.rows(t, "SELECT "+strings.Join(cols, ", "))[0]
	for _, r := range []int{0, 2047, 2048, 4999} {
		for c := range exprs {
			if !equalValues(rows[r][c], first[c]) {
				t.Errorf("row %d %s: %#v, single-row value %#v", r, exprs[c], rows[r][c], first[c])
			}
		}
	}
}

// TestSequenceAndFlatIntegers decodes range() output, multi-batch, with every
// integer width.
func TestSequenceAndFlatIntegers(t *testing.T) {
	e := newEnv(t, "inline_rows=100")
	const n = 100000
	e.reset()
	rows := e.rows(t, fmt.Sprintf(`SELECT range, range::INTEGER, (range %% 30000)::SMALLINT, (range %% 100)::TINYINT,
		range::UBIGINT, range::HUGEINT FROM range(%d)`, n))
	if len(rows) != n {
		t.Fatalf("got %d rows", len(rows))
	}
	for i, r := range rows {
		if r[0] != int64(i) || r[1] != int32(i) || r[2] != int16(i%30000) || r[3] != int8(i%100) ||
			r[4] != uint64(i) || fmt.Sprint(r[5]) != fmt.Sprint(i) {
			t.Fatalf("row %d: %#v", i, r)
		}
	}
	chunks := e.chunks(t)
	if len(chunks) < n/2048 {
		t.Errorf("only %d chunks for %d rows", len(chunks), n)
	}
	if kinds := e.columnKinds(t); kinds["sequenceColumn"] == 0 {
		t.Errorf("expected SEQUENCE vectors, got %v", kinds)
	}

	// Descending and negative sequences.
	rows = e.rows(t, "SELECT range FROM range(1000, -1000, -7)")
	for i, r := range rows {
		if r[0] != int64(1000-7*i) {
			t.Fatalf("row %d: %#v", i, r[0])
		}
	}
}

// TestDictionaryVectors scans dictionary-compressed columns of a
// checkpointed database file, which the server ships as DICTIONARY vectors.
func TestDictionaryVectors(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	e.exec(t, "ATTACH "+livetest.SQLString(filepath.Join(dir, "dict.db"))+" AS f")
	e.exec(t, `CREATE TABLE f.types_dict AS SELECT i AS id,
		CASE WHEN i % 13 = 0 THEN NULL ELSE ['alpha', 'beta', '', 'ünïcødé', repeat('long', 10)][i % 5 + 1] END AS s,
		CASE WHEN i % 17 = 0 THEN NULL ELSE ['a', 'b', 'c'][i % 3 + 1]::ENUM('a', 'b', 'c') END AS en,
		(i % 4)::INTEGER AS n,
		CASE WHEN i % 4 = 0 THEN NULL ELSE ((i % 4) * 1.25)::DECIMAL(9,2) END AS d,
		from_hex(['00', 'FF', '5C27'][i % 3 + 1]) AS b
		FROM range(50000) t(i)`)
	e.exec(t, "CHECKPOINT f")
	e.reset()
	rows := e.rows(t, "SELECT id, s, en, n, d, b, s::VARCHAR IS NOT DISTINCT FROM s FROM f.types_dict ORDER BY id")
	if len(rows) != 50000 {
		t.Fatalf("got %d rows", len(rows))
	}
	strs := []string{"alpha", "beta", "", "ünïcødé", strings.Repeat("long", 10)}
	blobs := [][]byte{{0x00}, {0xFF}, {0x5C, 0x27}}
	for _, r := range rows {
		i := r[0].(int64)
		var s any = strs[i%5]
		if i%13 == 0 {
			s = nil
		}
		var en any = []string{"a", "b", "c"}[i%3]
		if i%17 == 0 {
			en = nil
		}
		var d any = []string{"0.00", "1.25", "2.50", "3.75"}[i%4]
		if i%4 == 0 {
			d = nil
		}
		if r[1] != s || r[2] != en || r[3] != int32(i%4) || r[4] != d || !bytes.Equal(r[5].([]byte), blobs[i%3]) {
			t.Fatalf("row %d: %#v", i, r)
		}
	}
	// Without ORDER BY the scan output reaches the client unchanged.
	e.reset()
	rows = e.rows(t, "SELECT s, id FROM f.types_dict")
	kinds := e.columnKinds(t)
	if kinds["dictColumn"] == 0 {
		t.Errorf("expected DICTIONARY vectors for a dictionary-compressed column, got %v", kinds)
	}
	for _, r := range rows {
		i := r[1].(int64)
		var s any = strs[i%5]
		if i%13 == 0 {
			s = nil
		}
		if r[0] != s {
			t.Fatalf("row %d: %#v, want %#v", i, r[0], s)
		}
	}
}

// TestStringLayouts checks VARCHAR-physical FLAT vectors (the 107/108/109
// layout): empty strings, NULL patterns, inlined (<= 12 bytes) and heap
// strings, invalid UTF-8 in BLOBs, and strings over 1 MiB.
func TestStringLayouts(t *testing.T) {
	e := newEnv(t)
	e.reset()
	rows := e.rows(t, `SELECT i,
			CASE WHEN i % 3 = 0 THEN NULL WHEN i % 3 = 1 THEN '' ELSE repeat(chr((65 + i % 26)::INTEGER), (i % 40)::INTEGER) END,
			CASE WHEN i % 2 = 0 THEN NULL ELSE from_hex(printf('%02X', (i % 256)::INTEGER)) END
		FROM range(5000) t(i) ORDER BY i`)
	for _, r := range rows {
		i := r[0].(int64)
		var s any
		switch i % 3 {
		case 1:
			s = ""
		case 2:
			s = strings.Repeat(string(rune(65+i%26)), int(i%40))
		}
		if r[1] != s {
			t.Fatalf("row %d: %#v, want %#v", i, r[1], s)
		}
		if i%2 == 0 {
			if r[2] != nil {
				t.Fatalf("row %d blob: %#v", i, r[2])
			}
		} else if b := r[2].([]byte); len(b) != 1 || b[0] != byte(i%256) {
			t.Fatalf("row %d blob: %#v", i, r[2])
		}
	}
	// field 107 (byte_data_length) right after a has_validity=true + mask,
	// or has_validity=false: the server uses the 2.0 string layout.
	if raw := e.raw(); !bytes.Contains(raw, []byte{0x6B, 0x00}) || !bytes.Contains(raw, []byte{0x6C, 0x00}) {
		t.Errorf("no 107/108 string fields in the responses")
	}

	// Strings above 1 MiB, alone and among small ones.
	big := 3<<20 + 7
	rows = e.rows(t, fmt.Sprintf(`SELECT i, CASE WHEN i = 1 THEN repeat('ü', %d) WHEN i = 2 THEN repeat(chr(0), 10) ELSE 'small' || i END,
			CASE WHEN i = 1 THEN repeat('ab', %d)::BLOB END
		FROM range(4) t(i) ORDER BY i`, big/2, big))
	for _, r := range rows {
		i := r[0].(int64)
		s := r[1].(string)
		switch i {
		case 1:
			if s != strings.Repeat("ü", big/2) {
				t.Errorf("long string: %d bytes", len(s))
			}
			if b := r[2].([]byte); !bytes.Equal(b, bytes.Repeat([]byte("ab"), big)) {
				t.Errorf("long blob: %d bytes", len(b))
			}
		case 2:
			if s != strings.Repeat("\x00", 10) {
				t.Errorf("NUL string: %q", s)
			}
		default:
			if s != fmt.Sprint("small", i) {
				t.Errorf("row %d: %q", i, s)
			}
		}
	}
	// Many large strings: the result spans several FETCH batches.
	e.reset()
	rows = e.rows(t, "SELECT i, repeat(i::VARCHAR, 3000) FROM range(6000) t(i) ORDER BY i")
	for _, r := range rows {
		if r[1] != strings.Repeat(fmt.Sprint(r[0]), 3000) {
			t.Fatalf("row %v: wrong %d-byte string", r[0], len(r[1].(string)))
		}
	}
	if n := len(e.chunks(t)); n < 3 {
		t.Errorf("expected several chunks, got %d", n)
	}
}

// TestGeometry decodes GEOMETRY values as WKB and compares them with the
// server's own WKB, also after a checkpoint (where GEOMETRY may be shredded
// in storage).
func TestGeometry(t *testing.T) {
	e := newEnv(t)
	geoms := []string{
		"POINT (1 2)", "LINESTRING (0 0, 1 1, 2 0)", "POLYGON ((0 0, 1 0, 1 1, 0 1, 0 0))",
		"MULTIPOINT ((1 2), (3 4))", "GEOMETRYCOLLECTION (POINT (1 2), LINESTRING (0 0, 1 1))", "POINT EMPTY",
		"POINT Z (1 2 3)",
	}
	var vals []string
	for _, g := range geoms {
		vals = append(vals, "("+livetest.SQLString(g)+")")
	}
	src := "(VALUES " + strings.Join(vals, ", ") + ", (NULL)) v(w)"
	check := func(q string) {
		t.Helper()
		rows := e.rows(t, q)
		if len(rows) != len(geoms)+1 {
			t.Fatalf("%s: %d rows", q, len(rows))
		}
		for _, r := range rows {
			if r[1] == nil {
				if r[0] != nil {
					t.Errorf("NULL geometry decoded as %#v", r[0])
				}
				continue
			}
			got, ok := r[0].([]byte)
			if !ok || !bytes.Equal(got, r[1].([]byte)) {
				t.Errorf("%v: decoded %x, server WKB %x", r[2], r[0], r[1])
			}
		}
	}
	check("SELECT w::GEOMETRY, w::GEOMETRY::BLOB, w FROM " + src)
	// GEOMETRY vectors nested in LIST and STRUCT vectors.
	for _, r := range e.rows(t, "SELECT [w::GEOMETRY, NULL], {'g': w::GEOMETRY}, w::GEOMETRY::BLOB FROM "+src) {
		l := r[0].([]any)
		g := r[1].(map[string]any)["g"]
		if r[2] == nil {
			if l[0] != nil || g != nil {
				t.Errorf("NULL nested geometry: %#v %#v", l[0], g)
			}
			continue
		}
		if !bytes.Equal(l[0].([]byte), r[2].([]byte)) || l[1] != nil || !bytes.Equal(g.([]byte), r[2].([]byte)) {
			t.Errorf("nested geometry: %#v %#v, want %x", l, g, r[2])
		}
	}

	dir := t.TempDir()
	e.exec(t, "ATTACH "+livetest.SQLString(filepath.Join(dir, "geo.db"))+" AS g")
	e.exec(t, "SET geometry_minimum_shredding_size = 0")
	e.exec(t, "CREATE TABLE g.types_geo AS SELECT w::GEOMETRY AS geo, w FROM "+src+", range(100)")
	e.exec(t, "CREATE TABLE g.types_points AS SELECT ('POINT (' || i || ' ' || (i * 2) || ')')::GEOMETRY AS geo FROM range(5000) t(i)")
	e.exec(t, "CHECKPOINT g")
	rows := e.rows(t, "SELECT DISTINCT geo, geo::BLOB, w FROM g.types_geo")
	if len(rows) != len(geoms)+1 {
		t.Fatalf("checkpointed: %d distinct rows", len(rows))
	}
	check("SELECT geo, geo::BLOB, w FROM (SELECT DISTINCT geo, w FROM g.types_geo)")
	for _, r := range e.rows(t, "SELECT geo, geo::BLOB FROM g.types_points") {
		if !bytes.Equal(r[0].([]byte), r[1].([]byte)) {
			t.Fatalf("point: decoded %x, server WKB %x", r[0], r[1])
		}
	}
}

// equalValues compares decoded values, treating NaN as equal to itself.
func equalValues(a, b any) bool {
	return fmt.Sprintf("%#v", a) == fmt.Sprintf("%#v", b)
}
