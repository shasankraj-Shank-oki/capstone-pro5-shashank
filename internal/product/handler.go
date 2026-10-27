package product

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"oms/internal/auth"
	"oms/internal/config"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Handler struct {
	logger     *slog.Logger
	repository Repository
	db         *gorm.DB
}

func NewHandler(cfg config.Config, logger *slog.Logger, repository Repository, db *gorm.DB) http.Handler {
	handler := &Handler{logger: logger, repository: repository, db: db}
	router := gin.New()
	router.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) {
		logger.Error("product handler panic", "method", c.Request.Method, "path", c.Request.URL.Path)
		writeProductError(c.Writer, http.StatusInternalServerError, "internal_error", "Internal server error")
		c.Abort()
	}))
	router.GET("/health/live", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "UP"})
	})
	router.GET("/health/ready", handler.ready)

	authenticated := auth.Authenticate(cfg.JWTSecret)
	admin := auth.RequireRole(auth.RoleAdmin)
	api := router.Group("/api/v1/products")
	api.GET("", gin.WrapH(authenticated(http.HandlerFunc(handler.list))))
	api.GET("/:id", productRoute(authenticated(http.HandlerFunc(handler.get))))
	api.POST("", gin.WrapH(authenticated(admin(http.HandlerFunc(handler.create)))))
	api.DELETE("/:id", productRoute(authenticated(admin(http.HandlerFunc(handler.delete)))))

	router.GET("/internal/products/:id/price", productRoute(authenticated(http.HandlerFunc(handler.price))))
	internal := router.Group("/internal/orders")
	internal.POST("/:id/reservation", productRoute(authenticated(admin(http.HandlerFunc(handler.reservation)))))
	internal.DELETE("/:id/reservation", productRoute(authenticated(admin(http.HandlerFunc(handler.release)))))

	return router
}

func productRoute(target http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if id := c.Param("id"); id != "" {
			c.Request.SetPathValue("id", id)
		}
		target.ServeHTTP(c.Writer, c.Request)
	}
}

