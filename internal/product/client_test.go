package product

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"oms/internal/auth"
	"oms/internal/config"
)

func TestCircuitBreakerOpensAndRecovers(t *testing.T) {
	now := time.Now()
	breaker := &circuitBreaker{threshold: 2, recovery: time.Second}
	if !breaker.allow(now) {
		t.Fatal("new breaker should allow requests")
	}
	breaker.record(false, now)
	breaker.record(false, now)
	if breaker.allow(now) {
		t.Fatal("breaker should be open after threshold")
	}
	if !breaker.allow(now.Add(2 * time.Second)) {
		t.Fatal("breaker should allow a half-open probe after recovery")
	}
	if breaker.allow(now.Add(2 * time.Second)) {
		t.Fatal("breaker should allow only one half-open probe")
	}
	breaker.record(true, now.Add(2*time.Second))
	if !breaker.allow(now.Add(2 * time.Second)) {
		t.Fatal("breaker should close after a successful probe")
	}
}

func TestClientRetriesAreBoundedAndOpenBreaker(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, err := NewClient(config.Config{
		ProductServiceURL:   server.URL,
		JWTSecret:           []byte("0123456789abcdef0123456789abcdef"),
		ProductTimeout:      100 * time.Millisecond,
		ProductMaxRetries:   2,
		ProductRetryBase:    time.Millisecond,
		ProductBreakerLimit: 1,
		ProductBreakerWait:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Price(context.Background(), 501); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected controlled dependency error, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("request attempts = %d, want initial request plus two retries", got)
	}
	if _, err := client.Price(context.Background(), 501); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected open breaker to reject request, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("open breaker made another request: total calls = %d", got)
	}
}

func TestClientDoesNotRetryNonTransientResponse(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	client, err := NewClient(config.Config{
		ProductServiceURL:   server.URL,
		JWTSecret:           []byte("0123456789abcdef0123456789abcdef"),
		ProductTimeout:      100 * time.Millisecond,
		ProductMaxRetries:   3,
		ProductRetryBase:    time.Millisecond,
		ProductBreakerLimit: 1,
		ProductBreakerWait:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Price(context.Background(), 501); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected controlled dependency error, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("non-transient response retried %d times, want 1", got)
	}
	if _, err := client.Price(context.Background(), 501); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unexpected second call error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("non-transient response incorrectly opened breaker: total calls = %d", got)
	}
}

func TestClientHonorsPerAttemptTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"productId":501,"price":12.5}`))
		}
	}))
	defer server.Close()
	client, err := NewClient(config.Config{
		ProductServiceURL:   server.URL,
		JWTSecret:           []byte("0123456789abcdef0123456789abcdef"),
		ProductTimeout:      25 * time.Millisecond,
		ProductMaxRetries:   0,
		ProductRetryBase:    time.Millisecond,
		ProductBreakerLimit: 2,
		ProductBreakerWait:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := client.Price(context.Background(), 501); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected controlled timeout error, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("request exceeded timeout budget: %s", elapsed)
	}
}

func TestCancelledProbeDoesNotStickBreakerHalfOpen(t *testing.T) {
	now := time.Now()
	breaker := &circuitBreaker{threshold: 1, recovery: time.Second}
	breaker.record(false, now)
	if !breaker.allow(now.Add(2 * time.Second)) {
		t.Fatal("expected half-open probe")
	}
	breaker.release()
	if !breaker.allow(now.Add(2 * time.Second)) {
		t.Fatal("breaker stuck after cancelled probe")
	}
}

func TestClientUsesAdminTokenForStockOperationsAndMapsOutOfStock(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := auth.ParseToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), secret)
		if err != nil || principal.Role != auth.RoleAdmin {
			t.Errorf("invalid internal service token: principal=%#v err=%v", principal, err)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/internal/orders/42/reservation":
			var input struct {
				ProductID int64 `json:"productId"`
				Quantity  int   `json:"quantity"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input.ProductID != 7 || input.Quantity != 2 {
				t.Errorf("unexpected reservation request: %#v", input)
			}
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"insufficient_stock"}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/internal/orders/42/reservation":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"RELEASED"}`))
		default:
			t.Errorf("unexpected stock request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := NewClient(config.Config{
		ProductServiceURL: server.URL,
		JWTSecret:         secret,
		ProductTimeout:    time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Reserve(context.Background(), 42, 7, 2); !errors.Is(err, ErrOutOfStock) {
		t.Fatalf("reserve error = %v, want ErrOutOfStock", err)
	}
	if err := client.Release(context.Background(), 42); err != nil {
		t.Fatalf("release: %v", err)
	}
}
