//go:build integration

package orders_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"oms/internal/config"
	"oms/internal/database"
	"oms/internal/orders"
)

// Run with a reachable PostgreSQL: go test -tags integration ./internal/orders
// Uses DB_HOST, DB_PORT, DB_NAME, DB_USER, DB_PASSWORD (and optional DB_SSLMODE).
func TestGormRepositoryAgainstPostgres(t *testing.T) {
	if os.Getenv("DB_USER") == "" || os.Getenv("DB_PASSWORD") == "" {
		t.Skip("DB_USER and DB_PASSWORD not set")
	}
	getenv := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}
	cfg := config.Config{
		DBHost: getenv("DB_HOST", "localhost"), DBPort: getenv("DB_PORT", "5432"), DBName: getenv("DB_NAME", "oms"),
		DBUser: os.Getenv("DB_USER"), DBPassword: os.Getenv("DB_PASSWORD"), DBSSLMode: getenv("DB_SSLMODE", "prefer"),
		DBMaxOpenConns: 5, DBMaxIdleConns: 2, DBConnMaxLifetime: time.Minute,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := database.Open(ctx, cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close(db)
	repo := orders.NewSQLRepository(db)

	order := &orders.Order{CustomerID: 101, ProductID: 501, Quantity: 2, UnitPrice: 12.5, TotalAmount: 25, Status: orders.StatusCreated}
	if err := repo.Create(ctx, order); err != nil || order.ID == 0 {
		t.Fatalf("create: id=%d err=%v", order.ID, err)
	}
	found, err := repo.FindByID(ctx, order.ID)
	if err != nil || found.TotalAmount != 25 || found.Status != orders.StatusCreated {
		t.Fatalf("find: %#v err=%v", found, err)
	}
	updated, err := repo.UpdateStatus(ctx, order.ID, orders.StatusConfirmed)
	if err != nil || updated.Status != orders.StatusConfirmed || updated.ID != order.ID {
		t.Fatalf("update: %#v err=%v", updated, err)
	}
	list, err := repo.List(ctx, 5, 0)
	if err != nil || len(list) == 0 {
		t.Fatalf("list: len=%d err=%v", len(list), err)
	}
	if _, err := repo.FindByID(ctx, 1<<60); !errors.Is(err, orders.ErrNotFound) {
		t.Fatalf("missing order: got %v, want ErrNotFound", err)
	}
	if _, err := repo.UpdateStatus(ctx, 1<<60, orders.StatusCancelled); !errors.Is(err, orders.ErrNotFound) {
		t.Fatalf("update missing order: got %v, want ErrNotFound", err)
	}
}
