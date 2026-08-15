package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"builder-code-bot/internal/hyperliquid"
)

func TestInfoPostsJSONAndDecodesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"type":"spotMeta"}` {
			t.Fatalf("body = %s", body)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	c, err := New(Config{Network: hyperliquid.NetworkMainnet, BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		OK bool `json:"ok"`
	}
	if _, err := c.Info(context.Background(), map[string]string{"type": "spotMeta"}, &out); err != nil || !out.OK {
		t.Fatalf("Info() = %#v, %v", out, err)
	}
}

func TestExchangeRawPreservesExactBody(t *testing.T) {
	want := json.RawMessage("{ \n  \"nonce\": 42, \"action\": {\"type\":\"claimRewards\"}\n}")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/exchange" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		got, _ := io.ReadAll(r.Body)
		if string(got) != string(want) {
			t.Fatalf("body = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	c, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExchangeRaw(context.Background(), want, nil); err != nil {
		t.Fatal(err)
	}
}

func TestResponseBodyCloseErrorIsReturned(t *testing.T) {
	closeErr := errors.New("close response body: connection reset")
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantStatus bool
	}{
		{name: "successful response", statusCode: http.StatusOK, body: `{"ok":true}`},
		{name: "HTTP error response", statusCode: http.StatusTooManyRequests, body: `{"response":"rate limited"}`, wantStatus: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(Config{})
			if err != nil {
				t.Fatal(err)
			}
			c.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tt.statusCode,
					Body:       closeErrorBody{Reader: strings.NewReader(tt.body), err: closeErr},
					Header:     make(http.Header),
				}, nil
			})}

			_, err = c.Info(context.Background(), map[string]string{"type": "spotMeta"}, nil)
			if !errors.Is(err, closeErr) {
				t.Fatalf("Info() error = %v, want close error", err)
			}
			var statusErr *HTTPStatusError
			if errors.As(err, &statusErr) != tt.wantStatus {
				t.Fatalf("Info() status error = %v, want %v (error: %v)", statusErr, tt.wantStatus, err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type closeErrorBody struct {
	io.Reader
	err error
}

func (b closeErrorBody) Close() error { return b.err }

func TestNewUsesNetworkEndpointAndRejectsInvalidInput(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("New() returned nil client")
	}
	if _, err := New(Config{Network: "devnet"}); err == nil {
		t.Fatal("expected unsupported network error")
	}
	if _, err := New(Config{BaseURL: "ftp://example.com"}); err == nil {
		t.Fatal("expected invalid URL error")
	}
}

func TestNonSuccessfulResponsesIncludeSafeServerReason(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		call       func(*Client) (Response, error)
		endpoint   string
		reason     string
	}{
		{
			name: "info 422 JSON response", statusCode: http.StatusUnprocessableEntity,
			body: `{"status":"err","response":"invalid referral request"}`,
			call: func(c *Client) (Response, error) {
				return c.Info(context.Background(), map[string]string{"type": "referral"}, nil)
			},
			endpoint: endpointInfo, reason: "invalid referral request",
		},
		{
			name: "exchange 429 text response", statusCode: http.StatusTooManyRequests,
			body: "rate limit exceeded; retry later",
			call: func(c *Client) (Response, error) {
				return c.ExchangeRaw(context.Background(), json.RawMessage(`{"action":{}}`), nil)
			},
			endpoint: endpointExchange, reason: "rate limit exceeded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			c, err := New(Config{BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			response, err := tt.call(c)
			if err == nil {
				t.Fatal("expected HTTP status error")
			}
			var statusErr *HTTPStatusError
			if !errors.As(err, &statusErr) {
				t.Fatalf("error type = %T, want *HTTPStatusError", err)
			}
			if statusErr.Endpoint != tt.endpoint || statusErr.StatusCode != tt.statusCode {
				t.Fatalf("HTTPStatusError = %#v", statusErr)
			}
			if !strings.Contains(statusErr.BodyExcerpt, tt.reason) ||
				!strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("error does not include server reason: %v", err)
			}
			if string(response.Body) != statusErr.BodyExcerpt {
				t.Fatalf("response body = %q, excerpt = %q", response.Body, statusErr.BodyExcerpt)
			}
		})
	}
}

func TestHTTPStatusErrorExcerptIsBounded(t *testing.T) {
	body := strings.Repeat("service overloaded ", maxHTTPErrorExcerptBytes)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.Info(context.Background(), map[string]string{"type": "spotMeta"}, nil)
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("Info() error = %v, want *HTTPStatusError", err)
	}
	if len(statusErr.BodyExcerpt) > maxHTTPErrorExcerptBytes {
		t.Fatalf("excerpt length = %d, max = %d", len(statusErr.BodyExcerpt), maxHTTPErrorExcerptBytes)
	}
	if !strings.HasSuffix(statusErr.BodyExcerpt, "...[truncated]") {
		t.Fatalf("excerpt = %q, want truncation marker", statusErr.BodyExcerpt)
	}
	if string(response.Body) != statusErr.BodyExcerpt {
		t.Fatalf("response body and typed error excerpt differ")
	}
}

func TestHTTPStatusErrorExcerptRedactsSensitiveKeyValues(t *testing.T) {
	const body = `{
		"message":"rate limited; password=inline-secret; Authorization: Bearer bearer-secret; https://url-user:url-pass@example.test",
		"password":"password-secret",
		"nested":{"privateKey":"private-key-secret","clientToken":"token-secret","signature":"signature-secret"},
		"authorization":"Basic authorization-secret","cookie":"session=cookie-secret"
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.Info(context.Background(), map[string]string{"type": "referral"}, nil)
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("Info() error = %v, want *HTTPStatusError", err)
	}
	diagnostic := err.Error() + "\n" + string(response.Body)
	for _, leaked := range []string{
		"inline-secret", "password-secret", "private-key-secret", "token-secret",
		"bearer-secret", "url-user:url-pass", "authorization-secret", "cookie-secret", "signature-secret",
	} {
		if strings.Contains(diagnostic, leaked) {
			t.Fatalf("sensitive value %q leaked in diagnostic %q", leaked, diagnostic)
		}
	}
	if !strings.Contains(diagnostic, "rate limited") || !strings.Contains(diagnostic, "[MASKED]") {
		t.Fatalf("diagnostic lost safe reason or mask: %q", diagnostic)
	}
}

func TestSafeHTTPErrorExcerptRedactsMalformedText(t *testing.T) {
	excerpt := safeHTTPErrorExcerpt(
		[]byte(`upstream rejected: password="open-ended secret private_key=also-secret`),
		false,
	)
	if strings.Contains(excerpt, "open-ended") || strings.Contains(excerpt, "also-secret") {
		t.Fatalf("malformed response leaked a sensitive value: %q", excerpt)
	}
	if !strings.Contains(excerpt, "[MASKED]") {
		t.Fatalf("malformed response was not redacted: %q", excerpt)
	}
}
