package orders

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound       = errors.New("order not found")
	ErrProductMissing = errors.New("product not found")
	ErrInvalid        = errors.New("invalid order")
	ErrDependency     = errors.New("dependency unavailable")
	ErrOutOfStock     = errors.New("product out of stock")
)

type Status string

const (
	StatusCreated   Status = "CREATED"
	StatusConfirmed Status = "CONFIRMED"
	StatusCancelled Status = "CANCELLED"
)

func ValidStatus(status Status) bool {
	return status == StatusCreated || status == StatusConfirmed || status == StatusCancelled
}

type Order struct {
	ID          int64     `json:"id" gorm:"primaryKey;autoIncrement"`
	CustomerID  int64     `json:"customerId" gorm:"not null"`
	ProductID   int64     `json:"productId" gorm:"not null"`
	Quantity    int       `json:"quantity" gorm:"not null"`
	UnitPrice   float64   `json:"unitPrice" gorm:"type:numeric(12,2);not null"`
	TotalAmount float64   `json:"totalAmount" gorm:"type:numeric(14,2);not null"`
	Status      Status    `json:"status" gorm:"type:text;not null"`
	CreatedAt   time.Time `json:"createdAt" gorm:"autoCreateTime"`
	UpdatedAt   time.Time `json:"updatedAt" gorm:"autoUpdateTime"`
}

// TableName pins the GORM table to the one created by internal/database/schema.sql.
func (Order) TableName() string { return "orders" }

type CreateInput struct {
	CustomerID int64 `json:"customerId"`
	ProductID  int64 `json:"productId"`
	Quantity   int   `json:"quantity"`
}

type Repository interface {
	Create(ctx context.Context, order *Order) error
	Delete(ctx context.Context, id int64) error
	FindByID(ctx context.Context, id int64) (*Order, error)
	List(ctx context.Context, limit, offset int) ([]Order, error)
	UpdateStatus(ctx context.Context, id int64, status Status) (*Order, error)
}

type ProductClient interface {
	Price(ctx context.Context, productID int64) (float64, error)
	Reserve(ctx context.Context, orderID, productID int64, quantity int) error
	Release(ctx context.Context, orderID int64) error
}
