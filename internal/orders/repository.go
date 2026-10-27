package orders

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SQLRepository stores orders in PostgreSQL through GORM. GORM binds every
// value as a query parameter; no SQL is built from user input.
type SQLRepository struct {
	db *gorm.DB
}

func NewSQLRepository(db *gorm.DB) *SQLRepository {
	return &SQLRepository{db: db}
}

func (r *SQLRepository) Create(ctx context.Context, order *Order) error {
	if err := r.db.WithContext(ctx).Create(order).Error; err != nil {
		return fmt.Errorf("insert order: %w", err)
	}
	return nil
}

func (r *SQLRepository) Delete(ctx context.Context, id int64) error {
	result := r.db.WithContext(ctx).Delete(&Order{}, id)
	if result.Error != nil {
		return fmt.Errorf("delete order: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *SQLRepository) FindByID(ctx context.Context, id int64) (*Order, error) {
	order := new(Order)
	if err := r.db.WithContext(ctx).Where("id = ?", id).Take(order).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("find order: %w", err)
	}
	return order, nil
}

func (r *SQLRepository) List(ctx context.Context, limit, offset int) ([]Order, error) {
	orders := make([]Order, 0)
	err := r.db.WithContext(ctx).
		Order("created_at DESC, id DESC").
		Limit(limit).Offset(offset).
		Find(&orders).Error
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	return orders, nil
}

// UpdateStatus runs a single UPDATE ... RETURNING so the change and the
// returned row are atomic.
func (r *SQLRepository) UpdateStatus(ctx context.Context, id int64, status Status) (*Order, error) {
	order := new(Order)
	result := r.db.WithContext(ctx).
		Model(order).
		Clauses(clause.Returning{}).
		Where("id = ?", id).
		Update("status", status)
	if result.Error != nil {
		return nil, fmt.Errorf("update order status: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	return order, nil
}