func (h *Handler) ready(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	if h.db == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "NOT_READY"})
		return
	}
	sqlDB, err := h.db.DB()
	if err != nil {
		h.logger.Error("get database pool for readiness check", "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "NOT_READY"})
		return
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		h.logger.Error("database readiness check failed", "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "NOT_READY"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "READY"})
}

func (h *Handler) price(w http.ResponseWriter, r *http.Request) {
	id, err := productID(r.PathValue("id"))
	if err != nil {
		writeProductError(w, http.StatusBadRequest, "invalid_product_id", "Product ID must be a positive integer")
		return
	}
	item, err := h.repository.Get(r.Context(), id)
	if err != nil {
		h.respondError(w, err)
		return
	}
	writeProductJSON(w, http.StatusOK, struct {
		ProductID int64   `json:"productId"`
		Price     float64 `json:"price"`
	}{ProductID: item.ID, Price: item.Price})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := productID(r.PathValue("id"))
	if err != nil {
		writeProductError(w, http.StatusBadRequest, "invalid_product_id", "Product ID must be a positive integer")
		return
	}
	item, err := h.repository.Get(r.Context(), id)
	if err != nil {
		h.respondError(w, err)
		return
	}
	writeProductJSON(w, http.StatusOK, item)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	limit, err := queryProductInt(r, "limit", 50)
	if err != nil {
		writeProductError(w, http.StatusBadRequest, "invalid_pagination", "Invalid pagination parameters")
		return
	}
	offset, err := queryProductInt(r, "offset", 0)
	if err != nil || limit < 1 || limit > 100 || offset < 0 || offset > 100000 {
		writeProductError(w, http.StatusBadRequest, "invalid_pagination", "Invalid pagination parameters")
		return
	}
	items, err := h.repository.List(r.Context(), limit, offset)
	if err != nil {
		h.respondError(w, err)
		return
	}
	writeProductJSON(w, http.StatusOK, map[string]any{"products": items, "limit": limit, "offset": offset})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name  string  `json:"name"`
		Price float64 `json:"price"`
		Stock int64   `json:"stock"`
	}
	if !decodeProductJSON(w, r, &input) || !validProduct(input.Name, input.Price) || input.Stock < 0 {
		writeProductError(w, http.StatusBadRequest, "invalid_product", "Name, positive price, and non-negative stock are required")
		return
	}
	item := &Product{Name: strings.TrimSpace(input.Name), Price: roundPrice(input.Price), Stock: input.Stock}
	if err := h.repository.Create(r.Context(), item); err != nil {
		h.respondError(w, err)
		return
	}
	writeProductJSON(w, http.StatusCreated, item)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := productID(r.PathValue("id"))
	if err != nil {
		writeProductError(w, http.StatusBadRequest, "invalid_product_id", "Product ID must be a positive integer")
		return
	}
	if err := h.repository.Delete(r.Context(), id); err != nil {
		h.respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) reservation(w http.ResponseWriter, r *http.Request) {
	orderID, err := productID(r.PathValue("id"))
	if err != nil {
		writeProductError(w, http.StatusBadRequest, "invalid_order_id", "Order ID must be a positive integer")
		return
	}
	var input struct {
		ProductID int64 `json:"productId"`
		Quantity  int64 `json:"quantity"`
	}
	if !decodeProductJSON(w, r, &input) || input.ProductID <= 0 || input.Quantity < 1 || input.Quantity > 10000 {
		writeProductError(w, http.StatusBadRequest, "invalid_reservation", "Product ID and quantity are invalid")
		return
	}
	if err := h.repository.Reserve(r.Context(), orderID, input.ProductID, input.Quantity); err != nil {
		h.respondError(w, err)
		return
	}
	writeProductJSON(w, http.StatusOK, map[string]string{"status": "RESERVED"})
}

func (h *Handler) release(w http.ResponseWriter, r *http.Request) {
	orderID, err := productID(r.PathValue("id"))
	if err != nil {
		writeProductError(w, http.StatusBadRequest, "invalid_order_id", "Order ID must be a positive integer")
		return
	}
	if err := h.repository.Release(r.Context(), orderID); err != nil {
		h.respondError(w, err)
		return
	}
	writeProductJSON(w, http.StatusOK, map[string]string{"status": "RELEASED"})
}

func (h *Handler) respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrProductNotFound):
		writeProductError(w, http.StatusNotFound, "not_found", "Product not found")
	case errors.Is(err, ErrProductReserved):
		writeProductError(w, http.StatusConflict, "product_reserved", "Product has active stock reservations")
	case errors.Is(err, ErrOutOfStock):
		writeProductError(w, http.StatusConflict, "insufficient_stock", "Insufficient product stock")
	case errors.Is(err, ErrReservation):
		writeProductError(w, http.StatusConflict, "reservation_conflict", "Order has a different or released stock reservation")
	case errors.Is(err, ErrInvalidProduct):
		writeProductError(w, http.StatusBadRequest, "invalid_product", "Product input is invalid")
	default:
		h.logger.Error("product operation failed", "error", err)
		writeProductError(w, http.StatusInternalServerError, "internal_error", "Internal server error")
	}
}

func decodeProductJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return false
	}
	return errors.Is(decoder.Decode(&struct{}{}), io.EOF)
}

func validProduct(name string, price float64) bool {
	name = strings.TrimSpace(name)
	return len(name) > 0 && len(name) <= 200 && price > 0 && price <= 9999999999.99 &&
		!math.IsNaN(price) && !math.IsInf(price, 0) && roundPrice(price) > 0
}

func roundPrice(price float64) float64 {
	return math.Round(price*100) / 100
}

func productID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func queryProductInt(r *http.Request, name string, fallback int) (int, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	return strconv.Atoi(value)
}

func writeProductJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeProductError(w http.ResponseWriter, status int, code, message string) {
	writeProductJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
