// Package message implements the Quack wire protocol messages, including the
// header, the ten message bodies, and DataChunk vector encoding.
package message

import (
	"errors"
	"math"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// Type identifies a Quack message variant.
type Type uint8

// Message type ids. Match the upstream `MessageType` enum.
const (
	TypeInvalid            Type = 0
	TypeConnectionRequest  Type = 1
	TypeConnectionResponse Type = 2
	TypePrepareRequest     Type = 3
	TypePrepareResponse    Type = 4
	TypeFetchRequest       Type = 7
	TypeFetchResponse      Type = 8
	TypeAppendRequest      Type = 9
	TypeSuccessResponse    Type = 10
	TypeDisconnectMessage  Type = 11
	TypeErrorResponse      Type = 100
)

// optionalIndexInvalid is the DuckDB sentinel meaning "absent" inside an
// optional_idx field.
const optionalIndexInvalid uint64 = math.MaxUint64

// ErrUnknownMessageType is returned when an unrecognized message type appears
// on the wire.
var ErrUnknownMessageType = errors.New("message: unknown message type")

// Header is the shared message envelope.
type Header struct {
	Type          Type
	ConnectionID  string
	HasQueryID    bool
	ClientQueryID uint64
}

// Body is implemented by every concrete message variant.
type Body interface {
	MessageType() Type
	encode(w *codec.Writer)
}

// Connection messages.

// ConnectionRequest opens a Quack session.
type ConnectionRequest struct {
	AuthString               string
	ClientDuckdbVersion      string
	ClientPlatform           string
	MinSupportedQuackVersion uint64
	MaxSupportedQuackVersion uint64
}

// MessageType returns TypeConnectionRequest.
func (ConnectionRequest) MessageType() Type { return TypeConnectionRequest }

// ConnectionResponse acknowledges a new Quack session.
type ConnectionResponse struct {
	ServerDuckdbVersion string
	ServerPlatform      string
	QuackVersion        uint64
	HasQuackVersion     bool
}

// MessageType returns TypeConnectionResponse.
func (ConnectionResponse) MessageType() Type { return TypeConnectionResponse }

// PrepareRequest carries a SQL statement to prepare-and-execute.
type PrepareRequest struct {
	SQL string
}

// MessageType returns TypePrepareRequest.
func (PrepareRequest) MessageType() Type { return TypePrepareRequest }

// PrepareResponse delivers initial chunks and the result UUID.
type PrepareResponse struct {
	ResultTypes    []logicaltype.Type
	ResultNames    []string
	NeedsMoreFetch bool
	Results        []DataChunk
	ResultUUID     codec.HugeInt
}

// MessageType returns TypePrepareResponse.
func (PrepareResponse) MessageType() Type { return TypePrepareResponse }

// FetchRequest asks for additional chunks for a previous PrepareResponse.
type FetchRequest struct {
	ResultUUID codec.HugeInt
}

// MessageType returns TypeFetchRequest.
func (FetchRequest) MessageType() Type { return TypeFetchRequest }

// FetchResponse delivers additional chunks.
type FetchResponse struct {
	Results       []DataChunk
	HasBatchIndex bool
	BatchIndex    uint64
}

// MessageType returns TypeFetchResponse.
func (FetchResponse) MessageType() Type { return TypeFetchResponse }

// AppendRequest sends a DataChunk to be inserted into a table.
type AppendRequest struct {
	SchemaName  string
	TableName   string
	AppendChunk DataChunk
}

// MessageType returns TypeAppendRequest.
func (AppendRequest) MessageType() Type { return TypeAppendRequest }

// SuccessResponse acknowledges an APPEND or similar void-returning request.
type SuccessResponse struct{}

// MessageType returns TypeSuccessResponse.
func (SuccessResponse) MessageType() Type { return TypeSuccessResponse }

// DisconnectMessage tells the server to close the session.
type DisconnectMessage struct{}

// MessageType returns TypeDisconnectMessage.
func (DisconnectMessage) MessageType() Type { return TypeDisconnectMessage }

// ErrorResponse carries a server-side error message.
type ErrorResponse struct {
	Message string
}

// MessageType returns TypeErrorResponse.
func (ErrorResponse) MessageType() Type { return TypeErrorResponse }
