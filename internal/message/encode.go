package message

import (
	"fmt"

	"github.com/zzir/quackdriver/internal/codec"
	"github.com/zzir/quackdriver/internal/logicaltype"
)

// Encode serializes a message header and body to a single byte buffer.
func Encode(h Header, body Body) ([]byte, error) {
	if body == nil {
		return nil, fmt.Errorf("message: nil body")
	}
	h.Type = body.MessageType()
	w := codec.NewWriter()
	encodeHeader(w, h)
	body.encode(w)
	return w.Bytes(), nil
}

func encodeHeader(w *codec.Writer, h Header) {
	w.WriteObject(func(w *codec.Writer) {
		w.WriteField(1, func(w *codec.Writer) { w.WriteULEB128(uint64(h.Type)) })
		if h.ConnectionID != "" {
			w.WriteField(2, func(w *codec.Writer) { w.WriteString(h.ConnectionID) })
		}
		w.WriteField(3, func(w *codec.Writer) {
			if h.HasQueryID {
				w.WriteULEB128(h.ClientQueryID)
			} else {
				w.WriteULEB128(optionalIndexInvalid)
			}
		})
	})
}

func writeOptionalString(w *codec.Writer, id uint16, s string) {
	if s == "" {
		return
	}
	w.WriteField(id, func(w *codec.Writer) { w.WriteString(s) })
}

func writeOptionalULEBNonZero(w *codec.Writer, id uint16, v uint64) {
	if v == 0 {
		return
	}
	w.WriteField(id, func(w *codec.Writer) { w.WriteULEB128(v) })
}

func (m ConnectionRequest) encode(w *codec.Writer) {
	w.WriteObject(func(w *codec.Writer) {
		writeOptionalString(w, 1, m.AuthString)
		writeOptionalString(w, 2, m.ClientDuckdbVersion)
		writeOptionalString(w, 3, m.ClientPlatform)
		writeOptionalULEBNonZero(w, 4, m.MinSupportedQuackVersion)
		writeOptionalULEBNonZero(w, 5, m.MaxSupportedQuackVersion)
	})
}

func (m ConnectionResponse) encode(w *codec.Writer) {
	w.WriteObject(func(w *codec.Writer) {
		writeOptionalString(w, 1, m.ServerDuckdbVersion)
		writeOptionalString(w, 2, m.ServerPlatform)
		if m.HasQuackVersion {
			w.WriteField(3, func(w *codec.Writer) { w.WriteULEB128(m.QuackVersion) })
		}
	})
}

func (m PrepareRequest) encode(w *codec.Writer) {
	w.WriteObject(func(w *codec.Writer) {
		writeOptionalString(w, 1, m.SQL)
	})
}

func (m PrepareResponse) encode(w *codec.Writer) {
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
		if m.NeedsMoreFetch {
			w.WriteField(3, func(w *codec.Writer) { w.WriteBool(true) })
		}
		if len(m.Results) > 0 {
			w.WriteField(4, func(w *codec.Writer) { writeChunkPointerList(w, m.Results) })
		}
		w.WriteField(5, func(w *codec.Writer) { w.WriteHugeInt(m.ResultUUID) })
	})
}

func (m FetchRequest) encode(w *codec.Writer) {
	w.WriteObject(func(w *codec.Writer) {
		w.WriteField(1, func(w *codec.Writer) { w.WriteHugeInt(m.ResultUUID) })
	})
}

func (m FetchResponse) encode(w *codec.Writer) {
	w.WriteObject(func(w *codec.Writer) {
		if len(m.Results) > 0 {
			w.WriteField(1, func(w *codec.Writer) { writeChunkPointerList(w, m.Results) })
		}
		w.WriteField(2, func(w *codec.Writer) {
			if m.HasBatchIndex {
				w.WriteULEB128(m.BatchIndex)
			} else {
				w.WriteULEB128(optionalIndexInvalid)
			}
		})
	})
}

func (m AppendRequest) encode(w *codec.Writer) {
	w.WriteObject(func(w *codec.Writer) {
		writeOptionalString(w, 1, m.SchemaName)
		writeOptionalString(w, 2, m.TableName)
		w.WriteField(3, func(w *codec.Writer) {
			w.WriteBool(true)
			encodeDataChunkWrapper(w, m.AppendChunk)
		})
	})
}

func (SuccessResponse) encode(w *codec.Writer)   { w.WriteObject(func(*codec.Writer) {}) }
func (DisconnectMessage) encode(w *codec.Writer) { w.WriteObject(func(*codec.Writer) {}) }

func (m ErrorResponse) encode(w *codec.Writer) {
	w.WriteObject(func(w *codec.Writer) {
		writeOptionalString(w, 1, m.Message)
	})
}

func writeChunkPointerList(w *codec.Writer, chunks []DataChunk) {
	w.WriteULEB128(uint64(len(chunks)))
	for _, c := range chunks {
		w.WriteBool(true)
		encodeDataChunkWrapper(w, c)
	}
}
