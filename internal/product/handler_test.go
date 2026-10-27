package product

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oms/internal/auth"
	"oms/internal/config"
)

type memoryCatalog struct {
	items        map[int64]Product
	nextID       int64
	reservations map[int64]reservation
}

func (m *memoryCatalog) Create(_ context.Context, item *Product) error {
	if m.items == nil {
		m.items = make(map[int64]Product)
	}
	m.nextID++
	item.ID = m.nextID
	m.items[item.ID] = *item
	return nil
}

func (m *memoryCatalog) Delete(_ context.Context, id int64) error {
	if _, ok := m.items[id]; !ok {
		return ErrProductNotFound
	}
	for _, reservation := range m.reservations {
		if reservation.ProductID == id && !reservation.Released {
			return ErrProductReserved
		}
	}
	delete(m.items, id)
	return nil
}

func (m *memoryCatalog) Get(_ context.Context, id int64) (*Product, error) {
	item, ok := m.items[id]
	if !ok {
		return nil, ErrProductNotFound
	}
	return &item, nil
}

func (m *memoryCatalog) List(context.Context, int, int) ([]Product, error) {
	items := make([]Product, 0, len(m.items))
	for _, item := range m.items {
		items = append(items, item)
	}
	return items, nil
}

func (m *memoryCatalog) Reserve(_ context.Context, orderID, productID, quantity int64) error {
	item, ok := m.items[productID]
	if !ok {
		return ErrProductNotFound
	}
	if previous, found := m.reservations[orderID]; found {
		if previous.ProductID == productID && previous.Quantity == quantity && !previous.Released {
			return nil
		}
		return ErrReservation
	}
	if item.Stock < quantity {
		return ErrOutOfStock
	}
	item.Stock -= quantity
	m.items[productID] = item
	if m.reservations == nil {
		m.reservations = make(map[int64]reservation)
	}
	m.reservations[orderID] = reservation{OrderID: orderID, ProductID: productID, Quantity: quantity}
	return nil
}

func (m *memoryCatalog) Release(_ context.Context, orderID int64) error {
	itemReservation, ok := m.reservations[orderID]
	if !ok || itemReservation.Released {
		return nil
	}
	item := m.items[itemReservation.ProductID]
	item.Stock += itemReservation.Quantity
	m.items[item.ID] = item
	itemReservation.Released = true
	m.reservations[orderID] = itemReservation
	return nil
}

func TestProductRoutesSupportReadCreateAndAdminDelete(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	catalog := &memoryCatalog{}
	handler := NewHandler(config.Config{JWTSecret: secret}, slog.New(slog.NewTextHandler(io.Discard, nil)), catalog, nil)
	customer := productToken(t, secret, auth.RoleCustomer)
	admin := productToken(t, secret, auth.RoleAdmin)

	forbidden := productRequest(handler, http.MethodPost, "/api/v1/products", customer, `{"name":"Keyboard","price":49.99,"stock":3}`)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("customer create status = %d, want 403", forbidden.Code)
	}
	created := productRequest(handler, http.MethodPost, "/api/v1/products", admin, `{"name":"Keyboard","price":49.99,"stock":3}`)
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"stock":3`) {
		t.Fatalf("admin create: %d %s", created.Code, created.Body)
	}
	customerRead := productRequest(handler, http.MethodGet, "/api/v1/products/1", customer, "")
	if customerRead.Code != http.StatusOK || !strings.Contains(customerRead.Body.String(), `"price":49.99`) {
		t.Fatalf("customer read: %d %s", customerRead.Code, customerRead.Body)
	}
	customerList := productRequest(handler, http.MethodGet, "/api/v1/products", customer, "")
	if customerList.Code != http.StatusOK || !strings.Contains(customerList.Body.String(), `"products"`) {
		t.Fatalf("customer list: %d %s", customerList.Code, customerList.Body)
	}
	internalPrice := productRequest(handler, http.MethodGet, "/internal/products/1/price", "", "")
	if internalPrice.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated internal price status = %d, want 401", internalPrice.Code)
	}
	internalPrice = productRequest(handler, http.MethodGet, "/internal/products/1/price", customer, "")
	if internalPrice.Code != http.StatusOK || !strings.Contains(internalPrice.Body.String(), `"price":49.99`) {
		t.Fatalf("authenticated internal price: %d %s", internalPrice.Code, internalPrice.Body)
	}
	customerDelete := productRequest(handler, http.MethodDelete, "/api/v1/products/1", customer, "")
	if customerDelete.Code != http.StatusForbidden {
		t.Fatalf("customer delete status = %d, want 403", customerDelete.Code)
	}
	adminDelete := productRequest(handler, http.MethodDelete, "/api/v1/products/1", admin, "")
	if adminDelete.Code != http.StatusNoContent {
		t.Fatalf("admin delete status = %d, want 204", adminDelete.Code)
	}
	if _, ok := catalog.items[1]; ok {
		t.Fatal("product was not deleted")
	}
}

func TestInternalReservationIsIdempotentAndRestoresStock(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	catalog := &memoryCatalog{}
	catalog.Create(context.Background(), &Product{Name: "Keyboard", Price: 49.99, Stock: 4})
	handler := NewHandler(config.Config{JWTSecret: secret}, slog.New(slog.NewTextHandler(io.Discard, nil)), catalog, nil)
	admin := productToken(t, secret, auth.RoleAdmin)

	reserve := productRequest(handler, http.MethodPost, "/internal/orders/42/reservation", admin, `{"productId":1,"quantity":3}`)
	if reserve.Code != http.StatusOK {
		t.Fatalf("reserve: %d %s", reserve.Code, reserve.Body)
	}
	if got := catalog.items[1].Stock; got != 1 {
		t.Fatalf("reserved stock = %d, want 1", got)
	}
	if deleted := productRequest(handler, http.MethodDelete, "/api/v1/products/1", admin, ""); deleted.Code != http.StatusConflict {
		t.Fatalf("delete product with active reservation: %d %s", deleted.Code, deleted.Body)
	}
	if retry := productRequest(handler, http.MethodPost, "/internal/orders/42/reservation", admin, `{"productId":1,"quantity":3}`); retry.Code != http.StatusOK {
		t.Fatalf("idempotent reserve retry: %d %s", retry.Code, retry.Body)
	}
	for range 2 {
		release := productRequest(handler, http.MethodDelete, "/internal/orders/42/reservation", admin, "")
		if release.Code != http.StatusOK {
			t.Fatalf("release: %d %s", release.Code, release.Body)
		}
	}
	if got := catalog.items[1].Stock; got != 4 {
		t.Fatalf("released stock = %d, want 4", got)
	}
}

func productToken(t *testing.T, secret []byte, role string) string {
	t.Helper()
	token, err := auth.IssueToken(secret, "product-test", role, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func productRequest(handler http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
