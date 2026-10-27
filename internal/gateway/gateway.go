package gateway

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"oms/internal/auth"
	"oms/internal/config"
	"oms/internal/observability"

	"github.com/gin-gonic/gin"
)

func New(cfg config.Config, logger *slog.Logger) http.Handler {
	target, err := url.Parse(cfg.OrderServiceURL)
	if err != nil || target.Scheme != "http" || target.Host == "" {
		panic("ORDER_SERVICE_URL must be an absolute HTTP URL")
	}
	httpClient := &http.Client{
		Timeout: 28 * time.Second,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 28 * time.Second,
		},
	}
	forwarder := newForwarder(target, httpClient, logger)
	var productForwarder http.Handler
	if cfg.ProductServiceURL != "" {
		productTarget, err := url.Parse(cfg.ProductServiceURL)
		if err != nil || productTarget.Scheme != "http" || productTarget.Host == "" {
			panic("PRODUCT_SERVICE_URL must be an absolute HTTP URL")
		}
		productForwarder = newForwarder(productTarget, httpClient, logger)
	}
	authenticated := auth.Authenticate(cfg.JWTSecret)
	metrics := observability.NewMetrics("oms_gateway")
	router := gin.New()
	router.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) {
		logger.Error("gateway handler panic", "request_id", observability.RequestID(c.Request.Context()), "method", c.Request.Method, "path", c.Request.URL.Path)
		writeJSON(c.Writer, http.StatusInternalServerError, `{"error":{"code":"internal_error","message":"Internal server error"}}`)
		c.Abort()
	}))
	router.GET("/health/live", func(c *gin.Context) {
		writeJSON(c.Writer, http.StatusOK, `{"status":"UP"}`)
	})
	router.GET("/health/ready", func(c *gin.Context) {
		r := c.Request
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.OrderServiceURL, "/")+"/health/ready", nil)
		if err != nil {
			writeJSON(c.Writer, http.StatusServiceUnavailable, `{"status":"NOT_READY"}`)
			return
		}
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			writeJSON(c.Writer, http.StatusServiceUnavailable, `{"status":"NOT_READY"}`)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			writeJSON(c.Writer, http.StatusServiceUnavailable, `{"status":"NOT_READY"}`)
			return
		}
		writeJSON(c.Writer, http.StatusOK, `{"status":"READY"}`)
	})
	router.GET("/metrics", gin.WrapH(metrics.Handler()))
	if accounts := loginAccounts(cfg); len(accounts) > 0 {
		router.POST("/auth/login", gin.WrapH(auth.LoginHandler(cfg.JWTSecret, cfg.JWTTTL, accounts)))
	}
	forward := gin.WrapH(authenticated(forwarder))
	forwardAdmin := gin.WrapH(authenticated(auth.RequireRole(auth.RoleAdmin)(forwarder)))
	api := router.Group("/api")
	v1 := api.Group("/v1")
	orderRoutes := v1.Group("/orders")
	orderRoutes.POST("", forward)
	orderRoutes.GET("", forward)
	orderRoutes.GET("/:id", forward)
	orderRoutes.PUT("/:id/status", forwardAdmin)
	if productForwarder != nil {
		product := gin.WrapH(authenticated(productForwarder))
		productAdmin := gin.WrapH(authenticated(auth.RequireRole(auth.RoleAdmin)(productForwarder)))
		productRoutes := v1.Group("/products")
		productRoutes.GET("", product)
		productRoutes.GET("/:id", product)
		productRoutes.POST("", productAdmin)
		productRoutes.DELETE("/:id", productAdmin)
	}
	router.POST("/graphql", forward)
	return metrics.Middleware(logger, secureHeaders(cfg.CORSOrigins, router))
}

// loginAccounts builds the configured test/demo accounts. A role with no
// password configured has no account, so login is off by default.
func loginAccounts(cfg config.Config) []auth.Account {
	var accounts []auth.Account
	if cfg.AdminPassword != "" {
		accounts = append(accounts, auth.Account{Username: cfg.AdminUsername, Password: cfg.AdminPassword, Role: auth.RoleAdmin})
	}
	if cfg.UserPassword != "" {
		accounts = append(accounts, auth.Account{Username: cfg.UserUsername, Password: cfg.UserPassword, Role: auth.RoleCustomer})
	}
	return accounts
}

func newForwarder(target *url.URL, client *http.Client, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamURL := *target
		upstreamURL.Path = strings.TrimRight(target.Path, "/") + r.URL.Path
		upstreamURL.RawPath = ""
		upstreamURL.RawQuery = r.URL.RawQuery
		request, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), r.Body)
		if err != nil {
			logger.Error("create upstream request", "request_id", observability.RequestID(r.Context()), "error", err)
			writeJSON(w, http.StatusInternalServerError, `{"error":{"code":"internal_error","message":"Internal server error"}}`)
			return
		}
		request.ContentLength = r.ContentLength
		request.Header = r.Header.Clone()
		removeHopByHopHeaders(request.Header)
		response, err := client.Do(request)
		if err != nil {
			logger.Error("upstream request failed", "request_id", observability.RequestID(r.Context()), "error", err)
			writeJSON(w, http.StatusBadGateway, `{"error":{"code":"service_unavailable","message":"Order service unavailable"}}`)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			if strings.EqualFold(name, "Connection") || strings.EqualFold(name, "Keep-Alive") ||
				strings.EqualFold(name, "Proxy-Authenticate") || strings.EqualFold(name, "Proxy-Authorization") ||
				strings.EqualFold(name, "TE") || strings.EqualFold(name, "Trailer") ||
				strings.EqualFold(name, "Transfer-Encoding") || strings.EqualFold(name, "Upgrade") {
				continue
			}
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		if _, err := io.Copy(w, response.Body); err != nil {
			logger.Error("copy upstream response", "request_id", observability.RequestID(r.Context()), "error", err)
		}
	})
}

func removeHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"TE", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(name)
	}
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// secureHeaders adds basic security headers and, only for explicitly allowed
// origins, CORS headers. Preflight requests are answered before authentication.
func secureHeaders(allowedOrigins []string, next http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		allowed[origin] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if origin := r.Header.Get("Origin"); origin != "" && allowed[origin] {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
