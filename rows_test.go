package quackdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/message"
)

// Regression: Rows.Next must not panic when a server-supplied chunk has
// fewer columns or shorter column vectors than the PrepareResponse promised.
// A malicious or buggy server should produce a clear error instead.
func TestRowsNextDefendsAgainstShortChunk(t *testing.T) {
	// Three declared columns, but the chunk only has two.
	prep := message.PrepareResponse{
		ResultNames: []string{"a", "b", "c"},
		ResultTypes: []logicaltype.Type{
			{ID: logicaltype.IDInteger},
			{ID: logicaltype.IDInteger},
			{ID: logicaltype.IDInteger},
		},
		Results: []message.DataChunk{{
			RowCount: 1,
			Types: []logicaltype.Type{
				{ID: logicaltype.IDInteger},
				{ID: logicaltype.IDInteger},
			},
			Columns: [][]any{
				{int32(1)},
				{int32(2)},
			},
		}},
	}
	r := newRows(context.Background(), &Conn{}, prep)
	dest := make([]driver.Value, 3)
	if err := r.Next(dest); err == nil || !strings.Contains(err.Error(), "columns") {
		t.Fatalf("expected column-count error, got %v", err)
	}
}

func TestRowsNextDefendsAgainstShortColumn(t *testing.T) {
	prep := message.PrepareResponse{
		ResultNames: []string{"a"},
		ResultTypes: []logicaltype.Type{{ID: logicaltype.IDInteger}},
		Results: []message.DataChunk{{
			RowCount: 3, // claims 3 rows
			Types:    []logicaltype.Type{{ID: logicaltype.IDInteger}},
			Columns:  [][]any{{int32(1)}}, // but only delivers 1
		}},
	}
	r := newRows(context.Background(), &Conn{}, prep)
	dest := make([]driver.Value, 1)
	// First row should succeed.
	if err := r.Next(dest); err != nil {
		t.Fatalf("first row: %v", err)
	}
	// Second row hits the short column — should be a graceful error, not a panic.
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("Next panicked on short column: %v", rec)
		}
	}()
	if err := r.Next(dest); err == nil || !strings.Contains(err.Error(), "rows") {
		t.Fatalf("expected row-count error, got %v", err)
	}
}

// Regression: Rows.Close on an exhausted iterator is a cheap no-op. The
// double-close path used to nil out chunks but is otherwise inert.
func TestRowsCloseIdempotent(t *testing.T) {
	prep := message.PrepareResponse{
		ResultNames:    []string{"a"},
		ResultTypes:    []logicaltype.Type{{ID: logicaltype.IDInteger}},
		NeedsMoreFetch: false,
	}
	r := newRows(context.Background(), &Conn{}, prep)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second close should be no-op: %v", err)
	}
	dest := make([]driver.Value, 1)
	if err := r.Next(dest); !errors.Is(err, io.EOF) {
		t.Fatalf("Next on closed: got %v want io.EOF", err)
	}
}
