package btreeproto

import (
	"encoding/binary"
	"errors"
	"math"
)

// ColType matches ADR-0002's five canonical types plus NULL.
type ColType uint8

const (
	TypeNull ColType = iota
	TypeInt
	TypeReal
	TypeText
	TypeBlob
	TypeBool
)

// Value is a compact 40-byte scalar representation (1 pointer field instead of 2)
// avoiding separate string/[]byte dual-pointer GC overhead in hot scan loops.
type Value struct {
	Raw  []byte // TEXT (UTF-8 bytes) or BLOB payload
	Num  uint64 // int64 bits, float64 bits, or 0/1 for bool
	Type ColType
}

func NullValue() Value        { return Value{Type: TypeNull} }
func IntValue(v int64) Value  { return Value{Type: TypeInt, Num: uint64(v)} }
func BoolValue(v bool) Value {
	var n uint64
	if v {
		n = 1
	}
	return Value{Type: TypeBool, Num: n}
}
func RealValue(v float64) Value { return Value{Type: TypeReal, Num: math.Float64bits(v)} }
func TextValue(s []byte) Value  { return Value{Type: TypeText, Raw: s} }
func BlobValue(b []byte) Value  { return Value{Type: TypeBlob, Raw: b} }

func (v Value) AsInt() int64     { return int64(v.Num) }
func (v Value) AsReal() float64  { return math.Float64frombits(v.Num) }
func (v Value) AsBool() bool     { return v.Num != 0 }

var (
	ErrInvalidFloat   = errors.New("invalid float: NaN or Inf not permitted")
	ErrCorruptKey     = errors.New("corrupt memcomparable key")
	ErrCorruptRecord  = errors.New("corrupt row record")
	ErrColOutOfBounds = errors.New("column index out of bounds")
)

const (
	tagNull    byte = 0x00
	tagNonNull byte = 0x01
)

// AppendMemKey appends order-preserving memcomparable bytes for vals into dst.
// Bit i of descMask marks column i as DESC (bitwise-inverting its encoded bytes,
// which automatically maps NULLS FIRST on ASC to NULLS LAST on DESC).
func AppendMemKey(dst []byte, vals []Value, descMask uint64) ([]byte, error) {
	for i, v := range vals {
		start := len(dst)
		switch v.Type {
		case TypeNull:
			dst = append(dst, tagNull)
		case TypeBool:
			if v.Num != 0 {
				dst = append(dst, tagNonNull, 0x01)
			} else {
				dst = append(dst, tagNonNull, 0x00)
			}
		case TypeInt:
			u := v.Num ^ (uint64(1) << 63)
			dst = append(dst, tagNonNull)
			dst = binary.BigEndian.AppendUint64(dst, u)
		case TypeReal:
			f := math.Float64frombits(v.Num)
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return dst[:start], ErrInvalidFloat
			}
			var bits uint64
			if f == 0 {
				bits = 0 // normalize -0.0 to +0.0
			} else {
				bits = math.Float64bits(f)
			}
			if bits>>63 != 0 {
				bits = ^bits
			} else {
				bits ^= uint64(1) << 63
			}
			dst = append(dst, tagNonNull)
			dst = binary.BigEndian.AppendUint64(dst, bits)
		case TypeText, TypeBlob:
			dst = append(dst, tagNonNull)
			for _, b := range v.Raw {
				if b == 0x00 {
					dst = append(dst, 0x00, 0xFF)
				} else {
					dst = append(dst, b)
				}
			}
			dst = append(dst, 0x00, 0x00)
		default:
			return dst[:start], ErrCorruptKey
		}

		if (descMask & (uint64(1) << uint(i))) != 0 {
			for j := start; j < len(dst); j++ {
				dst[j] = ^dst[j]
			}
		}
	}
	return dst, nil
}

