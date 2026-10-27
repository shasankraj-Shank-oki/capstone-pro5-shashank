package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/graphql-go/graphql"
	"gorm.io/gorm"

	"oms/internal/auth"
	"oms/internal/config"
	"oms/internal/observability"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	logger  *slog.Logger
	service *Service
	db      *gorm.DB
	metrics *observability.Metrics
	schema  graphql.Schema
}

func NewHandler(cfg config.Config, logger *slog.Logger, service *Service, db *gorm.DB) http.Handler {
	handler := &Handler{logger: logger, service: service, db: db, metrics: observability.NewMetrics("oms_order_service")}
	handler.schema = handler.graphQLSchema()
	router := gin.New()
	router.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) {
		logger.Error("order handler panic", "request_id", observability.RequestID(c.Request.Context()), "method", c.Request.Method, "path", c.Request.URL.Path)
		writeError(c.Writer, c.Request, http.StatusInternalServerError, "internal_error", "Internal server error")
		c.Abort()
	}))
	router.GET("/health/live", gin.WrapF(handler.live))
	router.GET("/health/ready", gin.WrapF(handler.ready))
	router.GET("/metrics", gin.WrapH(handler.metrics.Handler()))
	authenticated := auth.Authenticate(cfg.JWTSecret)
	route := func(target http.Handler) gin.HandlerFunc {
		return func(c *gin.Context) {
			if id := c.Param("id"); id != "" {
				c.Request.SetPathValue("id", id)
			}
			target.ServeHTTP(c.Writer, c.Request)
		}
	}
	api := router.Group("/api")
	v1 := api.Group("/v1")
	orderRoutes := v1.Group("/orders")
	orderRoutes.POST("", route(authenticated(http.HandlerFunc(handler.create))))
	orderRoutes.GET("", route(authenticated(http.HandlerFunc(handler.list))))
	orderRoutes.GET("/:id", route(authenticated(http.HandlerFunc(handler.get))))
	orderRoutes.PUT("/:id/status", route(authenticated(auth.RequireRole(auth.RoleAdmin)(http.HandlerFunc(handler.updateStatus)))))
	router.POST("/graphql", route(authenticated(http.HandlerFunc(handler.graphql))))
	return handler.metrics.Middleware(logger, router)
}

func (h *Handler) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "UP"})
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.pingDatabase(ctx); err != nil {
		h.metrics.DatabaseError()
		h.logger.Error("database readiness check failed", "request_id", observability.RequestID(r.Context()), "error", err)
		writeError(w, r, http.StatusServiceUnavailable, "not_ready", "Database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "READY"})
}

func (h *Handler) pingDatabase(ctx context.Context) error {
	if h.db == nil {
		return errors.New("database not configured")
	}
	sqlDB, err := h.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var input CreateInput
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must contain one JSON object")
		return
	}
	order, err := h.service.Create(r.Context(), input)
	if err != nil {
		h.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, order)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_order_id", "Order ID must be a positive integer")
		return
	}
	order, err := h.service.Get(r.Context(), id)
	if err != nil {
		h.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	limit, err := queryInt(r, "limit", 50)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_pagination", "Invalid pagination parameters")
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_pagination", "Invalid pagination parameters")
		return
	}
	orders, err := h.service.List(r.Context(), limit, offset)
	if err != nil {
		h.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders, "limit": limit, "offset": offset})
}

