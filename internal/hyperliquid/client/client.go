package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hyperliquid-builder-code-bot/internal/hyperliquid"
)

const (
	MainnetBaseURL = "https://api.hyperliquid.xyz"
	TestnetBaseURL = "https://api.hyperliquid-testnet.xyz"

	DefaultTimeout          = 10 * time.Second
	DefaultMaxResponseBytes = 2 << 20

	endpointExchange = "/exchange"
	endpointInfo     = "/info"
)

type Config struct {
	Network          hyperliquid.Network
	BaseURL          string
	HTTPClient       *http.Client
	Timeout          time.Duration
	MaxResponseBytes int64
	UserAgent        string
}

type Client struct {
	baseURL          *url.URL
	httpClient       *http.Client
	maxResponseBytes int64
	userAgent        string
}

type Response struct {
	Endpoint   string
	StatusCode int
	Header     http.Header
	Body       []byte
}

type StatusError struct {
	Endpoint   string
	StatusCode int
	Status     string
	Body       []byte
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	if e == nil {
		return ""
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("hyperliquid %s returned %s, retry_after=%s", e.Endpoint, e.Status, e.RetryAfter)
	}
	return fmt.Sprintf("hyperliquid %s returned %s", e.Endpoint, e.Status)
}

func (e *StatusError) RateLimited() bool {
	return e != nil && e.StatusCode == http.StatusTooManyRequests
}

func New(cfg Config) (*Client, error) {
	network := hyperliquid.NormalizeNetwork(cfg.Network)
	defaultURL, err := BaseURLForNetwork(network)
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
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.Timeout}
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = DefaultMaxResponseBytes
	}
	return &Client{
		baseURL:          baseURL,
		httpClient:       httpClient,
		maxResponseBytes: cfg.MaxResponseBytes,
		userAgent:        strings.TrimSpace(cfg.UserAgent),
	}, nil
}

func BaseURLForNetwork(network hyperliquid.Network) (string, error) {
	switch hyperliquid.NormalizeNetwork(network) {
	case hyperliquid.NetworkMainnet:
		return MainnetBaseURL, nil
	case hyperliquid.NetworkTestnet:
		return TestnetBaseURL, nil
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

func (c *Client) post(ctx context.Context, endpoint string, request any, raw json.RawMessage, out any) (Response, error) {
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
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	httpResponse, err := c.httpClient.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("post hyperliquid %s: %w", endpoint, err)
	}
	defer httpResponse.Body.Close()
	body, err := readResponseBody(httpResponse.Body, c.maxResponseBytes)
	if err != nil {
		return Response{}, fmt.Errorf("read hyperliquid %s response: %w", endpoint, err)
	}
	response := Response{Endpoint: endpoint, StatusCode: httpResponse.StatusCode, Header: httpResponse.Header.Clone(), Body: body}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return response, &StatusError{Endpoint: endpoint, StatusCode: httpResponse.StatusCode, Status: httpResponse.Status, Body: body, RetryAfter: parseRetryAfter(httpResponse.Header.Get("Retry-After"))}
	}
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

func readResponseBody(body io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxResponseBytes
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("body exceeds %d bytes", maxBytes)
	}
	return data, nil
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	when, err := http.ParseTime(value)
	if err == nil {
		delay := time.Until(when)
		if delay > 0 {
			return delay
		}
	}
	return 0
}