// DecodeMemKeyInto decodes a memcomparable key into caller-owned dst slice and
// scratch byte buffer (for unescaping TEXT/BLOB) with zero heap allocations on reuse.
func DecodeMemKeyInto(raw []byte, schema []ColType, descMask uint64, dst []Value, scratch []byte) ([]Value, []byte, error) {
	dst = dst[:0]
	scratch = scratch[:0]
	pos := 0
	for i, typ := range schema {
		if pos >= len(raw) {
			return dst, scratch, ErrCorruptKey
		}
		inv := (descMask & (uint64(1) << uint(i))) != 0
		tag := raw[pos]
		if inv {
			tag = ^tag
		}
		pos++
		if tag == tagNull {
			dst = append(dst, NullValue())
			continue
		}
		if tag != tagNonNull {
			return dst, scratch, ErrCorruptKey
		}
		switch typ {
		case TypeBool:
			if pos >= len(raw) {
				return dst, scratch, ErrCorruptKey
			}
			b := raw[pos]
			if inv {
				b = ^b
			}
			pos++
			dst = append(dst, BoolValue(b != 0))
		case TypeInt:
			if pos+8 > len(raw) {
				return dst, scratch, ErrCorruptKey
			}
			u := readBE64(raw[pos:pos+8], inv)
			pos += 8
			dst = append(dst, IntValue(int64(u^(uint64(1)<<63))))
		case TypeReal:
			if pos+8 > len(raw) {
				return dst, scratch, ErrCorruptKey
			}
			bits := readBE64(raw[pos:pos+8], inv)
			pos += 8
			if bits>>63 != 0 {
				bits ^= uint64(1) << 63
			} else {
				bits = ^bits
			}
			dst = append(dst, RealValue(math.Float64frombits(bits)))
		case TypeText, TypeBlob:
			sStart := len(scratch)
			terminated := false
			for pos < len(raw) {
				b := raw[pos]
				if inv {
					b = ^b
				}
				pos++
				if b == 0x00 {
					if pos >= len(raw) {
						return dst, scratch, ErrCorruptKey
					}
					next := raw[pos]
					if inv {
						next = ^next
					}
					pos++
					if next == 0x00 {
						terminated = true
						break
					}
					if next == 0xFF {
						scratch = append(scratch, 0x00)
						continue
					}
					return dst, scratch, ErrCorruptKey
				}
				scratch = append(scratch, b)
			}
			if !terminated {
				return dst, scratch, ErrCorruptKey
			}
			val := Value{Type: typ, Raw: scratch[sStart:len(scratch)]}
			dst = append(dst, val)
		default:
			return dst, scratch, ErrCorruptKey
		}
	}
	if pos != len(raw) {
		return dst, scratch, ErrCorruptKey
	}
	return dst, scratch, nil
}

func readBE64(b []byte, inv bool) uint64 {
	u := binary.BigEndian.Uint64(b)
	if inv {
		return ^u
	}
	return u
}

// AppendRecord encodes a row using ADR-0005's compact header layout:
// [ColCount (uvarint)][NullBitmap ((N+7)/8)][VarintLenHeader (non-NULL var-len or fixed-len skipped)][Payloads]
// Note: Fixed-width types (INT=8B, REAL=8B, BOOL=1B) omit varint length entries?
// To remain schema-independent for header skipping AND super compact, we store
// uvarint payload length for EVERY non-NULL column (1B for <=127B, e.g. 8 for INT/REAL, 1 for BOOL).
func AppendRecord(dst []byte, vals []Value) []byte {
	n := len(vals)
	dst = binary.AppendUvarint(dst, uint64(n))

	bmBytes := (n + 7) / 8
	bmStart := len(dst)
	for i := 0; i < bmBytes; i++ {
		dst = append(dst, 0)
	}
	for i, v := range vals {
		if v.Type == TypeNull {
			dst[bmStart+i/8] |= 1 << uint(i%8)
		}
	}

	// Varint length header for all non-NULL columns.
	for _, v := range vals {
		switch v.Type {
		case TypeNull:
			// omitted
		case TypeBool:
			dst = binary.AppendUvarint(dst, 1)
		case TypeInt, TypeReal:
			dst = binary.AppendUvarint(dst, 8)
		case TypeText, TypeBlob:
			dst = binary.AppendUvarint(dst, uint64(len(v.Raw)))
		}
	}

	// Payloads.
	for _, v := range vals {
		switch v.Type {
		case TypeNull:
		case TypeBool:
			if v.Num != 0 {
				dst = append(dst, 1)
			} else {
				dst = append(dst, 0)
			}
		case TypeInt, TypeReal:
			dst = binary.LittleEndian.AppendUint64(dst, v.Num)
		case TypeText, TypeBlob:
			dst = append(dst, v.Raw...)
		}
	}
	return dst
}

