package config

import (
	"reflect"

	"github.com/go-viper/mapstructure/v2"

	"hyperliquid-builder-code-bot/internal/secret"
)

var secretStringType = reflect.TypeFor[secret.SecretString]()

func decodeHook() mapstructure.DecodeHookFuncType {
	return func(from, to reflect.Type, data any) (any, error) {
		if from.Kind() == reflect.String && to == secretStringType {
			return secret.NewString(data.(string)), nil
		}
		return data, nil
	}
}
