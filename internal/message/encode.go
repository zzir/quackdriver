package message

import (
	"fmt"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// Encode serializes a message: the header object, the body object and, for
// FETCH_RESPONSE and SEND_DATA_REQUEST, the raw DataChunk objects that follow
// the body. h.Type is taken from body.
//
// Properties follow duckdb-quack's generated serialize_quack_message.cpp:
// WriteProperty ones are always present, WritePropertyWithDefault ones are
// omitted when they hold their default, so a request encodes to the bytes
// the official client sends.
func Encode(h Header, body Body) ([]byte, error) { return EncodeSize(h, body, 0) }

// EncodeSize is Encode with a hint of the message's encoded size: the output
// buffer starts with that capacity, so a hint at or a little above the final
// size saves the copies a growing buffer makes (a 32 MiB SEND_DATA request
// otherwise copies its bytes several times over). A hint that is too small
// only costs those copies; 0 is no hint.
func EncodeSize(h Header, body Body, sizeHint int) ([]byte, error) {
	if body == nil {
		return nil, fmt.Errorf("message: nil body")
	}
	h.Type = body.MessageType()
	w := codec.NewWriterSize(sizeHint)
	if err := encodeHeader(w, h); err != nil {
		return nil, err
	}
	if err := body.encode(w); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}

func encodeHeader(w *codec.Writer, h Header) error {
	var err error
	w.WriteObject(func(w *codec.Writer) {
		w.WriteField(1, func(w *codec.Writer) { w.WriteULEB128(uint64(h.Type)) })
		w.WriteStringWithDefault(2, h.ConnectionID)
		w.WriteField(3, func(w *codec.Writer) {
			err = w.WriteOptionalIdx(OptionalIdx{Value: h.ClientQueryID, Valid: h.HasQueryID})
		})
	})
	if err != nil {
		return fmt.Errorf("message: header client_query_id: %w", err)
	}
	return nil
}

// writeBool writes a bool property "with default" false.
func writeBool(w *codec.Writer, id uint16, v bool) {
	if v {
		w.WriteField(id, func(w *codec.Writer) { w.WriteBool(true) })
	}
}

// writeIdx writes an optional_idx property "with default" (omitted when
// unset).
func writeIdx(w *codec.Writer, id uint16, name string, v OptionalIdx) error {
	if !v.Valid {
		return nil
	}
	var err error
	w.WriteField(id, func(w *codec.Writer) { err = w.WriteOptionalIdx(v) })
	if err != nil {
		return fmt.Errorf("message: %s: %w", name, err)
	}
	return nil
}

func writeHugeInt(w *codec.Writer, id uint16, v codec.HugeInt) {
	w.WriteField(id, func(w *codec.Writer) { w.WriteHugeInt(v) })
}

func (m ConnectionRequest) encode(w *codec.Writer) error {
	w.WriteObject(func(w *codec.Writer) {
		w.WriteStringWithDefault(1, m.AuthString)
		w.WriteStringWithDefault(2, m.ClientDuckdbVersion)
		w.WriteStringWithDefault(3, m.ClientPlatform)
		w.WriteUintWithDefault(4, m.MinSupportedQuackVersion)
		w.WriteUintWithDefault(5, m.MaxSupportedQuackVersion)
		w.WriteStringWithDefault(6, m.ClientID)
		w.WriteUintWithDefault(7, m.HeartbeatTimeoutSeconds)
	})
	return nil
}

func (m ConnectionResponse) encode(w *codec.Writer) error {
	w.WriteObject(func(w *codec.Writer) {
		w.WriteStringWithDefault(1, m.ServerDuckdbVersion)
		w.WriteStringWithDefault(2, m.ServerPlatform)
		w.WriteUintWithDefault(3, m.QuackVersion)
		w.WriteUintWithDefault(4, m.HeartbeatTimeoutSeconds)
	})
	return nil
}

func (m PrepareRequest) encode(w *codec.Writer) error {
	var err error
	w.WriteObject(func(w *codec.Writer) {
		w.WriteStringWithDefault(1, m.SQL)
		writeHugeInt(w, 2, m.QueryUUID)
		err = writeIdx(w, 3, "PrepareRequest inline_rows", m.InlineRows)
	})
	return err
}

func (m PrepareResponse) encode(w *codec.Writer) error {
	var err error
	w.WriteObject(func(w *codec.Writer) {
		if len(m.ResultTypes) > 0 {
			w.WriteField(1, func(w *codec.Writer) {
				w.WriteULEB128(uint64(len(m.ResultTypes)))
				for _, t := range m.ResultTypes {
					logicaltype.Encode(w, t)
				}
			})
		}
		if len(m.ResultNames) > 0 {
			w.WriteField(2, func(w *codec.Writer) {
				w.WriteULEB128(uint64(len(m.ResultNames)))
				for _, n := range m.ResultNames {
					w.WriteString(n)
				}
			})
		}
		writeBool(w, 3, m.NeedsMoreFetch)
		if len(m.Results) > 0 {
			// Each element is a present byte and a DataChunkWrapper {300: DataChunk}.
			w.WriteField(4, func(w *codec.Writer) {
				w.WriteULEB128(uint64(len(m.Results)))
				for i, c := range m.Results {
					w.WriteBool(true)
					w.WriteFieldID(300)
					if err = writeDataChunk(w, c); err != nil {
						err = fmt.Errorf("message: PrepareResponse result %d: %w", i, err)
						return
					}
					w.WriteEndObject()
				}
			})
		}
		writeHugeInt(w, 5, m.QueryUUID)
	})
	return err
}

func (m FetchRequest) encode(w *codec.Writer) error {
	w.WriteObject(func(w *codec.Writer) {
		writeHugeInt(w, 1, m.QueryUUID)
		w.WriteUintWithDefault(2, m.BatchIndex)
		w.WriteUintWithDefault(3, m.AckIndex)
	})
	return nil
}

func (m FetchResponse) encode(w *codec.Writer) error {
	var err error
	w.WriteObject(func(w *codec.Writer) {
		w.WriteUintWithDefault(1, uint64(len(m.Results)))
		if err = writeIdx(w, 2, "FetchResponse total_batches", m.TotalBatches); err == nil {
			err = writeIdx(w, 3, "FetchResponse batch_index", m.BatchIndex)
		}
	})
	if err != nil {
		return err
	}
	return writeChunks(w, "FetchResponse", m.Results)
}

func (m SendDataRequest) encode(w *codec.Writer) error {
	var err error
	w.WriteObject(func(w *codec.Writer) {
		w.WriteStringWithDefault(1, m.StreamID)
		w.WriteUintWithDefault(2, uint64(len(m.Chunks)))
		if err = writeIdx(w, 3, "SendDataRequest total_batches", m.TotalBatches); err == nil {
			err = writeIdx(w, 4, "SendDataRequest batch_index", m.BatchIndex)
		}
	})
	if err != nil {
		return err
	}
	return writeChunks(w, "SendDataRequest", m.Chunks)
}

// writeChunks appends the raw DataChunk objects that follow a FETCH_RESPONSE
// or SEND_DATA_REQUEST body. Upstream never sends an empty chunk
// (QuackChunkPayloadWriter::AppendChunk asserts size > 0), and
// scan_data_from_quack_client would take one for the end of the scan.
func writeChunks(w *codec.Writer, msg string, chunks []DataChunk) error {
	for i, c := range chunks {
		if c.RowCount == 0 {
			return fmt.Errorf("message: %s chunk %d has no rows", msg, i)
		}
		if err := writeDataChunk(w, c); err != nil {
			return fmt.Errorf("message: %s chunk %d: %w", msg, i, err)
		}
	}
	return nil
}

func (m SendDataResponse) encode(w *codec.Writer) error {
	var err error
	w.WriteObject(func(w *codec.Writer) {
		w.WriteField(1, func(w *codec.Writer) { err = w.WriteOptionalIdx(m.AcceptBudget) }) // always written
	})
	if err != nil {
		return fmt.Errorf("message: SendDataResponse accept_budget: %w", err)
	}
	return nil
}

func (m CancelRequest) encode(w *codec.Writer) error {
	w.WriteObject(func(w *codec.Writer) { writeHugeInt(w, 1, m.QueryUUID) })
	return nil
}

func (m Acknowledgement) encode(w *codec.Writer) error {
	w.WriteObject(func(w *codec.Writer) {
		if m.QueryUUID != (codec.HugeInt{}) { // written with the default hugeint_t(0, 0)
			writeHugeInt(w, 1, m.QueryUUID)
		}
	})
	return nil
}

func (HeartbeatRequest) encode(w *codec.Writer) error  { w.WriteEndObject(); return nil }
func (SuccessResponse) encode(w *codec.Writer) error   { w.WriteEndObject(); return nil }
func (DisconnectMessage) encode(w *codec.Writer) error { w.WriteEndObject(); return nil }

func (m ErrorResponse) encode(w *codec.Writer) error {
	w.WriteObject(func(w *codec.Writer) {
		w.WriteStringWithDefault(1, m.Message)
		w.WriteStringWithDefault(2, m.ExceptionType)
		if len(m.ExtraInfo) > 0 {
			w.WriteField(3, func(w *codec.Writer) { w.WriteStringMap(m.ExtraInfo) })
		}
		writeBool(w, 4, m.MustInvalidate)
	})
	return nil
}
