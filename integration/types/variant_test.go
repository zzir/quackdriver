//go:build integration

package types_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zzir/quackdriver/internal/livetest"
)

// TestVariantRejected: a result with a VARIANT column, at the top level or
// nested, fails with a hint to cast it, whether its values come inline or
// in FETCH batches; the session stays usable and the suggested cast works.
func TestVariantRejected(t *testing.T) {
	e := newEnv(t)
	c := livetest.Conn(t, e.db)
	for _, q := range []string{
		"SELECT 42::VARIANT AS v",
		"SELECT [1::VARIANT, 'x'::VARIANT] AS l",
		"SELECT {'v': 'x'::VARIANT} AS s",
		"SELECT 1::VARIANT AS v WHERE false",
		"SELECT i::VARIANT AS v FROM range(100000) t(i)", // more rows than PREPARE inlines
	} {
		rows, err := c.QueryContext(context.Background(), q)
		if err == nil {
			_ = rows.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "v::JSON") {
			t.Errorf("%s: %v, want the VARIANT error", q, err)
		}
	}
	if got := livetest.Scalar[string](t, c, "SELECT {'a': [1, 2]}::VARIANT::JSON"); got != `{"a":[1,2]}` {
		t.Errorf("VARIANT cast to JSON: %q", got)
	}
}
