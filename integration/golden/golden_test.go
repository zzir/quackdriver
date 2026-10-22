//go:build integration

// Package golden holds end-to-end tests of the wire-fixture tooling:
// cmd/dump-wire must reproduce testdata/golden byte for byte from a fresh
// DuckDB 2.0 server (only the server-assigned connection id differs), and
// cmd/probe-fetch must drive a large FETCH stream correctly for every
// read-ahead depth.
package golden

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/livetest"
	"github.com/zzir/quackdriver/internal/message"
)

// repoRoot is the module root, two levels above this package.
var repoRoot = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}()

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

// tool returns the path of a freshly built command from cmd/<name>.
func tool(t *testing.T, name string) string {
	t.Helper()
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "quackdriver-golden-bin-")
		if buildErr != nil {
			return
		}
		args := []string{"build", "-o", binDir + string(filepath.Separator)}
		if raceEnabled {
			args = append(args, "-race")
		}
		cmd := exec.Command("go", append(args, "./cmd/dump-wire", "./cmd/probe-fetch")...)
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return filepath.Join(binDir, name)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// run executes a built tool and returns its combined output.
func run(t *testing.T, bin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// readFixtures returns the fixture files of dir by name.
func readFixtures(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.bin"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = b
	}
	return out
}

// normalize replaces the session's connection id (taken from the handshake
// response) with a placeholder of the same length, and the server's version
// and platform strings in the handshake response with fixed placeholders, so
// that two captures compare byte for byte across server builds (the release
// v2.0.0 the fixtures came from reports "v2.0.0", a dev build "v2.0.0-dev1").
// It also sorts the extra_info entries of error responses, whose order
// depends on the server's platform.
func normalize(t *testing.T, set map[string][]byte) map[string][]byte {
	t.Helper()
	h, body, err := message.Decode(set["handshake.resp.bin"])
	if err != nil {
		t.Fatalf("handshake.resp.bin: %v", err)
	}
	if h.ConnectionID == "" {
		t.Fatal("handshake.resp.bin carries no connection id")
	}
	resp, ok := body.(message.ConnectionResponse)
	if !ok {
		t.Fatalf("handshake.resp.bin holds a %s", body.MessageType())
	}
	id := []byte(h.ConnectionID)
	placeholder := bytes.Repeat([]byte{'X'}, len(id))
	out := map[string][]byte{}
	for name, b := range set {
		out[name] = bytes.ReplaceAll(b, id, placeholder)
	}
	// CONNECTION_RESPONSE: field 1 server_duckdb_version, field 2
	// server_platform, each a length-prefixed string.
	hs := out["handshake.resp.bin"]
	for _, f := range []struct {
		id  uint16
		val string
	}{{1, resp.ServerDuckdbVersion}, {2, resp.ServerPlatform}} {
		old, repl := stringField(f.id, f.val), stringField(f.id, "<masked>")
		if bytes.Count(hs, old) != 1 {
			t.Fatalf("handshake.resp.bin: field %d %q not found exactly once", f.id, f.val)
		}
		hs = bytes.Replace(hs, old, repl, 1)
	}
	out["handshake.resp.bin"] = hs
	// An ERROR_RESPONSE's extra_info is a std::unordered_map, which the
	// server writes in hash order: libstdc++ (linux_amd64, where the
	// fixtures come from) and libc++ (macOS) order it differently. Encode
	// writes the entries sorted by key; Decode rejects unknown fields, so an
	// unchanged length means only the order changed.
	for name, b := range out {
		h, body, err := message.Decode(b)
		if er, ok := body.(message.ErrorResponse); err != nil || !ok || len(er.ExtraInfo) < 2 {
			continue
		}
		if canon, err := message.Encode(h, body); err == nil && len(canon) == len(b) {
			out[name] = canon
		}
	}
	return out
}

// stringField encodes a string property the way the server's
// BinarySerializer does: the field id (u16 LE), the ULEB128 length, the
// bytes.
func stringField(id uint16, s string) []byte {
	w := codec.NewWriter()
	w.WriteULEB128(uint64(len(s)))
	return append(append([]byte{byte(id), byte(id >> 8)}, w.Bytes()...), s...)
}

