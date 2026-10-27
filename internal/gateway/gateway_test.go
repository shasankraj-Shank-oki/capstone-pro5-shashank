package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"oms/internal/auth"
	"oms/internal/config"
	"oms/internal/orders"
	"oms/internal/product"
)

func TestGatewayAuthenticatesAndForwardsOrderRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/orders/42" || r.URL.Query().Get("view") != "full" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("X-Request-ID") != "request-42" {
			t.Errorf("request ID was not forwarded: %q", r.Header.Get("X-Request-ID"))
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("authorization was not forwarded")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":42}`))
	}))
	defer upstream.Close()

	secret := []byte("0123456789abcdef0123456789abcdef")
	handler := New(config.Config{
		OrderServiceURL: upstream.URL,
		JWTSecret:       secret,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	token, err := auth.IssueToken(secret, "test-user", auth.RoleCustomer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/orders/42", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request status = %d", unauthorized.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/orders/42?view=full", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Request-ID", "request-42")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != `{"id":42}` {
		t.Fatalf("unexpected gateway response: %d %s", response.Code, response.Body)
	}
	customerStatusUpdate := httptest.NewRequest(http.MethodPut, "/api/v1/orders/42/status", nil)
	customerStatusUpdate.Header.Set("Authorization", "Bearer "+token)
	forbidden := httptest.NewRecorder()
	handler.ServeHTTP(forbidden, customerStatusUpdate)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("customer status update status = %d, want %d", forbidden.Code, http.StatusForbidden)
	}
}

