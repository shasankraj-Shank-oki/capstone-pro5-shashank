package orders

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"oms/internal/auth"
	"oms/internal/config"
)

func TestRESTEndpointsEnforceAuthRolesAndOrderOperations(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	cfg := config.Config{JWTSecret: secret}
	handler := NewHandler(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), NewService(&fakeRepository{}, fakePrices{price: 12.5}), nil)
	customerToken := tokenFor(t, secret, auth.RoleCustomer)
	adminToken := tokenFor(t, secret, auth.RoleAdmin)

	response := request(handler, http.MethodPost, "/api/v1/orders", "", `{"customerId":101,"productId":501,"quantity":2}`)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated create: got %d, want %d", response.Code, http.StatusUnauthorized)
	}
	response = requestWithToken(handler, http.MethodPost, "/api/v1/orders", customerToken, `{"customerId":101,"productId":501,"quantity":2}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create order: got %d, want %d: %s", response.Code, http.StatusCreated, response.Body)
	}
	var created Order
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != 1 || created.Status != StatusConfirmed || created.TotalAmount != 25 {
		t.Fatalf("unexpected created order: %#v", created)
	}

	response = requestWithToken(handler, http.MethodGet, "/api/v1/orders/1", customerToken, "")
	if response.Code != http.StatusOK {
		t.Fatalf("get order: got %d: %s", response.Code, response.Body)
	}
	response = requestWithToken(handler, http.MethodGet, "/api/v1/orders?limit=10&offset=0", customerToken, "")
	if response.Code != http.StatusOK {
		t.Fatalf("list orders: got %d: %s", response.Code, response.Body)
	}
	response = requestWithToken(handler, http.MethodPut, "/api/v1/orders/1/status", customerToken, `{"status":"CONFIRMED"}`)
	if response.Code != http.StatusForbidden {
		t.Fatalf("customer status update: got %d, want %d", response.Code, http.StatusForbidden)
	}
	response = requestWithToken(handler, http.MethodPut, "/api/v1/orders/1/status", adminToken, `{"status":"CONFIRMED"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("admin status update: got %d: %s", response.Code, response.Body)
	}
}

func TestGraphQLCreateAndStatusAuthorization(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	cfg := config.Config{JWTSecret: secret}
	handler := NewHandler(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), NewService(&fakeRepository{}, fakePrices{price: 12.5}), nil)
	customerToken := tokenFor(t, secret, auth.RoleCustomer)
	adminToken := tokenFor(t, secret, auth.RoleAdmin)
	body := `{"query":"mutation($input: CreateOrderInput!){createOrder(input:$input){id customerId productId status totalAmount}}","variables":{"input":{"customerId":101,"productId":501,"quantity":2}}}`
	response := requestWithToken(handler, http.MethodPost, "/graphql", customerToken, body)
	if response.Code != http.StatusOK {
		t.Fatalf("GraphQL create: got %d: %s", response.Code, response.Body)
	}
	var result struct {
		Data struct {
			CreateOrder struct {
				ID          string  `json:"id"`
				CustomerID  string  `json:"customerId"`
				ProductID   string  `json:"productId"`
				Status      string  `json:"status"`
				TotalAmount float64 `json:"totalAmount"`
			} `json:"createOrder"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) > 0 || result.Data.CreateOrder.Status != string(StatusConfirmed) || result.Data.CreateOrder.TotalAmount != 25 || result.Data.CreateOrder.CustomerID != "101" || result.Data.CreateOrder.ProductID != "501" {
		t.Fatalf("GraphQL create returned unexpected result: %s", response.Body)
	}
	for _, query := range []string{
		`{"query":"{ order(id: \"1\") { id customerId productId quantity unitPrice totalAmount status } }"}`,
		`{"query":"{ orders(limit: 10, offset: 0) { id customerId productId quantity unitPrice totalAmount status } }"}`,
	} {
		response = requestWithToken(handler, http.MethodPost, "/graphql", customerToken, query)
		if response.Code != http.StatusOK || bytes.Contains(response.Body.Bytes(), []byte(`"errors"`)) {
			t.Fatalf("GraphQL read operation failed: %d %s", response.Code, response.Body)
		}
	}
	update := `{"query":"mutation { updateOrderStatus(id:\"1\", status:CONFIRMED) { id status } }"}`
	response = requestWithToken(handler, http.MethodPost, "/graphql", customerToken, update)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("administrator role required")) {
		t.Fatalf("customer GraphQL status update should be forbidden: %d %s", response.Code, response.Body)
	}
	response = requestWithToken(handler, http.MethodPost, "/graphql", adminToken, update)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"status":"CONFIRMED"`)) {
		t.Fatalf("admin GraphQL status update failed: %d %s", response.Code, response.Body)
	}
}

func tokenFor(t *testing.T, secret []byte, role string) string {
	t.Helper()
	token, err := auth.IssueToken(secret, "test-user", role, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func request(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	return requestWithToken(handler, method, path, token, body)
}

func requestWithToken(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
