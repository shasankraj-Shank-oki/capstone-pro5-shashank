package product

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"oms/internal/auth"
	"oms/internal/config"
	"oms/internal/observability"
)

var (
	ErrUnavailable = errors.New("product service unavailable")
)

type Client interface {
	Price(ctx context.Context, productID int64) (float64, error)
	Reserve(ctx context.Context, orderID, productID int64, quantity int) error
	Release(ctx context.Context, orderID int64) error
}

type client struct {
	baseURL   string
	jwtSecret []byte
	http      *http.Client
	timeout   time.Duration
	retries   int
	retryBase time.Duration
	breaker   *circuitBreaker
}

type circuitBreaker struct {
	mu        sync.Mutex
	failures  int
	threshold int
	recovery  time.Duration
	openedAt  time.Time
	halfOpen  bool
}

func NewClient(cfg config.Config) (Client, error) {
	base, err := url.Parse(cfg.ProductServiceURL)
	if err != nil || base.Scheme != "http" || base.Host == "" {
		return nil, errors.New("PRODUCT_SERVICE_URL must be an absolute HTTP URL")
	}
	return &client{
		baseURL:   strings.TrimRight(base.String(), "/"),
		jwtSecret: cfg.JWTSecret,
		http: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: cfg.ProductTimeout, KeepAlive: 30 * time.Second}).DialContext,
				MaxIdleConns:          20,
				MaxIdleConnsPerHost:   5,
				IdleConnTimeout:       30 * time.Second,
				ResponseHeaderTimeout: cfg.ProductTimeout,
			},
		},
		timeout:   cfg.ProductTimeout,
		retries:   cfg.ProductMaxRetries,
		retryBase: cfg.ProductRetryBase,
		breaker: &circuitBreaker{
			threshold: cfg.ProductBreakerLimit,
			recovery:  cfg.ProductBreakerWait,
		},
	}, nil
}

func (c *client) Reserve(ctx context.Context, orderID, productID int64, quantity int) error {
	body, err := json.Marshal(struct {
		ProductID int64 `json:"productId"`
		Quantity  int   `json:"quantity"`
	}{ProductID: productID, Quantity: quantity})
	if err != nil {
		return fmt.Errorf("%w: encode stock reservation: %v", ErrUnavailable, err)
	}
	status, code, err := c.stockRequest(ctx, http.MethodPost, "/internal/orders/"+strconv.FormatInt(orderID, 10)+"/reservation", body)
	if err != nil {
		return err
	}
	if status == http.StatusConflict && code == "insufficient_stock" {
		return ErrOutOfStock
	}
	if status != http.StatusOK {
		return fmt.Errorf("%w: reservation request returned %s", ErrUnavailable, http.StatusText(status))
	}
	return nil
}

func (c *client) Release(ctx context.Context, orderID int64) error {
	status, _, err := c.stockRequest(ctx, http.MethodDelete, "/internal/orders/"+strconv.FormatInt(orderID, 10)+"/reservation", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%w: release request returned %s", ErrUnavailable, http.StatusText(status))
	}
	return nil
}

func (c *client) stockRequest(ctx context.Context, method, path string, body []byte) (int, string, error) {
	token, err := auth.IssueToken(c.jwtSecret, "order-service", auth.RoleAdmin, time.Minute)
	if err != nil {
		return 0, "", fmt.Errorf("%w: create product service token: %v", ErrUnavailable, err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("%w: create stock request: %v", ErrUnavailable, err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if requestID := observability.RequestID(ctx); requestID != "" {
		request.Header.Set("X-Request-ID", requestID)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, "", fmt.Errorf("%w: stock request failed: %v", ErrUnavailable, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return 0, "", fmt.Errorf("%w: read stock response: %v", ErrUnavailable, err)
	}
	var result struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return 0, "", fmt.Errorf("%w: decode stock response: %v", ErrUnavailable, err)
	}
	return response.StatusCode, result.Error.Code, nil
}

func (c *client) Price(ctx context.Context, productID int64) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("%w: request canceled", ErrUnavailable)
	}
	if !c.breaker.allow(time.Now()) {
		return 0, ErrUnavailable
	}
	price, breakerFailure, err := c.fetchWithRetry(ctx, productID)
	if ctx.Err() != nil {
		c.breaker.release()
		return price, err
	}
	c.breaker.record(!breakerFailure, time.Now())
	return price, err
}

func (c *client) fetchWithRetry(ctx context.Context, productID int64) (float64, bool, error) {
	var lastErr error
	for attempt := 0; attempt <= c.retries; attempt++ {
		price, retryable, err := c.fetch(ctx, productID)
		if err == nil {
			return price, false, nil
		}
		if ctx.Err() != nil {
			return 0, false, fmt.Errorf("%w: request canceled", ErrUnavailable)
		}
		lastErr = err
		if !retryable || attempt == c.retries {
			if errors.Is(lastErr, ErrProductNotFound) {
				return 0, false, lastErr
			}
			return 0, retryable, fmt.Errorf("%w: %v", ErrUnavailable, lastErr)
		}
		delay := c.retryBase * time.Duration(1<<attempt)
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, false, fmt.Errorf("%w: request canceled", ErrUnavailable)
		case <-timer.C:
		}
	}
	return 0, true, fmt.Errorf("%w: %v", ErrUnavailable, lastErr)
}

func (c *client) fetch(ctx context.Context, productID int64) (float64, bool, error) {
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	token, err := auth.IssueToken(c.jwtSecret, "order-service", auth.RoleAdmin, time.Minute)
	if err != nil {
		return 0, false, fmt.Errorf("create product service token: %w", err)
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, c.baseURL+"/internal/products/"+strconv.FormatInt(productID, 10)+"/price", nil)
	if err != nil {
		return 0, false, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if requestID := observability.RequestID(ctx); requestID != "" {
		request.Header.Set("X-Request-ID", requestID)
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return 0, false, err
		}
		return 0, true, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= 500 {
		return 0, true, fmt.Errorf("product service returned %s", response.Status)
	}
	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusNotFound {
			return 0, false, ErrProductNotFound
		}
		return 0, false, fmt.Errorf("product service returned %s", response.Status)
	}
	var result struct {
		ProductID int64   `json:"productId"`
		Price     float64 `json:"price"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&result); err != nil {
		return 0, true, fmt.Errorf("decode product response: %w", err)
	}
	if result.ProductID != productID || result.Price <= 0 || result.Price > 9999999999.99 || math.IsNaN(result.Price) || math.IsInf(result.Price, 0) {
		return 0, true, errors.New("product service returned invalid price data")
	}
	return math.Round(result.Price*100) / 100, false, nil
}

func (b *circuitBreaker) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openedAt.IsZero() {
		return true
	}
	if now.Sub(b.openedAt) < b.recovery || b.halfOpen {
		return false
	}
	b.halfOpen = true
	return true
}

func (b *circuitBreaker) record(success bool, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if success {
		b.failures = 0
		b.openedAt = time.Time{}
		b.halfOpen = false
		return
	}
	b.failures++
	if b.failures >= b.threshold || b.halfOpen {
		b.openedAt = now
		b.halfOpen = false
	}
}

// release frees a half-open probe slot when the caller cancelled, so the
// breaker is neither closed by a non-result nor stuck rejecting forever.
func (b *circuitBreaker) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.halfOpen = false
}
