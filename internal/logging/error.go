package logging

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	unavailableErrorText = "error details unavailable"
	maxSafeErrorBytes    = 4096
)

var (
	errorURLUserinfoPattern    = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^/@\s]+)@`)
	errorMySQLDSNPattern       = regexp.MustCompile(`(?i)\b[^:\s/@]+:[^\r\n]*@((?:tcp|unix)\()`)
	errorJSONSecretPattern     = regexp.MustCompile(`(?i)("(?:[a-z0-9]+[_-])*(?:api[_-]?key|authorization|cookie|credential|encrypted[_-]?private[_-]?key|mnemonic|passphrase|password|passwd|proxy[_-]?authorization|pwd|private[_-]?key|secret|set[_-]?cookie|signing[_-]?key|token)(?:[_-][a-z0-9]+)*"\s*:\s*)("(?:\\.|[^"\\])*"|'[^'\r\n]*'|[^,;&#}\]\r\n]+)`)
	errorSecretPattern         = regexp.MustCompile(`(?i)(\b(?:[a-z0-9]+[_-])*(?:api[_-]?key|credential|encrypted[_-]?private[_-]?key|mnemonic|passphrase|password|passwd|pwd|private[_-]?key|secret|signing[_-]?key|token)(?:[_-][a-z0-9]+)*\b\s*(?:=|:)\s*)("(?:\\.|[^"\\])*"|'[^'\r\n]*'|[^,;&#}\]\r\n]+)`)
	errorAuthorizationPattern  = regexp.MustCompile(`(?i)(\b(?:proxy[-_ ]?)?authorization\s*(?:=|:)\s*)[^\r\n,;]+`)
	errorCookiePattern         = regexp.MustCompile(`(?i)(\b(?:set[-_ ]?)?cookie\s*(?:=|:)\s*)[^\r\n]+`)
	errorBearerPattern         = regexp.MustCompile(`(?i)(\b(?:authorization\s*(?:=|:)\s*)?bearer\s+)[a-z0-9._~+/=-]+`)
	diagnosticJSONFieldPattern = regexp.MustCompile(`("((?:\\.|[^"\\])*)"\s*:\s*)("(?:\\.|[^"\\])*"|'[^'\r\n]*'|[^,;&#}\]\r\n]+)`)
	diagnosticKeyValuePattern  = regexp.MustCompile(`(?i)(\b([a-z][a-z0-9_.-]*)\b\s*(?:=|:)\s*)("(?:\\.|[^"\\])*"|'[^'\r\n]*'|[^,;&#}\]\r\n]+)`)
	diagnosticSensitiveMatcher = newSensitiveMatcher(append(
		append([]string(nil), defaultSensitiveKeys...),
		"authorization", "proxy_authorization", "cookie", "set_cookie", "credential",
		"token", "signature", "signed_request",
	))
)

// SafeErrorText returns a diagnosable error string with common credentials
// removed. Error values often come from external clients and can embed DSNs,
// HTTP URLs, or key/value configuration fragments, so they must not be logged
// verbatim even when the surrounding slog attribute is named "error".
func SafeErrorText(err error) (text string) {
	if err == nil {
		return ""
	}
	// error implementations are external code. Keep logging best-effort even
	// if a broken Error method panics, and never render the recovered value
	// because that value may itself contain credentials.
	text = unavailableErrorText
	defer func() {
		if recover() != nil {
			text = unavailableErrorText
		}
	}()
	return SafeDiagnosticExcerpt(err.Error(), maxSafeErrorBytes)
}

// SafeDiagnosticText redacts credentials from dependency response text that is
// useful for diagnosis but is not represented as an error value.
func SafeDiagnosticText(text string) string {
	if text == "" {
		return ""
	}
	if sanitized, ok := sanitizeDiagnosticJSON(text); ok {
		text = sanitized
	}
	return sanitizeUnstructuredDiagnostic(text)
}

func sanitizeUnstructuredDiagnostic(text string) string {
	text = errorURLUserinfoPattern.ReplaceAllString(text, `${1}`+redactedValue+`@`)
	text = errorMySQLDSNPattern.ReplaceAllString(text, redactedValue+`@${1}`)
	text = redactMatchedDiagnosticFields(text, diagnosticJSONFieldPattern, true)
	text = redactMatchedDiagnosticFields(text, diagnosticKeyValuePattern, false)
	text = errorJSONSecretPattern.ReplaceAllString(text, `${1}"`+redactedValue+`"`)
	text = errorSecretPattern.ReplaceAllString(text, `${1}`+redactedValue)
	text = errorBearerPattern.ReplaceAllString(text, `${1}`+redactedValue)
	text = errorAuthorizationPattern.ReplaceAllString(text, `${1}`+redactedValue)
	text = errorCookiePattern.ReplaceAllString(text, `${1}`+redactedValue)
	return strings.TrimSpace(text)
}

func redactMatchedDiagnosticFields(text string, pattern *regexp.Regexp, quotedKey bool) string {
	matches := pattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	var output strings.Builder
	output.Grow(len(text))
	last := 0
	for _, match := range matches {
		if len(match) < 8 {
			continue
		}
		key := text[match[4]:match[5]]
		if quotedKey {
			if decoded, err := strconv.Unquote(`"` + key + `"`); err == nil {
				key = decoded
			}
		}
		if !diagnosticSensitiveMatcher.matches(key) {
			continue
		}
		valueStart, valueEnd := match[6], match[7]
		output.WriteString(text[last:valueStart])
		output.WriteString(`"` + redactedValue + `"`)
		last = valueEnd
	}
	if last == 0 {
		return text
	}
	output.WriteString(text[last:])
	return output.String()
}

func sanitizeDiagnosticJSON(text string) (string, bool) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", false
	}
	redactDiagnosticJSONValue(value)
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", false
	}
	return strings.TrimSpace(output.String()), true
}

func redactDiagnosticJSONValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if diagnosticSensitiveMatcher.matches(key) {
				typed[key] = redactedValue
				continue
			}
			if text, ok := child.(string); ok {
				typed[key] = sanitizeUnstructuredDiagnostic(text)
				continue
			}
			redactDiagnosticJSONValue(child)
		}
	case []any:
		for index, child := range typed {
			if text, ok := child.(string); ok {
				typed[index] = sanitizeUnstructuredDiagnostic(text)
				continue
			}
			redactDiagnosticJSONValue(child)
		}
	}
}

// SafeDiagnosticExcerpt applies credential redaction, flattens control
// characters, and bounds untrusted dependency text before it reaches a log.
func SafeDiagnosticExcerpt(text string, maxBytes int) string {
	text = SafeDiagnosticText(text)
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	const marker = " ...[truncated]"
	limit := maxBytes - len(marker)
	if limit <= 0 {
		return marker[:min(len(marker), maxBytes)]
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return strings.TrimSpace(text) + marker
}

func isErrorAttrKey(key string) bool {
	return slicesContain(splitTokens(key), "error")
}

func slicesContain(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
