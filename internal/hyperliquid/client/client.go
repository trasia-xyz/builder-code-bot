package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"builder-code-bot/internal/hyperliquid"
	"builder-code-bot/internal/logging"
	"builder-code-bot/internal/secret"
)

const (
	mainnetBaseURL = "https://api.hyperliquid.xyz"
	testnetBaseURL = "https://api.hyperliquid-testnet.xyz"

	defaultTimeout           = 10 * time.Second
	defaultMaxResponseBytes  = 2 << 20
	maxHTTPErrorExcerptBytes = 1024

	endpointExchange = "/exchange"
	endpointInfo     = "/info"
)

type Config struct {
	Network hyperliquid.Network
	BaseURL string
}

type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
}

type Response struct {
	// Body is the exact response body for successful requests. For non-2xx
	// responses it is the same bounded, redacted excerpt carried by
	// HTTPStatusError.
	Body []byte
}

// HTTPStatusError describes a non-successful Hyperliquid HTTP response. Its
// body excerpt is bounded and redacted so the error is safe to propagate to
// logs and notifications.
type HTTPStatusError struct {
	Endpoint    string
	StatusCode  int
	Status      string
	BodyExcerpt string
}

func (e *HTTPStatusError) Error() string {
	if e == nil {
		return "hyperliquid HTTP status error"
	}
	message := fmt.Sprintf("hyperliquid %s returned %s", e.Endpoint, e.Status)
	if e.BodyExcerpt != "" {
		message += ": " + e.BodyExcerpt
	}
	return message
}

func New(cfg Config) (*Client, error) {
	network := hyperliquid.NormalizeNetwork(cfg.Network)
	defaultURL, err := baseURLForNetwork(network)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(cfg.BaseURL)
	if target == "" {
		target = defaultURL
	}
	baseURL, err := parseBaseURL(target)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL: baseURL, httpClient: &http.Client{Timeout: defaultTimeout},
	}, nil
}

func baseURLForNetwork(network hyperliquid.Network) (string, error) {
	switch hyperliquid.NormalizeNetwork(network) {
	case hyperliquid.NetworkMainnet:
		return mainnetBaseURL, nil
	case hyperliquid.NetworkTestnet:
		return testnetBaseURL, nil
	default:
		return "", fmt.Errorf("unsupported hyperliquid network %q", network)
	}
}

func (c *Client) Info(ctx context.Context, request any, out any) (Response, error) {
	return c.post(ctx, endpointInfo, request, nil, out)
}

func (c *Client) ExchangeRaw(ctx context.Context, request json.RawMessage, out any) (Response, error) {
	if len(request) == 0 {
		return Response{}, fmt.Errorf("hyperliquid %s request is empty", endpointExchange)
	}
	return c.post(ctx, endpointExchange, nil, request, out)
}

func (c *Client) post(ctx context.Context, endpoint string, request any, raw json.RawMessage, out any) (response Response, returnErr error) {
	if ctx == nil {
		return Response{}, fmt.Errorf("context is nil")
	}
	if c == nil || c.httpClient == nil || c.baseURL == nil {
		return Response{}, fmt.Errorf("hyperliquid client is nil")
	}
	payload := []byte(raw)
	if raw == nil {
		if request == nil {
			return Response{}, fmt.Errorf("hyperliquid %s request is nil", endpoint)
		}
		var err error
		payload, err = json.Marshal(request)
		if err != nil {
			return Response{}, fmt.Errorf("encode hyperliquid %s request: %w", endpoint, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL(endpoint), bytes.NewReader(payload))
	if err != nil {
		return Response{}, fmt.Errorf("build hyperliquid %s request: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	httpResponse, err := c.httpClient.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("post hyperliquid %s: %w", endpoint, err)
	}
	defer func() {
		if closeErr := httpResponse.Body.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf(
				"close hyperliquid %s response body (%s): %w",
				endpoint, canonicalHTTPStatus(httpResponse.StatusCode), closeErr,
			))
		}
	}()
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		body, sourceTruncated, err := readBoundedResponseBody(httpResponse.Body)
		if err != nil {
			return Response{}, fmt.Errorf(
				"read hyperliquid %s error response (%s): %w",
				endpoint, canonicalHTTPStatus(httpResponse.StatusCode), err,
			)
		}
		excerpt := safeHTTPErrorExcerpt(body, sourceTruncated)
		response = Response{Body: []byte(excerpt)}
		return response, &HTTPStatusError{
			Endpoint: endpoint, StatusCode: httpResponse.StatusCode,
			Status: canonicalHTTPStatus(httpResponse.StatusCode), BodyExcerpt: excerpt,
		}
	}
	body, err := readResponseBody(httpResponse.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read hyperliquid %s response: %w", endpoint, err)
	}
	response = Response{Body: body}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return response, fmt.Errorf("decode hyperliquid %s response: %w", endpoint, err)
		}
	}
	return response, nil
}

func (c *Client) endpointURL(endpoint string) string {
	u := *c.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + endpoint
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}

func parseBaseURL(value string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("parse hyperliquid base url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("hyperliquid base url requires an http or https host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("hyperliquid base url must not include query or fragment")
	}
	return u, nil
}

func readResponseBody(body io.Reader) ([]byte, error) {
	data, truncated, err := readBoundedResponseBody(body)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, fmt.Errorf("body exceeds %d bytes", defaultMaxResponseBytes)
	}
	return data, nil
}

