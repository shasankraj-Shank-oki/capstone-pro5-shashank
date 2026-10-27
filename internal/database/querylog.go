package database

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"oms/internal/observability"
)

// queryLogger sends GORM query timings to slog with the request ID, so one
// request can be followed handler -> service -> repository -> PostgreSQL.
// SQL text and bound values are deliberately not logged.
type queryLogger struct{ log *slog.Logger }

func newQueryLogger(log *slog.Logger) logger.Interface { return queryLogger{log: log} }

func (q queryLogger) LogMode(logger.LogLevel) logger.Interface    { return q }
func (queryLogger) Info(context.Context, string, ...interface{})  {}
func (queryLogger) Warn(context.Context, string, ...interface{})  {}
func (queryLogger) Error(context.Context, string, ...interface{}) {}

func (q queryLogger) Trace(ctx context.Context, begin time.Time, _ func() (string, int64), err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return
	}
	attrs := []any{"request_id", observability.RequestID(ctx), "duration", time.Since(begin).String()}
	if err != nil {
		q.log.Error("database query failed", append(attrs, "error", err)...)
		return
	}
	q.log.Debug("database query", attrs...)
}
