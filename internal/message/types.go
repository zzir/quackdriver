// Package message implements the Quack wire protocol messages (protocol
// version 3, DuckDB 2.0): the header, the message bodies, and DataChunk
// vector encoding.
//
// A message is a MessageHeader object followed by a body object, both
// written by DuckDB's BinarySerializer with StorageVersion::V2_0_0.
// FETCH_RESPONSE and SEND_DATA_REQUEST are followed by chunk_count raw
// DataChunk objects (no wrapper object, no nullable marker).
package message

import (
	"encoding/binary"
	"errors"
	"strconv"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/value"
)

// QuackVersion is the Quack wire-protocol version this package speaks
// (QUACK_VERSION in duckdb-quack's quack_message.hpp).
const QuackVersion = 3

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
	TypeSendDataRequest    Type = 9
	TypeSuccessResponse    Type = 10
	TypeDisconnectMessage  Type = 11
	TypeCancelRequest      Type = 12
	TypeSendDataResponse   Type = 14
	TypeAcknowledgement    Type = 15
	TypeHeartbeatRequest   Type = 16
	TypeErrorResponse      Type = 100
)

var typeNames = map[Type]string{
	TypeInvalid: "INVALID", TypeConnectionRequest: "CONNECTION_REQUEST", TypeConnectionResponse: "CONNECTION_RESPONSE",
	TypePrepareRequest: "PREPARE_REQUEST", TypePrepareResponse: "PREPARE_RESPONSE", TypeFetchRequest: "FETCH_REQUEST",
	TypeFetchResponse: "FETCH_RESPONSE", TypeSendDataRequest: "SEND_DATA_REQUEST", TypeSuccessResponse: "SUCCESS_RESPONSE",
	TypeDisconnectMessage: "DISCONNECT_MESSAGE", TypeCancelRequest: "CANCEL_REQUEST", TypeSendDataResponse: "SEND_DATA_RESPONSE",
	TypeAcknowledgement: "ACKNOWLEDGEMENT", TypeHeartbeatRequest: "HEARTBEAT_REQUEST", TypeErrorResponse: "ERROR_RESPONSE",
}

// String returns the upstream enum name, e.g. "FETCH_RESPONSE".
func (t Type) String() string {
	if name, ok := typeNames[t]; ok {
		return name
	}
	return "MessageType(" + strconv.Itoa(int(t)) + ")"
}

// ErrUnknownMessageType is returned when an unrecognized message type appears
// on the wire.
var ErrUnknownMessageType = errors.New("message: unknown message type")

// OptionalIdx is DuckDB's optional_idx (see codec.OptionalIdx).
type OptionalIdx = codec.OptionalIdx

// Idx returns a valid OptionalIdx holding v.
func Idx(v uint64) OptionalIdx { return OptionalIdx{Value: v, Valid: true} }

// Header is the shared message envelope (MessageHeader).
type Header struct {
	Type          Type
	ConnectionID  string
	HasQueryID    bool
	ClientQueryID uint64
}

// Body is implemented by every concrete message variant.
type Body interface {
	MessageType() Type
	encode(w *codec.Writer) error
}

// ConnectionRequest opens a Quack session.
type ConnectionRequest struct {
	AuthString               string
	ClientDuckdbVersion      string
	ClientPlatform           string
	MinSupportedQuackVersion uint64
	MaxSupportedQuackVersion uint64
	// ClientID identifies the client in server logs and
	// quack_active_connections(); empty or at least 4 characters.
	ClientID string
	// HeartbeatTimeoutSeconds is the requested lease; the server rejects 0.
	HeartbeatTimeoutSeconds uint64
}

// MessageType returns TypeConnectionRequest.
func (ConnectionRequest) MessageType() Type { return TypeConnectionRequest }

// ConnectionResponse acknowledges a new Quack session. The connection id
// travels in the header.
type ConnectionResponse struct {
	ServerDuckdbVersion string
	ServerPlatform      string
	QuackVersion        uint64
	// HeartbeatTimeoutSeconds echoes the lease the server granted.
	HeartbeatTimeoutSeconds uint64
}

// MessageType returns TypeConnectionResponse.
func (ConnectionResponse) MessageType() Type { return TypeConnectionResponse }

// PrepareRequest carries a SQL statement to prepare-and-execute.
type PrepareRequest struct {
	SQL string
	// QueryUUID is chosen by the client (see NewQueryUUID) and names the
	// statement in later FETCH / CANCEL / ACKNOWLEDGEMENT messages.
	QueryUUID codec.HugeInt
	// InlineRows caps the rows the response may carry; unset uses the
	// server setting, 0 makes PREPARE answer before the query yields a row.
	InlineRows OptionalIdx
}

// MessageType returns TypePrepareRequest.
func (PrepareRequest) MessageType() Type { return TypePrepareRequest }