func TestGatewayProtectsAndForwardsProductRoutes(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		expectedPath := "/api/v1/products"
		if r.Method == http.MethodDelete {
			expectedPath = "/api/v1/products/1"
		}
		if r.URL.Path != expectedPath || r.Header.Get("Authorization") == "" {
			t.Errorf("unexpected product request: %s %s", r.Method, r.URL.Path)
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer upstream.Close()
	secret := []byte("0123456789abcdef0123456789abcdef")
	handler := New(config.Config{
		OrderServiceURL:   "http://127.0.0.1:1",
		ProductServiceURL: upstream.URL,
		JWTSecret:         secret,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	customerToken, err := auth.IssueToken(secret, "customer", auth.RoleCustomer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	adminToken, err := auth.IssueToken(secret, "admin", auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	forbidden := sendGatewayRequest(handler, http.MethodPost, "/api/v1/products", customerToken, `{"name":"Keyboard"}`)
	if forbidden.Code != http.StatusForbidden || calls != 0 {
		t.Fatalf("customer product create: status=%d upstream calls=%d", forbidden.Code, calls)
	}
	created := sendGatewayRequest(handler, http.MethodPost, "/api/v1/products", adminToken, `{"name":"Keyboard"}`)
	if created.Code != http.StatusCreated || calls != 1 {
		t.Fatalf("admin product create: status=%d upstream calls=%d body=%s", created.Code, calls, created.Body)
	}
	customerDelete := sendGatewayRequest(handler, http.MethodDelete, "/api/v1/products/1", customerToken, "")
	if customerDelete.Code != http.StatusForbidden || calls != 1 {
		t.Fatalf("customer product delete: status=%d upstream calls=%d", customerDelete.Code, calls)
	}
	adminDelete := sendGatewayRequest(handler, http.MethodDelete, "/api/v1/products/1", adminToken, "")
	if adminDelete.Code != http.StatusNoContent || calls != 2 {
		t.Fatalf("admin product delete: status=%d upstream calls=%d body=%s", adminDelete.Code, calls, adminDelete.Body)
	}
}

func TestGatewayGeneratesAndPropagatesMissingRequestID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			t.Error("gateway did not generate a request ID")
		}
		w.Header().Set("X-Upstream-Request-ID", id)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	secret := []byte("0123456789abcdef0123456789abcdef")
	token, err := auth.IssueToken(secret, "test-user", auth.RoleCustomer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.Config{
		OrderServiceURL: upstream.URL,
		JWTSecret:       secret,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/orders/42", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("unexpected response status: %d", response.Code)
	}
	if id := response.Header().Get("X-Request-ID"); id == "" || response.Header().Get("X-Upstream-Request-ID") != id {
		t.Fatalf("request ID was not propagated end to end: response ID=%q upstream ID=%q", id, response.Header().Get("X-Upstream-Request-ID"))
	}
}

func TestGatewayToOrderServiceEndToEnd(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serviceHandler := orders.NewHandler(
		config.Config{JWTSecret: secret},
		logger,
		orders.NewService(&memoryOrders{}, fixedPrice(12.5)),
		nil,
	)
	service := httptest.NewServer(serviceHandler)
	defer service.Close()
	gatewayHandler := New(config.Config{
		OrderServiceURL: service.URL,
		JWTSecret:       secret,
	}, logger)

	customerToken, err := auth.IssueToken(secret, "customer-101", auth.RoleCustomer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	adminToken, err := auth.IssueToken(secret, "admin-1", auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	created := sendGatewayRequest(gatewayHandler, http.MethodPost, "/api/v1/orders", customerToken, `{"customerId":101,"productId":501,"quantity":2}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create through gateway: %d %s", created.Code, created.Body)
	}
	var order orders.Order
	if err := json.Unmarshal(created.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}
	if order.ID != 1 || order.TotalAmount != 25 || order.Status != orders.StatusConfirmed {
		t.Fatalf("unexpected created order: %#v", order)
	}

	get := sendGatewayRequest(gatewayHandler, http.MethodGet, "/api/v1/orders/1", customerToken, "")
	if get.Code != http.StatusOK {
		t.Fatalf("get through gateway: %d %s", get.Code, get.Body)
	}
	list := sendGatewayRequest(gatewayHandler, http.MethodGet, "/api/v1/orders?limit=10&offset=0", customerToken, "")
	if list.Code != http.StatusOK || !bytes.Contains(list.Body.Bytes(), []byte(`"orders"`)) {
		t.Fatalf("list through gateway: %d %s", list.Code, list.Body)
	}
	forbidden := sendGatewayRequest(gatewayHandler, http.MethodPut, "/api/v1/orders/1/status", customerToken, `{"status":"CONFIRMED"}`)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("customer status update through gateway: %d", forbidden.Code)
	}
	updated := sendGatewayRequest(gatewayHandler, http.MethodPut, "/api/v1/orders/1/status", adminToken, `{"status":"CONFIRMED"}`)
	if updated.Code != http.StatusOK || !bytes.Contains(updated.Body.Bytes(), []byte(`"status":"CONFIRMED"`)) {
		t.Fatalf("admin status update through gateway: %d %s", updated.Code, updated.Body)
	}
	graphQL := sendGatewayRequest(gatewayHandler, http.MethodPost, "/graphql", customerToken, `{"query":"query { order(id:\"1\") { id customerId status createdAt updatedAt } }"}`)
	if graphQL.Code != http.StatusOK || !bytes.Contains(graphQL.Body.Bytes(), []byte(`"customerId":"101"`)) || !bytes.Contains(graphQL.Body.Bytes(), []byte(`"createdAt":`)) {
		t.Fatalf("GraphQL through gateway: %d %s", graphQL.Code, graphQL.Body)
	}
}

func TestGatewayCreatesMultipleOrdersThroughProductInventory(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	inventory := &memoryProducts{items: map[int64]product.Product{
		501: {ID: 501, Name: "Keyboard", Price: 12.5, Stock: 5},
	}}
	productService := httptest.NewServer(product.NewHandler(
		config.Config{JWTSecret: secret},
		logger,
		inventory,
		nil,
	))
	defer productService.Close()

	productClient, err := product.NewClient(config.Config{
		ProductServiceURL:   productService.URL,
		JWTSecret:           secret,
		ProductTimeout:      time.Second,
		ProductBreakerLimit: 5,
		ProductBreakerWait:  time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	orderService := httptest.NewServer(orders.NewHandler(
		config.Config{JWTSecret: secret},
		logger,
		orders.NewService(&memoryOrders{}, productClient),
		nil,
	))
	defer orderService.Close()
	gatewayHandler := New(config.Config{
		OrderServiceURL:   orderService.URL,
		ProductServiceURL: productService.URL,
		JWTSecret:         secret,
	}, logger)

	customerToken, err := auth.IssueToken(secret, "customer-101", auth.RoleCustomer, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	adminToken, err := auth.IssueToken(secret, "admin-1", auth.RoleAdmin, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	for _, customerID := range []int{101, 102} {
		body := fmt.Sprintf(`{"customerId":%d,"productId":501,"quantity":1}`, customerID)
		response := sendGatewayRequest(gatewayHandler, http.MethodPost, "/api/v1/orders", customerToken, body)
		if response.Code != http.StatusCreated {
			t.Fatalf("create order for customer %d: %d %s", customerID, response.Code, response.Body)
		}
	}
	if got := inventory.stock(501); got != 3 {
		t.Fatalf("stock after two successful orders = %d, want 3", got)
	}

	cancelled := sendGatewayRequest(gatewayHandler, http.MethodPut, "/api/v1/orders/1/status", adminToken, `{"status":"CANCELLED"}`)
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancel first order: %d %s", cancelled.Code, cancelled.Body)
	}
	if got := inventory.stock(501); got != 4 {
		t.Fatalf("stock after cancellation = %d, want 4", got)
	}
}

func sendGatewayRequest(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type memoryOrders struct {
	items  map[int64]orders.Order
	nextID int64
}

func (r *memoryOrders) Create(_ context.Context, order *orders.Order) error {
	if r.items == nil {
		r.items = make(map[int64]orders.Order)
	}
	r.nextID++
	order.ID = r.nextID
	order.CreatedAt = time.Now().UTC()
	order.UpdatedAt = order.CreatedAt
	r.items[order.ID] = *order
	return nil
}

func (r *memoryOrders) Delete(_ context.Context, id int64) error {
	if _, ok := r.items[id]; !ok {
		return orders.ErrNotFound
	}
	delete(r.items, id)
	return nil
}

func (r *memoryOrders) FindByID(_ context.Context, id int64) (*orders.Order, error) {
	order, ok := r.items[id]
	if !ok {
		return nil, orders.ErrNotFound
	}
	return &order, nil
}

func (r *memoryOrders) List(context.Context, int, int) ([]orders.Order, error) {
	result := make([]orders.Order, 0, len(r.items))
	for _, order := range r.items {
		result = append(result, order)
	}
	return result, nil
}

func (r *memoryOrders) UpdateStatus(_ context.Context, id int64, status orders.Status) (*orders.Order, error) {
	order, ok := r.items[id]
	if !ok {
		return nil, orders.ErrNotFound
	}
	order.Status = status
	order.UpdatedAt = time.Now().UTC()
	r.items[id] = order
	return &order, nil
}

type fixedPrice float64

func (p fixedPrice) Price(context.Context, int64) (float64, error) {
	return float64(p), nil
}

func (p fixedPrice) Reserve(context.Context, int64, int64, int) error { return nil }

func (p fixedPrice) Release(context.Context, int64) error { return nil }

type memoryProducts struct {
	items        map[int64]product.Product
	reservations map[int64]productReservation
}

type productReservation struct {
	productID int64
	quantity  int64
	released  bool
}

func (r *memoryProducts) Create(_ context.Context, item *product.Product) error {
	if r.items == nil {
		r.items = make(map[int64]product.Product)
	}
	r.items[item.ID] = *item
	return nil
}

func (r *memoryProducts) Delete(_ context.Context, id int64) error {
	if _, ok := r.items[id]; !ok {
		return product.ErrProductNotFound
	}
	delete(r.items, id)
	return nil
}

func (r *memoryProducts) Get(_ context.Context, id int64) (*product.Product, error) {
	item, ok := r.items[id]
	if !ok {
		return nil, product.ErrProductNotFound
	}
	return &item, nil
}

func (r *memoryProducts) List(context.Context, int, int) ([]product.Product, error) {
	items := make([]product.Product, 0, len(r.items))
	for _, item := range r.items {
		items = append(items, item)
	}
	return items, nil
}

func (r *memoryProducts) Reserve(_ context.Context, orderID, productID, quantity int64) error {
	item, ok := r.items[productID]
	if !ok {
		return product.ErrProductNotFound
	}
	if existing, ok := r.reservations[orderID]; ok {
		if existing.productID == productID && existing.quantity == quantity && !existing.released {
			return nil
		}
		return product.ErrReservation
	}
	if item.Stock < quantity {
		return product.ErrOutOfStock
	}
	item.Stock -= quantity
	r.items[productID] = item
	if r.reservations == nil {
		r.reservations = make(map[int64]productReservation)
	}
	r.reservations[orderID] = productReservation{productID: productID, quantity: quantity}
	return nil
}

func (r *memoryProducts) Release(_ context.Context, orderID int64) error {
	reservation, ok := r.reservations[orderID]
	if !ok || reservation.released {
		return nil
	}
	item, ok := r.items[reservation.productID]
	if !ok {
		return product.ErrProductNotFound
	}
	item.Stock += reservation.quantity
	r.items[reservation.productID] = item
	reservation.released = true
	r.reservations[orderID] = reservation
	return nil
}

func (r *memoryProducts) stock(id int64) int64 {
	return r.items[id].Stock
}

func TestGatewaySecurityHeadersAndCORS(t *testing.T) {
	handler := New(config.Config{
		OrderServiceURL: "http://127.0.0.1:1",
		JWTSecret:       []byte("0123456789abcdef0123456789abcdef"),
		CORSOrigins:     []string{"http://localhost:3000"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	live := httptest.NewRecorder()
	handler.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/health/live", nil))
	if live.Header().Get("X-Content-Type-Options") != "nosniff" || live.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("security headers missing: %v", live.Header())
	}
	if live.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS header must not be set without an Origin")
	}

	preflight := httptest.NewRequest(http.MethodOptions, "/api/v1/orders", nil)
	preflight.Header.Set("Origin", "http://localhost:3000")
	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, preflight)
	if allowed.Code != http.StatusNoContent || allowed.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatalf("allowed preflight: %d %v", allowed.Code, allowed.Header())
	}

	other := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	other.Header.Set("Origin", "http://evil.example")
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, other)
	if denied.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unlisted origin must not receive CORS headers")
	}
}