func readBoundedResponseBody(body io.Reader) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(body, defaultMaxResponseBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) <= defaultMaxResponseBytes {
		return data, false, nil
	}
	return data[:defaultMaxResponseBytes], true, nil
}

func canonicalHTTPStatus(statusCode int) string {
	statusText := http.StatusText(statusCode)
	if statusText == "" {
		return fmt.Sprintf("%d", statusCode)
	}
	return fmt.Sprintf("%d %s", statusCode, statusText)
}

func safeHTTPErrorExcerpt(body []byte, sourceTruncated bool) string {
	if len(body) == 0 {
		return ""
	}
	text := logging.SafeDiagnosticText(redactHTTPErrorBody(body))
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	return truncateHTTPErrorExcerpt(text, sourceTruncated)
}

func redactHTTPErrorBody(body []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err == nil {
		var trailing any
		if err := decoder.Decode(&trailing); err == io.EOF {
			if text, ok := value.(string); ok {
				value = redactTextKeyValues(text)
			} else {
				redactHTTPErrorJSON(value)
			}
			if encoded, err := json.Marshal(value); err == nil {
				return string(encoded)
			}
		}
	}
	return redactTextKeyValues(string(body))
}

func redactHTTPErrorJSON(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isSensitiveResponseKey(key) {
				typed[key] = secret.Masked
				continue
			}
			if text, ok := child.(string); ok {
				typed[key] = redactTextKeyValues(text)
				continue
			}
			redactHTTPErrorJSON(child)
		}
	case []any:
		for index, child := range typed {
			if text, ok := child.(string); ok {
				typed[index] = redactTextKeyValues(text)
				continue
			}
			redactHTTPErrorJSON(child)
		}
	}
}

var sensitiveResponseKeys = []string{
	"apikey",
	"authorization",
	"cookie",
	"credential",
	"encryptedprivatekey",
	"mnemonic",
	"passphrase",
	"password",
	"privatekey",
	"proxyauthorization",
	"secret",
	"setcookie",
	"signingkey",
	"accesstoken",
	"apitoken",
	"authtoken",
	"bearertoken",
	"refreshtoken",
	"sessiontoken",
	"signature",
	"signedrequest",
	"token",
}

func isSensitiveResponseKey(key string) bool {
	var normalized strings.Builder
	for _, r := range key {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			normalized.WriteRune(unicode.ToLower(r))
		}
	}
	candidate := normalized.String()
	for _, sensitive := range sensitiveResponseKeys {
		if strings.Contains(candidate, sensitive) {
			return true
		}
	}
	return false
}

// redactTextKeyValues is a conservative fallback for plain-text and malformed
// JSON error bodies. It recognizes common key=value and key:value shapes and
// masks the value through the next field delimiter.
func redactTextKeyValues(text string) string {
	var output strings.Builder
	last := 0
	for index := 0; index < len(text); index++ {
		if text[index] != '=' && text[index] != ':' {
			continue
		}
		key := responseKeyBefore(text, index)
		if !isSensitiveResponseKey(key) {
			continue
		}
		valueStart := index + 1
		for valueStart < len(text) && unicode.IsSpace(rune(text[valueStart])) {
			valueStart++
		}
		valueEnd := responseValueEnd(text, valueStart)
		output.WriteString(text[last:valueStart])
		if valueStart < len(text) && (text[valueStart] == '"' || text[valueStart] == '\'') {
			quote := text[valueStart]
			output.WriteByte(quote)
			output.WriteString(secret.Masked)
			if valueEnd > valueStart && valueEnd <= len(text) && text[valueEnd-1] == quote {
				output.WriteByte(quote)
			}
		} else {
			output.WriteString(secret.Masked)
		}
		last = valueEnd
		index = valueEnd - 1
	}
	if last == 0 {
		return text
	}
	output.WriteString(text[last:])
	return output.String()
}

func responseKeyBefore(text string, delimiter int) string {
	end := delimiter
	for end > 0 && unicode.IsSpace(rune(text[end-1])) {
		end--
	}
	if end == 0 {
		return ""
	}
	if text[end-1] == '"' || text[end-1] == '\'' {
		quote := text[end-1]
		for start := end - 2; start >= 0; start-- {
			if text[start] == quote && (start == 0 || text[start-1] != '\\') {
				return text[start+1 : end-1]
			}
		}
		return ""
	}
	start := end
	for start > 0 {
		r := rune(text[start-1])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' {
			break
		}
		start--
	}
	return text[start:end]
}

func responseValueEnd(text string, start int) int {
	if start >= len(text) {
		return start
	}
	if text[start] == '"' || text[start] == '\'' {
		quote := text[start]
		escaped := false
		for index := start + 1; index < len(text); index++ {
			if escaped {
				escaped = false
				continue
			}
			if text[index] == '\\' {
				escaped = true
				continue
			}
			if text[index] == quote {
				return index + 1
			}
		}
		return len(text)
	}
	for index := start; index < len(text); index++ {
		switch text[index] {
		case ',', ';', '&', '\n', '\r', '}', ']':
			return index
		}
	}
	return len(text)
}

func truncateHTTPErrorExcerpt(text string, sourceTruncated bool) string {
	const marker = " ...[truncated]"
	if !sourceTruncated && len(text) <= maxHTTPErrorExcerptBytes {
		return text
	}
	limit := maxHTTPErrorExcerptBytes - len(marker)
	if limit <= 0 {
		return marker
	}
	if len(text) > limit {
		text = text[:limit]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return strings.TrimSpace(text) + marker
}
