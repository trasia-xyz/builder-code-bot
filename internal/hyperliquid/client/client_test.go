package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"hyperliquid-builder-code-bot/internal/hyperliquid"
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
	if c.Network() != hyperliquid.NetworkMainnet {
		t.Fatalf("network = %q", c.Network())
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

func TestNewUsesNetworkEndpointAndRejectsInvalidInput(t *testing.T) {
	c, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Network() != hyperliquid.NetworkTestnet || c.BaseURL() != TestnetBaseURL {
		t.Fatalf("client = network %q, base URL %q", c.Network(), c.BaseURL())
	}
	if _, err := New(Config{Network: "devnet"}); err == nil {
		t.Fatal("expected unsupported network error")
	}
	if _, err := New(Config{BaseURL: "ftp://example.com"}); err == nil {
		t.Fatal("expected invalid URL error")
	}
}
