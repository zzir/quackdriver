package message

import (
	"fmt"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// Decode reads a header + body from b.
func Decode(b []byte) (Header, Body, error) {
	r := codec.NewReader(b)
	h, err := decodeHeader(r)
	if err != nil {
		return Header{}, nil, err
	}
	body, err := decodeBody(r, h.Type)
	if err != nil {
		return h, nil, err
	}
	if err := r.AssertEOF(); err != nil {
		return h, body, fmt.Errorf("message: %w", err)
	}
	return h, body, nil
}

func decodeHeader(r *codec.Reader) (Header, error) {
	var h Header
	err := r.ReadObject(func(r *codec.Reader) error {
		if err := r.ReadRequiredField(1, func(r *codec.Reader) error {
			v, err := r.ReadULEB128()
			h.Type = Type(v)
			return err
		}); err != nil {
			return err
		}
		if _, err := r.ReadOptionalField(2, func(r *codec.Reader) error {
			v, err := r.ReadString()
			h.ConnectionID = v
			return err
		}); err != nil {
			return err
		}
		return r.ReadRequiredField(3, func(r *codec.Reader) error {
			v, err := r.ReadULEB128()
			if err != nil {
				return err
			}
			if v != optionalIndexInvalid {
				h.HasQueryID = true
				h.ClientQueryID = v
			}
			return nil
		})
	})
	return h, err
}

func decodeBody(r *codec.Reader, t Type) (Body, error) {
	switch t {
	case TypeConnectionRequest:
		var m ConnectionRequest
		err := r.ReadObject(func(r *codec.Reader) error {
			if _, err := r.ReadOptionalField(1, func(r *codec.Reader) error {
				v, err := r.ReadString()
				m.AuthString = v
				return err
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(2, func(r *codec.Reader) error {
				v, err := r.ReadString()
				m.ClientDuckdbVersion = v
				return err
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(3, func(r *codec.Reader) error {
				v, err := r.ReadString()
				m.ClientPlatform = v
				return err
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(4, func(r *codec.Reader) error {
				v, err := r.ReadULEB128()
				m.MinSupportedQuackVersion = v
				return err
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(5, func(r *codec.Reader) error {
				v, err := r.ReadULEB128()
				m.MaxSupportedQuackVersion = v
				return err
			}); err != nil {
				return err
			}
			return nil
		})
		return m, err

	case TypeConnectionResponse:
		var m ConnectionResponse
		err := r.ReadObject(func(r *codec.Reader) error {
			if _, err := r.ReadOptionalField(1, func(r *codec.Reader) error {
				v, err := r.ReadString()
				m.ServerDuckdbVersion = v
				return err
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(2, func(r *codec.Reader) error {
				v, err := r.ReadString()
				m.ServerPlatform = v
				return err
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(3, func(r *codec.Reader) error {
				v, err := r.ReadULEB128()
				if err != nil {
					return err
				}
				m.HasQuackVersion = true
				m.QuackVersion = v
				return nil
			}); err != nil {
				return err
			}
			return nil
		})
		return m, err

	case TypePrepareRequest:
		var m PrepareRequest
		err := r.ReadObject(func(r *codec.Reader) error {
			_, err := r.ReadOptionalField(1, func(r *codec.Reader) error {
				v, err := r.ReadString()
				m.SQL = v
				return err
			})
			return err
		})
		return m, err

	case TypePrepareResponse:
		var m PrepareResponse
		err := r.ReadObject(func(r *codec.Reader) error {
			if _, err := r.ReadOptionalField(1, func(r *codec.Reader) error {
				n, err := r.ReadULEB128()
				if err != nil {
					return err
				}
				m.ResultTypes = make([]logicaltype.Type, int(n))
				for i := range m.ResultTypes {
					t, err := logicaltype.Decode(r)
					if err != nil {
						return err
					}
					m.ResultTypes[i] = t
				}
				return nil
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(2, func(r *codec.Reader) error {
				n, err := r.ReadULEB128()
				if err != nil {
					return err
				}
				m.ResultNames = make([]string, int(n))
				for i := range m.ResultNames {
					s, err := r.ReadString()
					if err != nil {
						return err
					}
					m.ResultNames[i] = s
				}
				return nil
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(3, func(r *codec.Reader) error {
				v, err := r.ReadBool()
				m.NeedsMoreFetch = v
				return err
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(4, func(r *codec.Reader) error {
				chunks, err := readChunkPointerList(r)
				m.Results = chunks
				return err
			}); err != nil {
				return err
			}
			return r.ReadRequiredField(5, func(r *codec.Reader) error {
				v, err := r.ReadHugeInt()
				m.ResultUUID = v
				return err
			})
		})
		return m, err

	case TypeFetchRequest:
		var m FetchRequest
		err := r.ReadObject(func(r *codec.Reader) error {
			return r.ReadRequiredField(1, func(r *codec.Reader) error {
				v, err := r.ReadHugeInt()
				m.ResultUUID = v
				return err
			})
		})
		return m, err

	case TypeFetchResponse:
		var m FetchResponse
		err := r.ReadObject(func(r *codec.Reader) error {
			if _, err := r.ReadOptionalField(1, func(r *codec.Reader) error {
				chunks, err := readChunkPointerList(r)
				m.Results = chunks
				return err
			}); err != nil {
				return err
			}
			return r.ReadRequiredField(2, func(r *codec.Reader) error {
				v, err := r.ReadULEB128()
				if err != nil {
					return err
				}
				if v != optionalIndexInvalid {
					m.HasBatchIndex = true
					m.BatchIndex = v
				}
				return nil
			})
		})
		return m, err

	case TypeAppendRequest:
		var m AppendRequest
		err := r.ReadObject(func(r *codec.Reader) error {
			if _, err := r.ReadOptionalField(1, func(r *codec.Reader) error {
				v, err := r.ReadString()
				m.SchemaName = v
				return err
			}); err != nil {
				return err
			}
			if _, err := r.ReadOptionalField(2, func(r *codec.Reader) error {
				v, err := r.ReadString()
				m.TableName = v
				return err
			}); err != nil {
				return err
			}
			return r.ReadRequiredField(3, func(r *codec.Reader) error {
				present, err := r.ReadBool()
				if err != nil {
					return err
				}
				if !present {
					return fmt.Errorf("message: AppendRequest is missing append_chunk")
				}
				chunk, err := decodeDataChunkWrapper(r)
				m.AppendChunk = chunk
				return err
			})
		})
		return m, err

	case TypeSuccessResponse:
		var m SuccessResponse
		err := r.ReadEndObject()
		return m, err

	case TypeDisconnectMessage:
		var m DisconnectMessage
		err := r.ReadEndObject()
		return m, err

	case TypeErrorResponse:
		var m ErrorResponse
		err := r.ReadObject(func(r *codec.Reader) error {
			_, err := r.ReadOptionalField(1, func(r *codec.Reader) error {
				v, err := r.ReadString()
				m.Message = v
				return err
			})
			return err
		})
		return m, err
	}
	return nil, fmt.Errorf("%w: %d", ErrUnknownMessageType, t)
}

func readChunkPointerList(r *codec.Reader) ([]DataChunk, error) {
	n, err := r.ReadULEB128()
	if err != nil {
		return nil, err
	}
	out := make([]DataChunk, int(n))
	for i := range out {
		present, err := r.ReadBool()
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, fmt.Errorf("message: null DataChunk pointer in result list")
		}
		c, err := decodeDataChunkWrapper(r)
		if err != nil {
			return nil, err
		}
		out[i] = c
	}
	return out, nil
}
