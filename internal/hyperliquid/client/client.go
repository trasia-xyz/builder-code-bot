package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"builder-code-bot/internal/hyperliquid"
)

const (
	mainnetBaseURL = "https://api.hyperliquid.xyz"
	testnetBaseURL = "https://api.hyperliquid-testnet.xyz"

	defaultTimeout          = 10 * time.Second
	defaultMaxResponseBytes = 2 << 20

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
	Body []byte
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
	httpResponse, err := c.httpClient.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("post hyperliquid %s: %w", endpoint, err)
	}
	defer httpResponse.Body.Close()
	body, err := readResponseBody(httpResponse.Body)
	if err != nil {
		return Response{}, fmt.Errorf("read hyperliquid %s response: %w", endpoint, err)
	}
	response := Response{Body: body}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return response, fmt.Errorf("hyperliquid %s returned %s", endpoint, httpResponse.Status)
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

func readResponseBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, defaultMaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > defaultMaxResponseBytes {
		return nil, fmt.Errorf("body exceeds %d bytes", defaultMaxResponseBytes)
	}
	return data, nil
}
