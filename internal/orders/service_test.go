package orders

import (
	"context"
	"errors"
	"testing"
	"time"

	"oms/internal/product"
)

type fakeRepository struct {
	order *Order
}

func (r *fakeRepository) Create(_ context.Context, order *Order) error {
	order.ID = 1
	order.CreatedAt = time.Now()
	order.UpdatedAt = order.CreatedAt
	r.order = order
	return nil
}

func (r *fakeRepository) Delete(_ context.Context, id int64) error {
	if r.order == nil || r.order.ID != id {
		return ErrNotFound
	}
	r.order = nil
	return nil
}

func (r *fakeRepository) FindByID(_ context.Context, id int64) (*Order, error) {
	if r.order == nil || r.order.ID != id {
		return nil, ErrNotFound
	}
	return r.order, nil
}

func (r *fakeRepository) List(context.Context, int, int) ([]Order, error) {
	if r.order == nil {
		return []Order{}, nil
	}
	return []Order{*r.order}, nil
}

func (r *fakeRepository) UpdateStatus(_ context.Context, id int64, status Status) (*Order, error) {
	order, err := r.FindByID(context.Background(), id)
	if err != nil {
		return nil, err
	}
	order.Status = status
	return order, nil
}

type fakePrices struct {
	price float64
	err   error
}

func (p fakePrices) Price(context.Context, int64) (float64, error) {
	return p.price, p.err
}

func (p fakePrices) Reserve(context.Context, int64, int64, int) error { return p.err }

func (p fakePrices) Release(context.Context, int64) error { return nil }

func TestCreateValidatesAndSnapshotsProductPrice(t *testing.T) {
	repo := &fakeRepository{}
	service := NewService(repo, fakePrices{price: 12.50})
	order, err := service.Create(context.Background(), CreateInput{CustomerID: 101, ProductID: 501, Quantity: 2})
	if err != nil {
		t.Fatal(err)
	}
	if order.TotalAmount != 25 || order.UnitPrice != 12.5 || order.Status != StatusConfirmed {
		t.Fatalf("unexpected order: %#v", order)
	}
	if repo.order.Status != StatusConfirmed {
		t.Fatalf("persisted status = %s, want %s", repo.order.Status, StatusConfirmed)
	}
	if _, err := service.Create(context.Background(), CreateInput{CustomerID: 0, ProductID: 501, Quantity: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected invalid input, got %v", err)
	}
}

func TestCreateMapsProductDependencyFailure(t *testing.T) {
	service := NewService(&fakeRepository{}, fakePrices{err: errors.New("offline")})
	_, err := service.Create(context.Background(), CreateInput{CustomerID: 1, ProductID: 1, Quantity: 1})
	if !errors.Is(err, ErrDependency) {
		t.Fatalf("expected dependency error, got %v", err)
	}
}

func TestCreateReturnsProductNotFound(t *testing.T) {
	service := NewService(&fakeRepository{}, fakePrices{err: product.ErrProductNotFound})
	_, err := service.Create(context.Background(), CreateInput{CustomerID: 1, ProductID: 42, Quantity: 1})
	if !errors.Is(err, ErrProductMissing) {
		t.Fatalf("expected product not found error, got %v", err)
	}
}

type inventoryPrices struct {
	reserveErr   error
	releaseErr   error
	releaseCalls int
}

func (p *inventoryPrices) Price(context.Context, int64) (float64, error) { return 12.5, nil }

func (p *inventoryPrices) Reserve(context.Context, int64, int64, int) error { return p.reserveErr }

func (p *inventoryPrices) Release(context.Context, int64) error {
	p.releaseCalls++
	return p.releaseErr
}

func TestCreateReservesAndCancellationReleasesInventory(t *testing.T) {
	repository := &fakeRepository{}
	inventory := &inventoryPrices{}
	service := NewService(repository, inventory)
	order, err := service.Create(context.Background(), CreateInput{CustomerID: 1, ProductID: 5, Quantity: 2})
	if err != nil {
		t.Fatal(err)
	}
	if order.ID == 0 || repository.order == nil {
		t.Fatalf("order was not persisted before reservation: %#v", order)
	}
	for range 2 {
		order, err = service.UpdateStatus(context.Background(), order.ID, StatusCancelled)
		if err != nil {
			t.Fatal(err)
		}
	}
	if order.Status != StatusCancelled || inventory.releaseCalls != 2 {
		t.Fatalf("status=%s release calls=%d", order.Status, inventory.releaseCalls)
	}
}

func TestCreateRemovesOrderWhenStockReservationFails(t *testing.T) {
	repository := &fakeRepository{}
	service := NewService(repository, &inventoryPrices{reserveErr: product.ErrOutOfStock})
	_, err := service.Create(context.Background(), CreateInput{CustomerID: 1, ProductID: 5, Quantity: 2})
	if !errors.Is(err, ErrOutOfStock) {
		t.Fatalf("expected out-of-stock error, got %v", err)
	}
	if repository.order != nil {
		t.Fatalf("failed order was not rolled back: %#v", repository.order)
	}
}

func TestCreateCompensatesWhenOrderConfirmationFails(t *testing.T) {
	repository := &confirmationFailRepository{fakeRepository: fakeRepository{}}
	inventory := &inventoryPrices{}
	service := NewService(repository, inventory)
	_, err := service.Create(context.Background(), CreateInput{CustomerID: 1, ProductID: 5, Quantity: 2})
	if !errors.Is(err, ErrDependency) {
		t.Fatalf("expected dependency error, got %v", err)
	}
	if inventory.releaseCalls != 1 {
		t.Fatalf("reservation release calls = %d, want 1", inventory.releaseCalls)
	}
	if repository.order != nil {
		t.Fatalf("order was not removed after confirmation failure: %#v", repository.order)
	}
}

type confirmationFailRepository struct {
	fakeRepository
}

func (r *confirmationFailRepository) UpdateStatus(_ context.Context, id int64, status Status) (*Order, error) {
	if status == StatusConfirmed {
		return nil, errors.New("database unavailable")
	}
	return r.fakeRepository.UpdateStatus(context.Background(), id, status)
}

func TestCancellationReportsProductServiceFailure(t *testing.T) {
	repository := &fakeRepository{}
	inventory := &inventoryPrices{}
	service := NewService(repository, inventory)
	order, err := service.Create(context.Background(), CreateInput{CustomerID: 1, ProductID: 5, Quantity: 1})
	if err != nil {
		t.Fatal(err)
	}
	inventory.releaseErr = errors.New("offline")
	if _, err := service.UpdateStatus(context.Background(), order.ID, StatusCancelled); !errors.Is(err, ErrDependency) {
		t.Fatalf("expected dependency failure, got %v", err)
	}
	if repository.order.Status != StatusCancelled {
		t.Fatalf("cancellation status was not persisted for retry: %s", repository.order.Status)
	}
}
