package database

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"oms/internal/config"
)

//go:embed schema.sql
var schema string

// Open connects to PostgreSQL through GORM, configures the connection pool,
// and applies schema.sql (idempotent: CREATE ... IF NOT EXISTS).
func Open(ctx context.Context, cfg config.Config, log *slog.Logger) (*gorm.DB, error) {
	if cfg.DBUser == "" || cfg.DBPassword == "" {
		return nil, fmt.Errorf("DB_USER and DB_PASSWORD must be set")
	}
	if log == nil {
		log = slog.Default()
	}
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.DBUser, cfg.DBPassword),
		Host:     net.JoinHostPort(cfg.DBHost, cfg.DBPort),
		Path:     cfg.DBName,
		RawQuery: url.Values{"sslmode": {cfg.DBSSLMode}}.Encode(),
	}).String()
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:                 newQueryLogger(log),
		SkipDefaultTransaction: true,
		NowFunc:                func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL with GORM: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get SQL pool: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.DBMaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.DBMaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.DBConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	if err := initializeSchema(ctx, db); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the underlying connection pool.
func Close(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func initializeSchema(ctx context.Context, db *gorm.DB) error {
	schemaCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := db.WithContext(schemaCtx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", int64(714238510294)).Error; err != nil {
			return fmt.Errorf("lock database schema initialization: %w", err)
		}
		for _, statement := range strings.Split(schema, ";") {
			statement = strings.TrimSpace(statement)
			if statement == "" {
				continue
			}
			if err := tx.Exec(statement).Error; err != nil {
				return fmt.Errorf("initialize database schema: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("initialize database schema: %w", err)
	}
	return nil
}