// PrepareResponse delivers the schema, the inlined chunks and the query UUID.
type PrepareResponse struct {
	ResultTypes    []logicaltype.Type
	ResultNames    []string
	NeedsMoreFetch bool
	Results        []DataChunk
	QueryUUID      codec.HugeInt
}

// MessageType returns TypePrepareResponse.
func (PrepareResponse) MessageType() Type { return TypePrepareResponse }

// FetchRequest asks for one dense batch (BatchIndex, from 1) of a result and
// acknowledges every batch up to AckIndex.
type FetchRequest struct {
	QueryUUID  codec.HugeInt
	BatchIndex uint64
	AckIndex   uint64
}

// MessageType returns TypeFetchRequest.
func (FetchRequest) MessageType() Type { return TypeFetchRequest }

// FetchResponse delivers one batch. On the wire chunk_count = len(Results)
// and the chunks follow the body as raw DataChunk objects. An empty Results
// marks the end of the stream; TotalBatches is set on that terminal response.
type FetchResponse struct {
	Results      []DataChunk
	TotalBatches OptionalIdx
	BatchIndex   OptionalIdx
}

// MessageType returns TypeFetchResponse.
func (FetchResponse) MessageType() Type { return TypeFetchResponse }

// SendDataRequest sends one batch of a client data stream read by
// scan_data_from_quack_client. Data batches carry BatchIndex (1, 2, ...) and
// Chunks; the terminal message carries only TotalBatches. On the wire
// chunk_count = len(Chunks) and the chunks follow the body as raw DataChunk
// objects.
type SendDataRequest struct {
	StreamID     string
	Chunks       []DataChunk
	TotalBatches OptionalIdx
	BatchIndex   OptionalIdx
}

// MessageType returns TypeSendDataRequest.
func (SendDataRequest) MessageType() Type { return TypeSendDataRequest }

// SendDataResponse acknowledges a SendDataRequest. AcceptBudget is a
// placeholder for future flow control and is currently always unset.
type SendDataResponse struct {
	AcceptBudget OptionalIdx
}

// MessageType returns TypeSendDataResponse.
func (SendDataResponse) MessageType() Type { return TypeSendDataResponse }

// CancelRequest cancels the statement named by QueryUUID. The zero UUID is a
// wildcard: it cancels whatever runs on the connection.
type CancelRequest struct {
	QueryUUID codec.HugeInt
}

// MessageType returns TypeCancelRequest.
func (CancelRequest) MessageType() Type { return TypeCancelRequest }

// Acknowledgement releases the server-side result cache of a query.
type Acknowledgement struct {
	QueryUUID codec.HugeInt
}

// MessageType returns TypeAcknowledgement.
func (Acknowledgement) MessageType() Type { return TypeAcknowledgement }

// HeartbeatRequest renews the connection lease.
type HeartbeatRequest struct{}

// MessageType returns TypeHeartbeatRequest.
func (HeartbeatRequest) MessageType() Type { return TypeHeartbeatRequest }

// SuccessResponse acknowledges a void-returning request.
type SuccessResponse struct{}

// MessageType returns TypeSuccessResponse.
func (SuccessResponse) MessageType() Type { return TypeSuccessResponse }

// DisconnectMessage tells the server to close the session.
type DisconnectMessage struct{}

// MessageType returns TypeDisconnectMessage.
func (DisconnectMessage) MessageType() Type { return TypeDisconnectMessage }

// ErrorResponse carries a server-side error.
type ErrorResponse struct {
	Message string
	// ExceptionType is DuckDB's exception type name, e.g. "Catalog",
	// "Parser", "Invalid Input"; empty when the server sent none.
	ExceptionType string
	// ExtraInfo is the error's transferable extra info (DuckDB never sends
	// exception_type, exception_message, position or stack traces here).
	ExtraInfo map[string]string
	// MustInvalidate is set when the server's own database was invalidated
	// by this error; the connection must not be reused.
	MustInvalidate bool
}

// MessageType returns TypeErrorResponse.
func (ErrorResponse) MessageType() Type { return TypeErrorResponse }

// NewQueryUUID returns a random version-4 UUID as DuckDB's hugeint
// representation, the way UUIDv4::GenerateRandomUUID builds it (bytes 0..7
// big-endian into upper, 8..15 into lower). It is never zero: the version
// nibble keeps the upper word non-zero, so it can never collide with the
// CANCEL wildcard or the ACKNOWLEDGEMENT default.
func NewQueryUUID() codec.HugeInt {
	b, err := value.RandomUUID()
	if err != nil {
		// crypto/rand only fails when the OS entropy source is broken;
		// a predictable query id would be worse than stopping.
		panic("message: crypto/rand failed: " + err.Error())
	}
	return codec.HugeInt{
		Upper: int64(binary.BigEndian.Uint64(b[0:8])),
		Lower: binary.BigEndian.Uint64(b[8:16]),
	}
}