// DecodeRecordInto decodes all columns of a row into caller-owned dst without allocating.
// Returned TEXT/BLOB slices borrow raw directly (valid while raw is pinned/alive).
func DecodeRecordInto(raw []byte, schema []ColType, dst []Value) ([]Value, error) {
	colCount, n := binary.Uvarint(raw)
	if n <= 0 || int(colCount) != len(schema) {
		return dst[:0], ErrCorruptRecord
	}
	pos := n
	bmBytes := (int(colCount) + 7) / 8
	if pos+bmBytes > len(raw) {
		return dst[:0], ErrCorruptRecord
	}
	bm := raw[pos : pos+bmBytes]
	pos += bmBytes

	// First pass: find where payloads start while recording lengths on stack (fast path <= 64 cols).
	var stackLens [64]uint32
	var lens []uint32
	if int(colCount) <= len(stackLens) {
		lens = stackLens[:colCount]
	} else {
		lens = make([]uint32, colCount)
	}
	for i := 0; i < int(colCount); i++ {
		if (bm[i/8] & (1 << uint(i%8))) != 0 {
			continue
		}
		l, k := binary.Uvarint(raw[pos:])
		if k <= 0 {
			return dst[:0], ErrCorruptRecord
		}
		pos += k
		lens[i] = uint32(l)
	}

	dst = dst[:0]
	for i := 0; i < int(colCount); i++ {
		if (bm[i/8] & (1 << uint(i%8))) != 0 {
			dst = append(dst, NullValue())
			continue
		}
		l := int(lens[i])
		if pos+l > len(raw) {
			return dst[:0], ErrCorruptRecord
		}
		chunk := raw[pos : pos+l]
		pos += l
		switch schema[i] {
		case TypeBool:
			if l != 1 {
				return dst[:0], ErrCorruptRecord
			}
			dst = append(dst, BoolValue(chunk[0] != 0))
		case TypeInt:
			if l != 8 {
				return dst[:0], ErrCorruptRecord
			}
			dst = append(dst, IntValue(int64(binary.LittleEndian.Uint64(chunk))))
		case TypeReal:
			if l != 8 {
				return dst[:0], ErrCorruptRecord
			}
			dst = append(dst, Value{Type: TypeReal, Num: binary.LittleEndian.Uint64(chunk)})
		case TypeText, TypeBlob:
			dst = append(dst, Value{Type: schema[i], Raw: chunk})
		default:
			return dst[:0], ErrCorruptRecord
		}
	}
	return dst, nil
}

// DecodeColumn decodes a single projected column colIdx directly by summing header
// lengths without decoding or touching payloads of columns 0..colIdx-1.
func DecodeColumn(raw []byte, colIdx int, typ ColType) (Value, error) {
	colCount, n := binary.Uvarint(raw)
	if n <= 0 || colIdx < 0 || colIdx >= int(colCount) {
		return Value{}, ErrColOutOfBounds
	}
	pos := n
	bmBytes := (int(colCount) + 7) / 8
	if pos+bmBytes > len(raw) {
		return Value{}, ErrCorruptRecord
	}
	bm := raw[pos : pos+bmBytes]
	pos += bmBytes

	if (bm[colIdx/8] & (1 << uint(colIdx%8))) != 0 {
		return NullValue(), nil
	}

	var skipPayload int
	var targetLen int
	for i := 0; i < int(colCount); i++ {
		if (bm[i/8] & (1 << uint(i%8))) != 0 {
			continue
		}
		l, k := binary.Uvarint(raw[pos:])
		if k <= 0 {
			return Value{}, ErrCorruptRecord
		}
		pos += k
		if i < colIdx {
			skipPayload += int(l)
		} else if i == colIdx {
			targetLen = int(l)
		}
	}

	pStart := pos + skipPayload
	pEnd := pStart + targetLen
	if pEnd > len(raw) {
		return Value{}, ErrCorruptRecord
	}
	chunk := raw[pStart:pEnd]
	switch typ {
	case TypeBool:
		if targetLen != 1 {
			return Value{}, ErrCorruptRecord
		}
		return BoolValue(chunk[0] != 0), nil
	case TypeInt:
		if targetLen != 8 {
			return Value{}, ErrCorruptRecord
		}
		return IntValue(int64(binary.LittleEndian.Uint64(chunk))), nil
	case TypeReal:
		if targetLen != 8 {
			return Value{}, ErrCorruptRecord
		}
		return Value{Type: TypeReal, Num: binary.LittleEndian.Uint64(chunk)}, nil
	case TypeText, TypeBlob:
		return Value{Type: typ, Raw: chunk}, nil
	default:
		return Value{}, ErrCorruptRecord
	}
}