// TestCaptureReproducesGolden captures a fresh fixture set from a new server
// and compares it with the committed one. A difference is protocol drift:
// either the server build changed (then regenerate and review the diff) or
// the encoder no longer produces the bytes it used to.
func TestCaptureReproducesGolden(t *testing.T) {
	srv := livetest.StartServer(t, livetest.ServerOptions{})
	out := t.TempDir()
	res, err := run(t, tool(t, "dump-wire"), "-dsn", srv.DSN(), "-out", out, "-v")
	if err != nil {
		t.Fatalf("dump-wire: %v\n%s", err, res)
	}
	if !strings.Contains(res, "== select_variant: PREPARE_REQUEST -> PREPARE_RESPONSE") {
		t.Errorf("verbose output lacks the exchange summary:\n%s", res)
	}

	got := normalize(t, readFixtures(t, out))
	want := normalize(t, readFixtures(t, filepath.Join(repoRoot, "testdata", "golden")))
	var names []string
	for n := range want {
		names = append(names, n)
	}
	for n := range got {
		if _, ok := want[n]; !ok {
			t.Errorf("capture produced %s, which testdata/golden lacks", n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		g, ok := got[n]
		switch {
		case !ok:
			t.Errorf("capture did not produce %s", n)
		case !bytes.Equal(g, want[n]):
			t.Errorf("%s differs from testdata/golden (%d vs %d bytes)\n got  %x\n want %x", n, len(g), len(want[n]), head(g), head(want[n]))
		}
	}
}

func head(b []byte) []byte {
	if len(b) > 96 {
		return b[:96]
	}
	return b
}

// TestCaptureFailureKeepsFixtures checks that a failed capture leaves the
// output directory untouched and no scratch directory behind.
func TestCaptureFailureKeepsFixtures(t *testing.T) {
	parent := t.TempDir()
	out := filepath.Join(parent, "golden")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(out, "old.req.bin")
	if err := os.WriteFile(keep, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	dsn := fmt.Sprintf("quack://127.0.0.1:%d?token=x&connect_timeout=2s", livetest.FreePort(t))
	if res, err := run(t, tool(t, "dump-wire"), "-dsn", dsn, "-out", out); err == nil {
		t.Fatalf("dump-wire against a closed port succeeded:\n%s", res)
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "old" {
		t.Errorf("existing fixture changed: %q, %v", b, err)
	}
	entries, _ := os.ReadDir(parent)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("scratch files left behind: %v", names)
	}
}

// TestCaptureNeedsUTC: a server outside UTC renders TIMESTAMPTZ with its own
// offset, which the fixtures must not hold; dump-wire fails and keeps the
// existing fixtures.
func TestCaptureNeedsUTC(t *testing.T) {
	srv := livetest.StartServer(t, livetest.ServerOptions{InitSQL: "SET GLOBAL TimeZone = 'America/New_York';"})
	out := t.TempDir()
	keep := filepath.Join(out, "old.req.bin")
	if err := os.WriteFile(keep, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := run(t, tool(t, "dump-wire"), "-dsn", srv.DSN(), "-out", out)
	if err == nil || !strings.Contains(res, "select_timestamptz: the server renders TIMESTAMPTZ as") || !strings.Contains(res, "TZ=UTC") {
		t.Fatalf("dump-wire against a server in America/New_York: %v\n%s", err, res)
	}
	if b, err := os.ReadFile(keep); err != nil || string(b) != "old" {
		t.Errorf("existing fixture changed: %q, %v", b, err)
	}
}

var (
	fetchLine  = regexp.MustCompile(`FETCH: (\d+) round trip\(s\): (\d+) data batch\(es\), 1 terminal, (\d+) past the end; total_batches=(\d+); max (\d+) in flight`)
	prepLine   = regexp.MustCompile(`PREPARE: (\d+) inline rows`)
	rowsLine   = regexp.MustCompile(`rows: (\d+) in .* out of order: (\d+)`)
	verboseHit = regexp.MustCompile(`FETCH batch_index=(\d+)\s+ack_index=(\d+)\s+-> `)
)

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestProbeFetch runs cmd/probe-fetch over a large result for several
// read-ahead depths and checks its report: every row arrives in order, the
// terminal FETCH's total_batches equals the data batches fetched, the window
// never exceeds fetch_ahead+1 requests, and every request acknowledges
// exactly the batches consumed before it.
func TestProbeFetch(t *testing.T) {
	srv := livetest.StartServer(t, livetest.ServerOptions{})
	const rows = 300000
	for _, tc := range []struct {
		params     string
		inline     bool // PREPARE may inline rows
		fetchAhead int
	}{
		{"fetch_ahead=0", true, 0},
		{"fetch_ahead=1&inline_rows=0", false, 1},
		{"fetch_ahead=4&inline_rows=0", false, 4},
		{"fetch_ahead=16", true, 16},
	} {
		t.Run(tc.params, func(t *testing.T) {
			res, err := run(t, tool(t, "probe-fetch"), "-dsn", srv.DSN(tc.params), "-rows", strconv.Itoa(rows), "-batch-bytes", "262144", "-v")
			if err != nil {
				t.Fatalf("probe-fetch: %v\n%s", err, res)
			}
			m := fetchLine.FindStringSubmatch(res)
			p := prepLine.FindStringSubmatch(res)
			r := rowsLine.FindStringSubmatch(res)
			if m == nil || p == nil || r == nil {
				t.Fatalf("unexpected report:\n%s", res)
			}
			trips, data, past, total, maxInFlight := atoi(t, m[1]), atoi(t, m[2]), atoi(t, m[3]), atoi(t, m[4]), atoi(t, m[5])
			if inline := atoi(t, p[1]); !tc.inline && inline != 0 {
				t.Errorf("inline_rows=0 but PREPARE inlined %d rows", inline)
			}
			if atoi(t, r[1]) != rows || r[2] != "0" {
				t.Errorf("rows line %q", r[0])
			}
			if data < 2 || total != data || trips != data+1+past {
				t.Errorf("report %q inconsistent", m[0])
			}
			if maxInFlight > tc.fetchAhead+1 || past > tc.fetchAhead {
				t.Errorf("fetch_ahead=%d but %q", tc.fetchAhead, m[0])
			}
			if tc.fetchAhead > 0 && maxInFlight < 2 {
				t.Errorf("fetch_ahead=%d never had two requests in flight: %q", tc.fetchAhead, m[0])
			}
			// Batches are consumed in order: batch i is requested with an
			// acknowledgement of at least i-1-fetch_ahead and below i.
			hits := verboseHit.FindAllStringSubmatch(res, -1)
			if len(hits) != data+1 {
				t.Errorf("%d verbose FETCH lines, want %d", len(hits), data+1)
			}
			for k, h := range hits {
				batch, ack := atoi(t, h[1]), atoi(t, h[2])
				if batch != k+1 || ack >= batch || ack < batch-1-tc.fetchAhead {
					t.Errorf("FETCH line %d: batch_index=%d ack_index=%d", k, batch, ack)
				}
			}
		})
	}
}