func (h *Handler) updateStatus(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_order_id", "Order ID must be a positive integer")
		return
	}
	var input struct {
		Status Status `json:"status"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Invalid request body")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body must contain one JSON object")
		return
	}
	order, err := h.service.UpdateStatus(r.Context(), id, input.Status)
	if err != nil {
		h.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, order)
}

func (h *Handler) graphql(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Query         string         `json:"query"`
		OperationName string         `json:"operationName"`
		Variables     map[string]any `json:"variables"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.Query) == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_graphql_request", "Invalid GraphQL request")
		return
	}
	result := graphql.Do(graphql.Params{
		Schema:         h.schema,
		RequestString:  request.Query,
		OperationName:  request.OperationName,
		VariableValues: request.Variables,
		Context:        r.Context(),
	})
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) graphQLSchema() graphql.Schema {
	statusType := graphql.NewEnum(graphql.EnumConfig{
		Name: "OrderStatus",
		Values: graphql.EnumValueConfigMap{
			string(StatusCreated):   &graphql.EnumValueConfig{Value: string(StatusCreated)},
			string(StatusConfirmed): &graphql.EnumValueConfig{Value: string(StatusConfirmed)},
			string(StatusCancelled): &graphql.EnumValueConfig{Value: string(StatusCancelled)},
		},
	})
	orderType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Order",
		Fields: graphql.Fields{
			"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"customerId":  &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"productId":   &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"quantity":    &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"unitPrice":   &graphql.Field{Type: graphql.NewNonNull(graphql.Float)},
			"totalAmount": &graphql.Field{Type: graphql.NewNonNull(graphql.Float)},
			"status": &graphql.Field{
				Type: graphql.NewNonNull(statusType),
				Resolve: func(p graphql.ResolveParams) (any, error) {
					if order, ok := p.Source.(*Order); ok {
						return string(order.Status), nil
					}
					if order, ok := p.Source.(Order); ok {
						return string(order.Status), nil
					}
					return nil, errors.New("invalid order")
				},
			},
			"createdAt": &graphql.Field{Type: graphql.NewNonNull(graphql.String), Resolve: timeField(func(o Order) time.Time { return o.CreatedAt })},
			"updatedAt": &graphql.Field{Type: graphql.NewNonNull(graphql.String), Resolve: timeField(func(o Order) time.Time { return o.UpdatedAt })},
		},
	})
	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "CreateOrderInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"customerId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"productId":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"quantity":   &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.Int)},
		},
	})
	idArg := graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.ID)}
	query := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"order": &graphql.Field{
				Type: orderType, Args: graphql.FieldConfigArgument{"id": &idArg},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					id, err := parseID(fmt.Sprint(p.Args["id"]))
					if err != nil {
						return nil, errors.New("invalid order ID")
					}
					return h.runGraphQL(p.Context, func(ctx context.Context) (any, error) { return h.service.Get(ctx, id) })
				},
			},
			"orders": &graphql.Field{
				Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(orderType))),
				Args: graphql.FieldConfigArgument{
					"limit":  &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 50},
					"offset": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
				},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					return h.runGraphQL(p.Context, func(ctx context.Context) (any, error) {
						return h.service.List(ctx, integerArg(p.Args["limit"], 50), integerArg(p.Args["offset"], 0))
					})
				},
			},
		},
	})
	mutation := graphql.NewObject(graphql.ObjectConfig{
		Name: "Mutation",
		Fields: graphql.Fields{
			"createOrder": &graphql.Field{
				Type: graphql.NewNonNull(orderType),
				Args: graphql.FieldConfigArgument{"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)}},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					input, ok := p.Args["input"].(map[string]any)
					if !ok {
						return nil, errors.New("invalid create order input")
					}
					customerID, err := parseID(fmt.Sprint(input["customerId"]))
					if err != nil {
						return nil, errors.New("invalid customer ID")
					}
					productID, err := parseID(fmt.Sprint(input["productId"]))
					if err != nil {
						return nil, errors.New("invalid product ID")
					}
					request := CreateInput{CustomerID: customerID, ProductID: productID, Quantity: integerArg(input["quantity"], 0)}
					return h.runGraphQL(p.Context, func(ctx context.Context) (any, error) { return h.service.Create(ctx, request) })
				},
			},
			"updateOrderStatus": &graphql.Field{
				Type: graphql.NewNonNull(orderType),
				Args: graphql.FieldConfigArgument{
					"id":     &idArg,
					"status": &graphql.ArgumentConfig{Type: graphql.NewNonNull(statusType)},
				},
				Resolve: func(p graphql.ResolveParams) (any, error) {
					principal, ok := auth.FromContext(p.Context)
					if !ok || principal.Role != auth.RoleAdmin {
						return nil, errors.New("administrator role required")
					}
					id, err := parseID(fmt.Sprint(p.Args["id"]))
					if err != nil {
						return nil, errors.New("invalid order ID")
					}
					status := Status(fmt.Sprint(p.Args["status"]))
					return h.runGraphQL(p.Context, func(ctx context.Context) (any, error) {
						return h.service.UpdateStatus(ctx, id, status)
					})
				},
			},
		},
	})
	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: query, Mutation: mutation})
	if err != nil {
		panic(fmt.Sprintf("create GraphQL schema: %v", err))
	}
	return schema
}

func (h *Handler) runGraphQL(ctx context.Context, action func(context.Context) (any, error)) (any, error) {
	result, err := action(ctx)
	if err != nil {
		h.logServiceError(ctx, err)
		switch {
		case errors.Is(err, ErrOutOfStock):
			return nil, errors.New("product out of stock")
		case errors.Is(err, ErrProductMissing):
			return nil, errors.New("product not found")
		case errors.Is(err, ErrNotFound):
			return nil, errors.New("order not found")
		case errors.Is(err, ErrInvalid):
			return nil, errors.New("invalid order input")
		case errors.Is(err, ErrDependency):
			return nil, errors.New("product service unavailable")
		default:
			return nil, errors.New("internal server error")
		}
	}
	return result, nil
}

func (h *Handler) respondError(w http.ResponseWriter, r *http.Request, err error) {
	h.logServiceError(r.Context(), err)
	switch {
	case errors.Is(err, ErrProductMissing):
		writeError(w, r, http.StatusNotFound, "product_not_found", "Product not found")
	case errors.Is(err, ErrOutOfStock):
		writeError(w, r, http.StatusConflict, "insufficient_stock", "Insufficient product stock")
	case errors.Is(err, ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", "Order not found")
	case errors.Is(err, ErrInvalid):
		writeError(w, r, http.StatusBadRequest, "invalid_order", "Order input is invalid")
	case errors.Is(err, ErrDependency):
		writeError(w, r, http.StatusServiceUnavailable, "product_service_unavailable", "Product service unavailable")
	default:
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Internal server error")
	}
}

func (h *Handler) logServiceError(ctx context.Context, err error) {
	if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) &&
		!errors.Is(err, ErrProductMissing) && !errors.Is(err, ErrOutOfStock) {
		if !errors.Is(err, ErrDependency) {
			h.metrics.DatabaseError()
		}
		h.logger.Error("order operation failed", "request_id", observability.RequestID(ctx), "error", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"code": code, "message": message, "requestId": observability.RequestID(r.Context()),
	}})
}

func parseID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

func queryInt(r *http.Request, name string, fallback int) (int, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	return strconv.Atoi(value)
}

func integerArg(value any, fallback int) int {
	switch number := value.(type) {
	case int:
		return number
	case int32:
		return int(number)
	case int64:
		return int(number)
	case float64:
		return int(number)
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	default:
		return fallback
	}
}

func timeField(get func(Order) time.Time) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (any, error) {
		switch order := p.Source.(type) {
		case *Order:
			return get(*order).UTC().Format(time.RFC3339), nil
		case Order:
			return get(order).UTC().Format(time.RFC3339), nil
		}
		return nil, errors.New("invalid order")
	}
}
