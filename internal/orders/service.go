package orders

import (
	"context"
	"errors"
	"time"

	"oms/internal/product"
)

type Service struct {
	repository Repository
	prices     ProductClient
}

func NewService(repository Repository, prices ProductClient) *Service {
	return &Service{repository: repository, prices: prices}
}

func (s *Service) Create(ctx context.Context, input CreateInput) (*Order, error) {
	if input.CustomerID <= 0 || input.ProductID <= 0 || input.Quantity < 1 || input.Quantity > 10000 {
		return nil, ErrInvalid
	}
	price, err := s.prices.Price(ctx, input.ProductID)
	if err != nil {
		if errors.Is(err, product.ErrProductNotFound) {
			return nil, ErrProductMissing
		}
		return nil, errors.Join(ErrDependency, err)
	}
	total := price * float64(input.Quantity)
	if total > 999999999999.99 {
		return nil, ErrInvalid
	}
	order := &Order{
		CustomerID:  input.CustomerID,
		ProductID:   input.ProductID,
		Quantity:    input.Quantity,
		UnitPrice:   price,
		TotalAmount: total,
		Status:      StatusCreated,
	}
	if err := s.repository.Create(ctx, order); err != nil {
		return nil, err
	}
	if err := s.prices.Reserve(ctx, order.ID, order.ProductID, order.Quantity); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if releaseErr := s.prices.Release(cleanupCtx, order.ID); releaseErr == nil {
			if deleteErr := s.repository.Delete(cleanupCtx, order.ID); deleteErr != nil {
				_, statusErr := s.repository.UpdateStatus(cleanupCtx, order.ID, StatusCancelled)
				err = errors.Join(err, deleteErr, statusErr)
			}
		} else {
			_, statusErr := s.repository.UpdateStatus(cleanupCtx, order.ID, StatusCancelled)
			err = errors.Join(err, releaseErr, statusErr)
		}
		if errors.Is(err, product.ErrOutOfStock) {
			return nil, errors.Join(ErrOutOfStock, err)
		}
		if errors.Is(err, product.ErrProductNotFound) {
			return nil, errors.Join(ErrProductMissing, err)
		}
		return nil, errors.Join(ErrDependency, err)
	}
	confirmed, err := s.repository.UpdateStatus(ctx, order.ID, StatusConfirmed)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if releaseErr := s.prices.Release(cleanupCtx, order.ID); releaseErr == nil {
			if deleteErr := s.repository.Delete(cleanupCtx, order.ID); deleteErr != nil {
				_, statusErr := s.repository.UpdateStatus(cleanupCtx, order.ID, StatusCancelled)
				err = errors.Join(err, deleteErr, statusErr)
			}
		} else {
			_, statusErr := s.repository.UpdateStatus(cleanupCtx, order.ID, StatusCancelled)
			err = errors.Join(err, releaseErr, statusErr)
		}
		return nil, errors.Join(ErrDependency, err)
	}
	return confirmed, nil
}

func (s *Service) Get(ctx context.Context, id int64) (*Order, error) {
	if id <= 0 {
		return nil, ErrInvalid
	}
	return s.repository.FindByID(ctx, id)
}

func (s *Service) List(ctx context.Context, limit, offset int) ([]Order, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 100000 {
		return nil, ErrInvalid
	}
	return s.repository.List(ctx, limit, offset)
}

func (s *Service) UpdateStatus(ctx context.Context, id int64, status Status) (*Order, error) {
	if id <= 0 || !ValidStatus(status) {
		return nil, ErrInvalid
	}
	order, err := s.repository.UpdateStatus(ctx, id, status)
	if err != nil {
		return nil, err
	}
	if status == StatusCancelled {
		if err := s.prices.Release(ctx, id); err != nil {
			return nil, errors.Join(ErrDependency, err)
		}
	}
	return order, nil
}
