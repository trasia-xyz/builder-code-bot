package wire

// Field is one key/value entry in an ordered Hyperliquid wire object.
type Field struct {
	Key   string
	Value any
}

// Object is a map-like value with caller-controlled field order.
//
// Hyperliquid action hashes are sensitive to object key order. Use Object for
// schema-level action objects. Plain Go maps are only suitable for data maps
// whose order is intentionally canonicalized before signing.
type Object []Field

// F creates one Object field.
func F(key string, value any) Field {
	return Field{Key: key, Value: value}
}
