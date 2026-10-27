package orders

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"oms/internal/auth"
	"oms/internal/config"
)

type failingRepository struct{ fakeRepository }

func (failingRepository) Create(context.Context, *Order) error {
	return errors.New("connection refused: password=secret-should-not-leak")
}

func (failingRepository) FindByID(context.Context, int64) (*Order, error) {
	return nil, errors.New("connection refused")
}

func TestDatabaseFailureReturnsControlledErrorAndCountsMetric(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	handler := NewHandler(config.Config{JWTSecret: secret}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewService(&failingRepository{}, fakePrices{price: 12.5}), nil)
	token := tokenFor(t, secret, auth.RoleCustomer)

	response := requestWithToken(handler, http.MethodPost, "/api/v1/orders", token, `{"customerId":101,"productId":501,"quantity":2}`)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("create with failing database: got %d, want 500", response.Code)
	}
	if strings.Contains(response.Body.String(), "secret-should-not-leak") || strings.Contains(response.Body.String(), "connection refused") {
		t.Fatalf("internal error details leaked: %s", response.Body)
	}
	response = requestWithToken(handler, http.MethodGet, "/api/v1/orders/1", token, "")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("get with failing database: got %d, want 500", response.Code)
	}
	metrics := requestWithToken(handler, http.MethodGet, "/metrics", "", "")
	if !strings.Contains(metrics.Body.String(), "oms_order_service_database_errors_total 2") {
		t.Fatalf("database error counter not incremented:\n%s", metrics.Body)
	}
}

func TestReadinessReportsUnavailableWithoutDatabase(t *testing.T) {
	handler := NewHandler(config.Config{JWTSecret: []byte("0123456789abcdef0123456789abcdef")},
		slog.New(slog.NewTextHandler(io.Discard, nil)), NewService(&fakeRepository{}, fakePrices{price: 1}), nil)
	if code := requestWithToken(handler, http.MethodGet, "/health/live", "", "").Code; code != http.StatusOK {
		t.Fatalf("live: got %d, want 200", code)
	}
	if code := requestWithToken(handler, http.MethodGet, "/health/ready", "", "").Code; code != http.StatusServiceUnavailable {
		t.Fatalf("ready without database: got %d, want 503", code)
	}
}
