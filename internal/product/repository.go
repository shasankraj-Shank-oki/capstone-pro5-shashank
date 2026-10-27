package product

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrProductNotFound = errors.New("product not found")
	ErrOutOfStock      = errors.New("insufficient product stock")
	ErrInvalidProduct  = errors.New("invalid product")
	ErrReservation     = errors.New("reservation conflict")
	ErrProductReserved = errors.New("product has active reservations")
)

type Product struct {
	ID        int64     `json:"id" gorm:"primaryKey;autoIncrement"`
	Name      string    `json:"name" gorm:"size:200;not null"`
	Price     float64   `json:"price" gorm:"type:numeric(12,2);not null;check:price > 0"`
	Stock     int64     `json:"stock" gorm:"not null;default:0;check:stock >= 0"`
	CreatedAt time.Time `json:"createdAt" gorm:"autoCreateTime"`
	UpdatedAt time.Time `json:"updatedAt" gorm:"autoUpdateTime"`
}

func (Product) TableName() string { return "products" }

type reservation struct {
	OrderID   int64 `gorm:"primaryKey"`
	ProductID int64 `gorm:"not null"`
	Quantity  int64 `gorm:"not null"`
	Released  bool  `gorm:"not null;default:false"`
}

func (reservation) TableName() string { return "stock_reservations" }

type Repository interface {
	Create(ctx context.Context, product *Product) error
	Delete(ctx context.Context, id int64) error
	Get(ctx context.Context, id int64) (*Product, error)
	List(ctx context.Context, limit, offset int) ([]Product, error)
	Reserve(ctx context.Context, orderID, productID, quantity int64) error
	Release(ctx context.Context, orderID int64) error
}

type SQLRepository struct {
	db *gorm.DB
}

func NewSQLRepository(db *gorm.DB) *SQLRepository {
	return &SQLRepository{db: db}
}

func (r *SQLRepository) Create(ctx context.Context, product *Product) error {
	if err := r.db.WithContext(ctx).Create(product).Error; err != nil {
		return fmt.Errorf("create product: %w", err)
	}
	return nil
}

func (r *SQLRepository) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var product Product
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&product).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrProductNotFound
		}
		if err != nil {
			return fmt.Errorf("lock product for deletion: %w", err)
		}
		var activeReservations int64
		if err := tx.Model(&reservation{}).
			Where("product_id = ? AND released = ?", id, false).
			Count(&activeReservations).Error; err != nil {
			return fmt.Errorf("check product reservations: %w", err)
		}
		if activeReservations > 0 {
			return ErrProductReserved
		}
		if err := tx.Delete(&product).Error; err != nil {
			return fmt.Errorf("delete product: %w", err)
		}
		return nil
	})
}

func (r *SQLRepository) Get(ctx context.Context, id int64) (*Product, error) {
	product := new(Product)
	if err := r.db.WithContext(ctx).Where("id = ?", id).Take(product).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrProductNotFound
		}
		return nil, fmt.Errorf("get product: %w", err)
	}
	return product, nil
}

func (r *SQLRepository) List(ctx context.Context, limit, offset int) ([]Product, error) {
	products := make([]Product, 0)
	if err := r.db.WithContext(ctx).Order("id ASC").Limit(limit).Offset(offset).Find(&products).Error; err != nil {
		return nil, fmt.Errorf("list products: %w", err)
	}
	return products, nil
}

func (r *SQLRepository) Reserve(ctx context.Context, orderID, productID, quantity int64) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing reservation
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id = ?", orderID).Take(&existing).Error
		if err == nil {
			if existing.ProductID == productID && existing.Quantity == quantity && !existing.Released {
				return nil
			}
			return ErrReservation
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("check stock reservation: %w", err)
		}
		result := tx.Model(&Product{}).
			Where("id = ? AND stock >= ?", productID, quantity).
			Update("stock", gorm.Expr("stock - ?", quantity))
		if result.Error != nil {
			return fmt.Errorf("reserve product stock: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			var count int64
			if err := tx.Model(&Product{}).Where("id = ?", productID).Count(&count).Error; err != nil {
				return fmt.Errorf("check product: %w", err)
			}
			if count == 0 {
				return ErrProductNotFound
			}
			return ErrOutOfStock
		}
		if err := tx.Create(&reservation{OrderID: orderID, ProductID: productID, Quantity: quantity}).Error; err != nil {
			return fmt.Errorf("record stock reservation: %w", err)
		}
		return nil
	})
	if err == nil {
		return nil
	}
	var existing reservation
	if lookupErr := r.db.WithContext(ctx).Where("order_id = ?", orderID).Take(&existing).Error; lookupErr == nil &&
		existing.ProductID == productID && existing.Quantity == quantity && !existing.Released {
		return nil
	}
	return err
}

func (r *SQLRepository) Release(ctx context.Context, orderID int64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var item reservation
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id = ?", orderID).Take(&item).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find stock reservation: %w", err)
		}
		if item.Released {
			return nil
		}
		result := tx.Model(&Product{}).
			Where("id = ?", item.ProductID).
			Update("stock", gorm.Expr("stock + ?", item.Quantity))
		if result.Error != nil {
			return fmt.Errorf("release product stock: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return ErrProductNotFound
		}
		if err := tx.Model(&item).Update("released", true).Error; err != nil {
			return fmt.Errorf("mark stock reservation released: %w", err)
		}
		return nil
	})
}
