package message

import (
	"fmt"
	"math"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// Decode reads a header, a body and, for FETCH_RESPONSE and
// SEND_DATA_REQUEST, the chunk_count raw DataChunk objects that follow the
// body, then asserts that no bytes remain. Properties DuckDB writes "with
// default" may be absent and decode to their default; properties it always
// writes are required. Fields must appear in ascending id order, exactly as
// DuckDB's BinaryDeserializer expects; unknown fields are an error.
//
// Decoded DataChunks reference b (vector data is not copied), so b must not
// be modified or reused while they are in use.
func Decode(b []byte) (Header, Body, error) {
	r := codec.NewReader(b)
	h, err := decodeHeader(r)
	if err != nil {
		return Header{}, nil, fmt.Errorf("message: header: %w", err)
	}
	body, err := decodeBody(r, h.Type)
	if err != nil {
		return h, nil, fmt.Errorf("message: %s: %w", h.Type, err)
	}
	if err := r.AssertEOF(); err != nil {
		return h, body, fmt.Errorf("message: %s: %w", h.Type, err)
	}
	return h, body, nil
}

func decodeHeader(r *codec.Reader) (Header, error) {
	var h Header
	err := readFields(r,
		required(1, func(r *codec.Reader) error {
			typ, err := r.ReadULEB128()
			if err == nil && typ > math.MaxUint8 {
				err = fmt.Errorf("%w: %d", ErrUnknownMessageType, typ)
			}
			h.Type = Type(typ)
			return err
		}),
		strProp(2, &h.ConnectionID),
		required(3, func(r *codec.Reader) error {
			v, err := r.ReadOptionalIdx()
			h.HasQueryID, h.ClientQueryID = v.Valid, v.Value
			return err
		}),
	)
	return h, err
}

// prop reads one property of a message object.
type prop = func(*codec.Reader) error

// readFields reads an object made of props, in order.
func readFields(r *codec.Reader, props ...prop) error {
	return r.ReadObject(func(r *codec.Reader) error {
		for _, f := range props {
			if err := f(r); err != nil {
				return err
			}
		}
		return nil
	})
}

// required is a property DuckDB always writes.
func required(id uint16, read prop) prop {
	return func(r *codec.Reader) error { return r.ReadRequiredField(id, read) }
}

// optional is a property written "with default": absent means the default.
func optional(id uint16, read prop) prop {
	return func(r *codec.Reader) error {
		_, err := r.ReadOptionalField(id, read)
		return err
	}
}

func strProp(id uint16, dst *string) prop {
	return func(r *codec.Reader) error { return r.ReadStringWithDefault(id, dst) }
}

func uintProp(id uint16, dst *uint64) prop {
	return func(r *codec.Reader) error { return r.ReadUintWithDefault(id, dst) }
}

func boolProp(id uint16, dst *bool) prop {
	return optional(id, func(r *codec.Reader) (err error) { *dst, err = r.ReadBool(); return err })
}

func idxProp(id uint16, dst *OptionalIdx) prop {
	return optional(id, func(r *codec.Reader) (err error) { *dst, err = r.ReadOptionalIdx(); return err })
}

func hugeProp(id uint16, dst *codec.HugeInt) prop {
	return required(id, func(r *codec.Reader) (err error) { *dst, err = r.ReadHugeInt(); return err })
}

func decodeBody(r *codec.Reader, t Type) (Body, error) {
	switch t {
	case TypeConnectionRequest:
		var m ConnectionRequest
		err := readFields(r, strProp(1, &m.AuthString), strProp(2, &m.ClientDuckdbVersion), strProp(3, &m.ClientPlatform),
			uintProp(4, &m.MinSupportedQuackVersion), uintProp(5, &m.MaxSupportedQuackVersion), strProp(6, &m.ClientID),
			uintProp(7, &m.HeartbeatTimeoutSeconds))
		return m, err
	case TypeConnectionResponse:
		var m ConnectionResponse
		err := readFields(r, strProp(1, &m.ServerDuckdbVersion), strProp(2, &m.ServerPlatform), uintProp(3, &m.QuackVersion),
			uintProp(4, &m.HeartbeatTimeoutSeconds))
		return m, err
	case TypePrepareRequest:
		var m PrepareRequest
		err := readFields(r, strProp(1, &m.SQL), hugeProp(2, &m.QueryUUID), idxProp(3, &m.InlineRows))
		return m, err
	case TypePrepareResponse:
		var m PrepareResponse
		err := readFields(r,
			optional(1, func(r *codec.Reader) error {
				return readList(r, &m.ResultTypes, func(r *codec.Reader, i int) (logicaltype.Type, error) {
					t, err := logicaltype.Decode(r)
					if err != nil {
						err = fmt.Errorf("result type %d: %w", i, err)
					}
					return t, err
				})
			}),
			optional(2, func(r *codec.Reader) error {
				return readList(r, &m.ResultNames, func(r *codec.Reader, _ int) (string, error) { return r.ReadString() })
			}),
			boolProp(3, &m.NeedsMoreFetch),
			// vector<unique_ptr<DataChunkWrapper>>: a present byte, then
			// the wrapper object {300: DataChunk}. The server never sends
			// a null entry.
			optional(4, func(r *codec.Reader) error {
				return readList(r, &m.Results, func(r *codec.Reader, i int) (DataChunk, error) {
					present, err := r.ReadBool()
					if err == nil && !present {
						err = fmt.Errorf("null DataChunk in result list at index %d", i)
					}
					var c DataChunk
					if err == nil {
						err = readFields(r, required(300, func(r *codec.Reader) (err error) { c, err = decodeDataChunk(r); return err }))
					}
					if err != nil {
						err = fmt.Errorf("result chunk %d: %w", i, err)
					}
					return c, err
				})
			}),
			hugeProp(5, &m.QueryUUID))
		return m, err
	case TypeFetchRequest:
		var m FetchRequest
		err := readFields(r, hugeProp(1, &m.QueryUUID), uintProp(2, &m.BatchIndex), uintProp(3, &m.AckIndex))
		return m, err
	case TypeFetchResponse:
		var m FetchResponse
		var count uint64
		err := readFields(r, uintProp(1, &count), idxProp(2, &m.TotalBatches), idxProp(3, &m.BatchIndex))
		if err == nil {
			m.Results, err = readChunks(r, count)
		}
		return m, err
	case TypeSendDataRequest:
		var m SendDataRequest
		var count uint64
		err := readFields(r, strProp(1, &m.StreamID), uintProp(2, &count), idxProp(3, &m.TotalBatches), idxProp(4, &m.BatchIndex))
		if err == nil {
			m.Chunks, err = readChunks(r, count)
		}
		return m, err
	case TypeSendDataResponse:
		var m SendDataResponse
		err := readFields(r, required(1, func(r *codec.Reader) (err error) { m.AcceptBudget, err = r.ReadOptionalIdx(); return err }))
		return m, err
	case TypeCancelRequest:
		var m CancelRequest
		err := readFields(r, hugeProp(1, &m.QueryUUID))
		return m, err
	case TypeAcknowledgement:
		var m Acknowledgement
		err := readFields(r, optional(1, func(r *codec.Reader) (err error) { m.QueryUUID, err = r.ReadHugeInt(); return err }))
		return m, err
	case TypeHeartbeatRequest:
		return HeartbeatRequest{}, r.ReadEndObject()
	case TypeSuccessResponse:
		return SuccessResponse{}, r.ReadEndObject()
	case TypeDisconnectMessage:
		return DisconnectMessage{}, r.ReadEndObject()
	case TypeErrorResponse:
		var m ErrorResponse
		err := readFields(r, strProp(1, &m.Message), strProp(2, &m.ExceptionType),
			optional(3, func(r *codec.Reader) (err error) { m.ExtraInfo, err = r.ReadStringMap(); return err }),
			boolProp(4, &m.MustInvalidate))
		return m, err
	}
	return nil, fmt.Errorf("%w: %d", ErrUnknownMessageType, t)
}

// readList reads a list whose count comes from the wire into *dst.
func readList[T any](r *codec.Reader, dst *[]T, read func(*codec.Reader, int) (T, error)) error {
	n, err := r.ReadCount()
	if err != nil {
		return err
	}
	*dst = make([]T, 0, min(n, codec.MaxPrealloc))
	for i := range n {
		v, err := read(r, i)
		if err != nil {
			return err
		}
		*dst = append(*dst, v)
	}
	return nil
}

// readChunks reads the count raw DataChunk objects that follow a
// FETCH_RESPONSE or SEND_DATA_REQUEST body.
func readChunks(r *codec.Reader, count uint64) ([]DataChunk, error) {
	if count == 0 {
		return nil, nil
	}
	// Every DataChunk takes several bytes.
	if count > uint64(r.Remaining()) {
		return nil, fmt.Errorf("chunk_count %d exceeds the %d remaining byte(s)", count, r.Remaining())
	}
	out := make([]DataChunk, 0, min(count, codec.MaxPrealloc))
	for i := range count {
		c, err := decodeDataChunk(r)
		if err != nil {
			return nil, fmt.Errorf("chunk %d of %d: %w", i, count, err)
		}
		out = append(out, c)
	}
	return out, nil
}
