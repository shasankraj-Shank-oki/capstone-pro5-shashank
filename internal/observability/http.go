package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type requestIDKey struct{}

func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func NewLogger(level string) *slog.Logger {
	var logLevel slog.Level
	if err := logLevel.UnmarshalText([]byte(level)); err != nil {
		logLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
}

type Metrics struct {
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
	errors   *prometheus.CounterVec
	dbErrors prometheus.Counter
	registry *prometheus.Registry
}

func NewMetrics(namespace string) *Metrics {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "http_requests_total", Help: "HTTP requests."}, []string{"method", "route", "status"}),
		latency:  prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: "http_request_duration_seconds", Help: "HTTP request duration.", Buckets: prometheus.DefBuckets}, []string{"method", "route"}),
		errors:   prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "http_errors_total", Help: "HTTP error responses."}, []string{"method", "route", "status"}),
		dbErrors: prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: "database_errors_total", Help: "Database operation errors."}),
		registry: prometheus.NewRegistry(),
	}
	m.registry.MustRegister(m.requests, m.latency, m.errors, m.dbErrors)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) DatabaseError() {
	m.dbErrors.Inc()
}

func (m *Metrics) Middleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			var bytes [16]byte
			if _, err := rand.Read(bytes[:]); err != nil {
				logger.Error("generate request id", "error", err)
				http.Error(w, `{"error":{"code":"internal_error","message":"Internal server error"}}`, http.StatusInternalServerError)
				return
			}
			id = hex.EncodeToString(bytes[:])
		}
		w.Header().Set("X-Request-ID", id)
		r.Header.Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		r = r.WithContext(ctx)
		recorder := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(recorder, r)
		duration := time.Since(start)
		route := routeName(r.URL.Path)
		status := strconv.Itoa(recorder.status)
		m.requests.WithLabelValues(r.Method, route, status).Inc()
		m.latency.WithLabelValues(r.Method, route).Observe(duration.Seconds())
		if recorder.status >= 400 {
			m.errors.WithLabelValues(r.Method, route, status).Inc()
		}
		attributes := []any{"request_id", id, "method", r.Method, "path", r.URL.Path, "status", recorder.status, "duration", duration.String()}
		if recorder.status >= 400 {
			attributes = append(attributes, "error", "request failed")
		}
		logger.Info("http request", attributes...)
	})
}

func validRequestID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("-_.", char)) {
			return false
		}
	}
	return true
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(body []byte) (int, error) {
	return w.ResponseWriter.Write(body)
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusWriter) CloseNotify() <-chan bool {
	if notifier, ok := w.ResponseWriter.(http.CloseNotifier); ok {
		return notifier.CloseNotify()
	}
	return make(chan bool)
}

func routeName(path string) string {
	switch {
	case path == "/health/live":
		return "/health/live"
	case path == "/health/ready":
		return "/health/ready"
	case path == "/metrics":
		return "/metrics"
	case path == "/auth/login":
		return "/auth/login"
	case path == "/graphql":
		return "/graphql"
	case path == "/api/v1/orders":
		return "/api/v1/orders"
	case path == "/api/v1/products":
		return "/api/v1/products"
	case strings.HasPrefix(path, "/api/v1/orders/"):
		if strings.HasSuffix(path, "/status") {
			return "/api/v1/orders/{id}/status"
		}
		return "/api/v1/orders/{id}"
	case strings.HasPrefix(path, "/api/v1/products/"):
		return "/api/v1/products/{id}"
	default:
		return "unmatched"
	}
}
