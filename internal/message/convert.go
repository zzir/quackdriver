package message

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/zzir/quackdriver/internal/logicaltype"
	"github.com/zzir/quackdriver/internal/value"
)

// fixedBoxer returns the element width of a fixed-size type and the function
// that boxes one element into the Go value documented for the logical type.
func fixedBoxer(t logicaltype.Type) (int, func([]byte) any, error) {
	phys := t.PhysicalType()
	width, err := logicaltype.FixedSize(phys)
	if err != nil {
		return 0, nil, fmt.Errorf("message: cannot decode vector of type %s", t.Name())
	}
	le := binary.LittleEndian
	var box func([]byte) any
	switch t.ID {
	case logicaltype.IDBoolean:
		box = func(b []byte) any { return b[0] != 0 }
	case logicaltype.IDTinyint:
		box = func(b []byte) any { return int8(b[0]) }
	case logicaltype.IDUTinyint:
		box = func(b []byte) any { return b[0] }
	case logicaltype.IDSmallint:
		box = func(b []byte) any { return int16(le.Uint16(b)) }
	case logicaltype.IDUSmallint:
		box = func(b []byte) any { return le.Uint16(b) }
	case logicaltype.IDInteger:
		box = func(b []byte) any { return int32(le.Uint32(b)) }
	case logicaltype.IDUInteger:
		box = func(b []byte) any { return le.Uint32(b) }
	case logicaltype.IDBigint:
		box = func(b []byte) any { return int64(le.Uint64(b)) }
	case logicaltype.IDUBigint, logicaltype.IDPointer:
		box = func(b []byte) any { return le.Uint64(b) }
	case logicaltype.IDFloat:
		box = func(b []byte) any { return math.Float32frombits(le.Uint32(b)) }
	case logicaltype.IDDouble:
		box = func(b []byte) any { return math.Float64frombits(le.Uint64(b)) }
	case logicaltype.IDDate:
		box = func(b []byte) any { return value.DateFromDays(int32(le.Uint32(b))) }
	case logicaltype.IDTime:
		box = func(b []byte) any { return time.Duration(int64(le.Uint64(b))) * time.Microsecond }
	case logicaltype.IDTimeNs:
		box = func(b []byte) any { return time.Duration(int64(le.Uint64(b))) }
	case logicaltype.IDTimeTz:
		box = func(b []byte) any { return value.TimeTzFromBits(le.Uint64(b)) }
	case logicaltype.IDTimestampSec, logicaltype.IDTimestampMs, logicaltype.IDTimestamp, logicaltype.IDTimestampTz,
		logicaltype.IDTimestampNs, logicaltype.IDTimestampTzNs:
		perSecond := timestampUnits(t.ID)
		box = func(b []byte) any { return value.TimestampFromUnits(int64(le.Uint64(b)), perSecond) }
	case logicaltype.IDHugeint:
		box = func(b []byte) any { return value.HugeInt(int64(le.Uint64(b[8:])), le.Uint64(b)) }
	case logicaltype.IDUHugeint:
		box = func(b []byte) any { return value.UHugeInt(le.Uint64(b[8:]), le.Uint64(b)) }
	case logicaltype.IDUUID:
		box = func(b []byte) any { return value.UUIDString(int64(le.Uint64(b[8:])), le.Uint64(b)) }
	case logicaltype.IDInterval:
		box = func(b []byte) any {
			return IntervalValue{Months: int32(le.Uint32(b)), Days: int32(le.Uint32(b[4:])), Micros: int64(le.Uint64(b[8:]))}
		}
	case logicaltype.IDDecimal:
		scale := int(t.Info.Scale) // PhysicalType is valid, so Info is DECIMAL info
		switch phys {
		case logicaltype.PhysicalInt16:
			box = func(b []byte) any { return value.FormatDecimal64(int64(int16(le.Uint16(b))), scale) }
		case logicaltype.PhysicalInt32:
			box = func(b []byte) any { return value.FormatDecimal64(int64(int32(le.Uint32(b))), scale) }
		case logicaltype.PhysicalInt64:
			box = func(b []byte) any { return value.FormatDecimal64(int64(le.Uint64(b)), scale) }
		default:
			box = func(b []byte) any {
				return value.FormatDecimal(value.HugeInt(int64(le.Uint64(b[8:])), le.Uint64(b)), scale)
			}
		}
	case logicaltype.IDEnum:
		labels := t.Info.EnumValues // PhysicalType is valid, so Info is ENUM info
		box = func(b []byte) any { return labels[enumIndexAt(b, width)] }
	default:
		return 0, nil, fmt.Errorf("message: cannot decode vector of type %s", t.Name())
	}
	return width, box, nil
}

// enumIndexAt reads an ENUM dictionary index of the given width.
func enumIndexAt(b []byte, width int) uint32 {
	switch width {
	case 1:
		return uint32(b[0])
	case 2:
		return uint32(binary.LittleEndian.Uint16(b))
	}
	return binary.LittleEndian.Uint32(b)
}

// timestampUnits returns the units per second of a TIMESTAMP-family type.
func timestampUnits(id logicaltype.ID) int64 {
	switch id {
	case logicaltype.IDTimestampSec:
		return value.Seconds
	case logicaltype.IDTimestampMs:
		return value.Milliseconds
	case logicaltype.IDTimestampNs, logicaltype.IDTimestampTzNs:
		return value.Nanoseconds
	}
	return value.Microseconds
}

// encodeBit is the inverse of value.BitString, following Bit::ToBit and
// Bit::Finalize (padding bits are set to 1).
func encodeBit(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("BIT value must have at least one bit")
	}
	padding := (8 - len(s)%8) % 8
	out := make([]byte, 1+(len(s)+padding)/8)
	out[0] = byte(padding)
	out[1] = ^byte(0xff >> padding)
	for i := range len(s) {
		bit := i + padding
		switch s[i] {
		case '1':
			out[1+bit/8] |= 0x80 >> (bit % 8)
		case '0':
		default:
			return nil, fmt.Errorf("invalid character %q in BIT value", s[i])
		}
	}
	return out, nil
}

// maxBignumBytes is the largest data byte count the 23-bit header can hold.
const maxBignumBytes = 1<<23 - 1

// encodeBignum is the inverse of value.Bignum, matching IntToBignum /
// Bignum::FromByteArray (minimal magnitude bytes, zero is one 0x00 byte).
func encodeBignum(v *big.Int) ([]byte, error) {
	mag := new(big.Int).Abs(v).Bytes()
	if len(mag) == 0 {
		mag = []byte{0}
	}
	if len(mag) > maxBignumBytes {
		return nil, fmt.Errorf("BIGNUM value needs %d bytes, maximum is %d", len(mag), maxBignumBytes)
	}
	header := uint32(len(mag)) | 0x800000
	out := append([]byte{byte(header >> 16), byte(header >> 8), byte(header)}, mag...)
	if v.Sign() < 0 {
		for i := range out {
			out[i] = ^out[i]
		}
	}
	return out, nil
}
