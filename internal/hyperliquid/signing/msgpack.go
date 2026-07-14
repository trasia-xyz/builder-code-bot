package signing

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"

	"builder-code-bot/internal/hyperliquid/wire"
)

func packMsgpack(value any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeMsgpackValue(&buf, value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeMsgpackValue(buf *bytes.Buffer, value any) error {
	if value == nil {
		buf.WriteByte(0xc0)
		return nil
	}
	switch v := value.(type) {
	case wire.Object:
		return encodeMsgpackObject(buf, v)
	case []wire.Field:
		return encodeMsgpackObject(buf, wire.Object(v))
	case []any:
		return encodeMsgpackArray(buf, v)
	case string:
		return encodeMsgpackString(buf, v)
	case bool:
		if v {
			buf.WriteByte(0xc3)
		} else {
			buf.WriteByte(0xc2)
		}
		return nil
	case int:
		return encodeMsgpackInt(buf, int64(v))
	case int8:
		return encodeMsgpackInt(buf, int64(v))
	case int16:
		return encodeMsgpackInt(buf, int64(v))
	case int32:
		return encodeMsgpackInt(buf, int64(v))
	case int64:
		return encodeMsgpackInt(buf, v)
	case uint:
		return encodeMsgpackUint(buf, uint64(v))
	case uint8:
		return encodeMsgpackUint(buf, uint64(v))
	case uint16:
		return encodeMsgpackUint(buf, uint64(v))
	case uint32:
		return encodeMsgpackUint(buf, uint64(v))
	case uint64:
		return encodeMsgpackUint(buf, v)
	}
	return fmt.Errorf("unsupported msgpack value type %T; Hyperliquid actions must use signing.Object and supported scalar or array values", value)
}

func encodeMsgpackObject(buf *bytes.Buffer, obj wire.Object) error {
	if err := encodeMsgpackMapLen(buf, len(obj)); err != nil {
		return err
	}
	for _, field := range obj {
		if field.Key == "" {
			return fmt.Errorf("msgpack object field key is empty")
		}
		if err := encodeMsgpackString(buf, field.Key); err != nil {
			return err
		}
		if err := encodeMsgpackValue(buf, field.Value); err != nil {
			return fmt.Errorf("encode field %q: %w", field.Key, err)
		}
	}
	return nil
}

func encodeMsgpackArray(buf *bytes.Buffer, values []any) error {
	if err := encodeMsgpackArrayLen(buf, len(values)); err != nil {
		return err
	}
	for i, value := range values {
		if err := encodeMsgpackValue(buf, value); err != nil {
			return fmt.Errorf("encode array index %d: %w", i, err)
		}
	}
	return nil
}

func encodeMsgpackString(buf *bytes.Buffer, value string) error {
	length := len(value)
	switch {
	case length <= 31:
		buf.WriteByte(0xa0 | byte(length))
	case length <= math.MaxUint8:
		buf.WriteByte(0xd9)
		buf.WriteByte(byte(length))
	case length <= math.MaxUint16:
		buf.WriteByte(0xda)
		writeUint16(buf, uint16(length))
	default:
		buf.WriteByte(0xdb)
		writeUint32(buf, uint32(length))
	}
	buf.WriteString(value)
	return nil
}

func encodeMsgpackInt(buf *bytes.Buffer, value int64) error {
	if value >= 0 {
		return encodeMsgpackUint(buf, uint64(value))
	}
	switch {
	case value >= -32:
		buf.WriteByte(byte(int8(value)))
	case value >= math.MinInt8:
		buf.WriteByte(0xd0)
		buf.WriteByte(byte(int8(value)))
	case value >= math.MinInt16:
		buf.WriteByte(0xd1)
		writeUint16(buf, uint16(int16(value)))
	case value >= math.MinInt32:
		buf.WriteByte(0xd2)
		writeUint32(buf, uint32(int32(value)))
	default:
		buf.WriteByte(0xd3)
		writeUint64(buf, uint64(value))
	}
	return nil
}

func encodeMsgpackUint(buf *bytes.Buffer, value uint64) error {
	switch {
	case value <= 0x7f:
		buf.WriteByte(byte(value))
	case value <= math.MaxUint8:
		buf.WriteByte(0xcc)
		buf.WriteByte(byte(value))
	case value <= math.MaxUint16:
		buf.WriteByte(0xcd)
		writeUint16(buf, uint16(value))
	case value <= math.MaxUint32:
		buf.WriteByte(0xce)
		writeUint32(buf, uint32(value))
	default:
		buf.WriteByte(0xcf)
		writeUint64(buf, value)
	}
	return nil
}

func encodeMsgpackMapLen(buf *bytes.Buffer, length int) error {
	if length < 0 {
		return fmt.Errorf("msgpack map length is negative")
	}
	switch {
	case length <= 15:
		buf.WriteByte(0x80 | byte(length))
	case length <= math.MaxUint16:
		buf.WriteByte(0xde)
		writeUint16(buf, uint16(length))
	default:
		buf.WriteByte(0xdf)
		writeUint32(buf, uint32(length))
	}
	return nil
}

func encodeMsgpackArrayLen(buf *bytes.Buffer, length int) error {
	if length < 0 {
		return fmt.Errorf("msgpack array length is negative")
	}
	switch {
	case length <= 15:
		buf.WriteByte(0x90 | byte(length))
	case length <= math.MaxUint16:
		buf.WriteByte(0xdc)
		writeUint16(buf, uint16(length))
	default:
		buf.WriteByte(0xdd)
		writeUint32(buf, uint32(length))
	}
	return nil
}

func writeUint16(buf *bytes.Buffer, value uint16) {
	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], value)
	buf.Write(tmp[:])
}
func writeUint32(buf *bytes.Buffer, value uint32) {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], value)
	buf.Write(tmp[:])
}
func writeUint64(buf *bytes.Buffer, value uint64) {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], value)
	buf.Write(tmp[:])
}
