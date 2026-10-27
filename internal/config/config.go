package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	AppPort             string
	OrderServicePort    string
	ProductServicePort  string
	OrderServiceURL     string
	ProductServiceURL   string
	DBHost              string
	DBPort              string
	DBName              string
	DBUser              string
	DBPassword          string
	DBSSLMode           string
	DBMaxOpenConns      int
	DBMaxIdleConns      int
	DBConnMaxLifetime   time.Duration
	AdminUsername       string
	AdminPassword       string
	UserUsername        string
	UserPassword        string
	LogLevel            string
	JWTSecret           []byte
	JWTTTL              time.Duration
	ProductTimeout      time.Duration
	ProductMaxRetries   int
	ProductRetryBase    time.Duration
	ProductBreakerLimit int
	ProductBreakerWait  time.Duration
	ShutdownTimeout     time.Duration
	CORSOrigins         []string
}

func Load() (Config, error) {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}
	cfg := Config{
		AppPort:            env("APP_PORT", "8080"),
		OrderServicePort:   env("ORDER_SERVICE_PORT", "8081"),
		ProductServicePort: env("PRODUCT_SERVICE_PORT", "8082"),
		OrderServiceURL:    env("ORDER_SERVICE_URL", "http://localhost:8081"),
		ProductServiceURL:  env("PRODUCT_SERVICE_URL", "http://localhost:8082"),
		DBHost:             env("DB_HOST", "localhost"),
		DBPort:             env("DB_PORT", "5432"),
		DBName:             env("DB_NAME", "oms"),
		DBUser:             os.Getenv("DB_USER"),
		DBPassword:         os.Getenv("DB_PASSWORD"),
		DBSSLMode:          env("DB_SSLMODE", "prefer"),
		AdminUsername:      env("AUTH_ADMIN_USERNAME", "admin"),
		AdminPassword:      os.Getenv("AUTH_ADMIN_PASSWORD"),
		UserUsername:       env("AUTH_USER_USERNAME", "user"),
		UserPassword:       os.Getenv("AUTH_USER_PASSWORD"),
		LogLevel:           env("LOG_LEVEL", "info"),
		JWTSecret:          []byte(os.Getenv("JWT_SECRET")),
		CORSOrigins:        splitList(os.Getenv("CORS_ALLOWED_ORIGINS")),
	}
	var err error
	if cfg.JWTTTL, err = duration("JWT_TTL", "15m"); err != nil {
		return Config{}, err
	}
	if cfg.ProductTimeout, err = duration("PRODUCT_TIMEOUT", "2s"); err != nil {
		return Config{}, err
	}
	if cfg.ProductRetryBase, err = duration("PRODUCT_RETRY_BASE_DELAY", "100ms"); err != nil {
		return Config{}, err
	}
	if cfg.ProductBreakerWait, err = duration("PRODUCT_BREAKER_RECOVERY", "15s"); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = duration("SHUTDOWN_TIMEOUT", "10s"); err != nil {
		return Config{}, err
	}
	if cfg.DBConnMaxLifetime, err = duration("DB_CONN_MAX_LIFETIME", "30m"); err != nil {
		return Config{}, err
	}
	if cfg.DBMaxOpenConns, err = integer("DB_MAX_OPEN_CONNS", 25); err != nil {
		return Config{}, err
	}
	if cfg.DBMaxIdleConns, err = integer("DB_MAX_IDLE_CONNS", 10); err != nil {
		return Config{}, err
	}
	if cfg.ProductMaxRetries, err = integer("PRODUCT_MAX_RETRIES", 2); err != nil {
		return Config{}, err
	}
	if cfg.ProductBreakerLimit, err = integer("PRODUCT_BREAKER_THRESHOLD", 5); err != nil {
		return Config{}, err
	}
	if cfg.DBPort, err = port("DB_PORT", cfg.DBPort); err != nil {
		return Config{}, err
	}
	if cfg.AppPort, err = port("APP_PORT", cfg.AppPort); err != nil {
		return Config{}, err
	}
	if cfg.OrderServicePort, err = port("ORDER_SERVICE_PORT", cfg.OrderServicePort); err != nil {
		return Config{}, err
	}
	if cfg.ProductServicePort, err = port("PRODUCT_SERVICE_PORT", cfg.ProductServicePort); err != nil {
		return Config{}, err
	}
	switch cfg.DBSSLMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return Config{}, fmt.Errorf("DB_SSLMODE must be one of disable, allow, prefer, require, verify-ca, verify-full")
	}
	if cfg.DBMaxOpenConns < 1 || cfg.DBMaxOpenConns > 200 || cfg.DBMaxIdleConns < 0 || cfg.DBMaxIdleConns > cfg.DBMaxOpenConns || cfg.DBConnMaxLifetime <= 0 {
		return Config{}, fmt.Errorf("database pool settings are outside allowed ranges")
	}
	for _, password := range []struct{ name, value string }{
		{"AUTH_ADMIN_PASSWORD", cfg.AdminPassword}, {"AUTH_USER_PASSWORD", cfg.UserPassword},
	} {
		if password.value != "" && len(password.value) < 12 {
			return Config{}, fmt.Errorf("%s must contain at least 12 characters", password.name)
		}
	}
	if cfg.AdminPassword != "" && cfg.UserPassword != "" && cfg.AdminUsername == cfg.UserUsername {
		return Config{}, fmt.Errorf("AUTH_ADMIN_USERNAME and AUTH_USER_USERNAME must differ")
	}
	if len(cfg.JWTSecret) < 32 {
		return Config{}, fmt.Errorf("JWT_SECRET must contain at least 32 bytes")
	}
	if cfg.JWTTTL <= 0 || cfg.ProductTimeout <= 0 || cfg.ProductRetryBase <= 0 || cfg.ProductBreakerWait <= 0 || cfg.ShutdownTimeout <= 0 {
		return Config{}, fmt.Errorf("duration configuration values must be positive")
	}
	if cfg.ProductTimeout > 30*time.Second || cfg.ProductRetryBase > 2*time.Second {
		return Config{}, fmt.Errorf("Product Service timeout must be at most 30s and retry base delay at most 2s")
	}
	if cfg.ProductMaxRetries < 0 || cfg.ProductMaxRetries > 5 || cfg.ProductBreakerLimit < 1 {
		return Config{}, fmt.Errorf("retry and breaker configuration values are outside allowed ranges")
	}
	productBudget := cfg.ProductTimeout * time.Duration(cfg.ProductMaxRetries+1)
	for attempt := 0; attempt < cfg.ProductMaxRetries; attempt++ {
		delay := cfg.ProductRetryBase * time.Duration(1<<attempt)
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
		productBudget += delay
	}
	if productBudget > 25*time.Second {
		return Config{}, fmt.Errorf("configured Product Service timeout and retry budget must not exceed 25s")
	}
	for _, item := range []struct{ name, rawURL string }{
		{name: "ORDER_SERVICE_URL", rawURL: cfg.OrderServiceURL},
		{name: "PRODUCT_SERVICE_URL", rawURL: cfg.ProductServiceURL},
	} {
		name, rawURL := item.name, item.rawURL
		parsed, err := url.Parse(rawURL)
		if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return Config{}, fmt.Errorf("%s must be an absolute HTTP URL", name)
		}
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return Config{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn, or error")
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func duration(key, fallback string) (time.Duration, error) {
	value, err := time.ParseDuration(env(key, fallback))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return value, nil
}

func integer(key string, fallback int) (int, error) {
	value, err := strconv.Atoi(env(key, strconv.Itoa(fallback)))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return value, nil
}

func port(key, value string) (string, error) {
	number, err := strconv.Atoi(value)
	if err != nil || number < 1 || number > 65535 {
		return "", fmt.Errorf("%s must be a valid TCP port", key)
	}
	return strconv.Itoa(number), nil
}

func splitList(value string) []string {
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}
