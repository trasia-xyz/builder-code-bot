package signing

import "hyperliquid-builder-code-bot/internal/hyperliquid/wire"

type Field = wire.Field
type Object = wire.Object

func F(key string, value any) Field {
	return wire.F(key, value)
}
