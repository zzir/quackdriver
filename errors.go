package quackdriver

import (
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"

	"github.com/zzir/quackdriver/internal/message"
	"github.com/zzir/quackdriver/internal/transport"
)

// Error is an error reported by the Quack server (an ERROR_RESPONSE). Use
// errors.As to inspect it:
//
//	var qe *quackdriver.Error
//	if errors.As(err, &qe) && qe.Type == "Constraint" { ... }
type Error struct {
	// Type is DuckDB's exception type name, e.g. "Catalog", "Parser",
	// "Constraint", "Invalid Input". It is empty when the server sent none.
	Type string
	// Message is the server's error message without the type prefix.
	Message string
	// ExtraInfo holds the error's transferable extra info (for example
	// "error_subtype" or "name"); nil when there is none.
	ExtraInfo map[string]string
	// MustInvalidate is set when the server's own database was invalidated
	// by this error. The connection that received it is discarded.
	MustInvalidate bool
}

// Error formats the error the way DuckDB does ("<Type> Error: <Message>"),
// prefixed with "quackdriver: ".
func (e *Error) Error() string {
	if e.Type == "" {
		return "quackdriver: " + e.Message
	}
	return "quackdriver: " + e.Type + " Error: " + e.Message
}

func newServerError(r message.ErrorResponse) *Error {
	return &Error{
		Type:           r.ExceptionType,
		Message:        r.Message,
		ExtraInfo:      r.ExtraInfo,
		MustInvalidate: r.MustInvalidate,
	}
}

// ErrConnBusy is returned by statements issued on a connection while an
// Appender holds it exclusively. Close the Appender first, or use another
// connection.
var ErrConnBusy = errors.New("quackdriver: connection is busy: an Appender is open on it")

// ValueError reports a row whose Go values do not fit the columns of an
// Append or Appender: a value that cannot be converted to its column's type
// (see AppendColumn) or a row with the wrong number of values. Use errors.As
// to tell it from errors that end the stream:
//
//   - Conn.Append returns it before anything is sent; the table is
//     unchanged.
//   - Appender.AppendRow returns it after skipping the row; the Appender
//     stays usable. Any other AppendRow error has ended the stream.
type ValueError struct {
	// Row is the 0-based number of the row: its index in Conn.Append's
	// rows, or the number of earlier AppendRow calls on the Appender
	// (skipped rows included).
	Row int64
	// Column is the 0-based index of the offending column, or -1 when the
	// row has the wrong number of values.
	Column int
	// Err describes the problem.
	Err error

	msg string
}

func (e *ValueError) Error() string { return e.msg }
func (e *ValueError) Unwrap() error { return e.Err }

// newValueError builds a ValueError; prefix ("quackdriver: " or
// "quackdriver: Appender: ") and column (its quoted name or index; ignored
// when col < 0) only shape the message.
func newValueError(prefix string, row int64, col int, column string, err error) *ValueError {
	msg := prefix + "row " + strconv.FormatInt(row, 10)
	if col >= 0 {
		msg += " column " + column + ": " + err.Error()
	} else {
		msg += " " + err.Error()
	}
	return &ValueError{Row: row, Column: col, Err: err, msg: msg}
}

// errVariant is returned for a statement whose result has a VARIANT column:
// the driver does not decode VARIANT values.
var errVariant = errors.New("quackdriver: VARIANT values are not supported; cast them in the query, e.g. SELECT v::JSON")

// errConnClosed is returned when a closed Conn is used directly (outside
// database/sql, which never hands out closed connections).
var errConnClosed = errors.New("quackdriver: connection is closed")

// connGoneMessages are the server answers that mean the session no longer
// exists (QuackServer::DispatchMessage). They are produced before the server
// touches the statement, so the request that received them was not executed.
// They carry the same exception type as errors raised by SQL (error() in a
// query can produce any message), so only these exact texts are trusted.
var connGoneMessages = map[string]bool{
	"Invalid connection id":              true,
	"Connection heartbeat lease expired": true,
	"Database was closed":                true,
}

// invalidatedPrefix starts the server's answer once its database was
// invalidated; the server always sets must_invalidate on it.
const invalidatedPrefix = "The server database has been invalidated"

// isConnGone reports whether a server error means the session is gone.
func isConnGone(e *Error) bool {
	if connGoneMessages[e.Message] {
		return true
	}
	return e.MustInvalidate && strings.HasPrefix(e.Message, invalidatedPrefix)
}

// badConnError reports a request that provably did not execute on a session
// that is no longer usable. It satisfies errors.Is(err, driver.ErrBadConn),
// so database/sql retries the operation on a fresh connection, and unwraps to
// the underlying cause (a *Error or a transport error).
type badConnError struct{ cause error }

func (e *badConnError) Error() string {
	return "quackdriver: bad connection: " + strings.TrimPrefix(e.cause.Error(), "quackdriver: ")
}

func (e *badConnError) Unwrap() error { return e.cause }

func (e *badConnError) Is(target error) bool { return target == driver.ErrBadConn }

// classify turns a request error into what the caller returns, marking the
// connection bad when the session is gone.
//
// first is true when the failed request starts an operation (PREPARE of a new
// statement, a heartbeat for Ping): nothing was executed, so a session-gone
// answer or a request that never left the client becomes a bad-connection
// error that database/sql may retry on a fresh connection. For follow-up
// requests (FETCH of a statement the server already runs) a retry would run
// the statement twice, so the connection is only marked bad and the cause is
// returned as is.
func (c *Conn) classify(err error, first bool) error {
	var se *Error
	notRun := false // the request provably did not execute
	switch {
	case errors.As(err, &se):
		if isConnGone(se) || se.MustInvalidate {
			c.markGone()
		}
		notRun = isConnGone(se)
	case transport.NotSent(err):
		c.markBad()
		notRun = true
	}
	if first && notRun {
		return &badConnError{cause: err}
	}
	return err
}
