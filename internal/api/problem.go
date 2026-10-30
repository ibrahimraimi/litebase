package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ibrahimraimi/litebase/internal/auth"
	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/migrate"
)

const (
	CodeSQLError        = "sql_error"
	CodeBusy            = "busy"
	CodeQueueFull       = "queue_full"
	CodeTimeout         = "timeout"
	CodeUnauthorized    = "unauthorized"
	CodeForbidden       = "forbidden"
	CodeNotFound        = "not_found"
	CodeInvalidRequest  = "invalid_request"
	CodeConflict        = "conflict"
	CodeRateLimited     = "rate_limited"
	CodePayloadTooLarge = "payload_too_large"
	CodeInternalError   = "internal_error"
)

type ProblemDetail struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func writeProblem(w http.ResponseWriter, status int, code string, detail string) {
	if status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	problem := ProblemDetail{
		Type:   "https://litebase.dev/problems/" + code,
		Title:  http.StatusText(status),
		Status: status,
		Code:   code,
		Detail: detail,
	}
	_ = json.NewEncoder(w).Encode(problem)
}

func writeMappedError(w http.ResponseWriter, err error) {
	status, code, detail := classifyError(err)
	writeProblem(w, status, code, detail)
}

func classifyError(err error) (int, string, string) {
	var maxBytesErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesErr):
		return http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "request body exceeds maximum allowed size"
	case errors.Is(err, engine.ErrInvalidIdentifier),
		errors.Is(err, engine.ErrEmptyStatement),
		errors.Is(err, engine.ErrMultiStatementNotAllowed),
		errors.Is(err, engine.ErrInteractiveTxNotSupported),
		errors.Is(err, migrate.ErrDropTableNotAllowed),
		errors.Is(err, migrate.ErrManualMigrationRequired),
		errors.Is(err, auth.ErrInvalidScope):
		return http.StatusBadRequest, CodeInvalidRequest, err.Error()
	case errors.Is(err, migrate.ErrChecksumMismatch),
		errors.Is(err, migrate.ErrDeployInProgress),
		errors.Is(err, migrate.ErrMergeConflict),
		errors.Is(err, control.ErrAlreadyExists):
		return http.StatusConflict, CodeConflict, err.Error()
	case errors.Is(err, control.ErrNotFound):
		return http.StatusNotFound, CodeNotFound, err.Error()
	case errors.Is(err, auth.ErrMissingToken),
		errors.Is(err, auth.ErrMalformedToken),
		errors.Is(err, auth.ErrTokenExpired):
		return http.StatusUnauthorized, CodeUnauthorized, err.Error()
	case errors.Is(err, auth.ErrForbiddenScope),
		errors.Is(err, engine.ErrAuthorizationDenied),
		errors.Is(err, engine.ErrReadOnlyViolation):
		return http.StatusForbidden, CodeForbidden, err.Error()
	case errors.Is(err, engine.ErrQueueFull):
		return http.StatusServiceUnavailable, CodeQueueFull, err.Error()
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return http.StatusGatewayTimeout, CodeTimeout, "query execution timed out"
	default:
		return classifySQLOrGenericError(err)
	}
}

func classifySQLOrGenericError(err error) (int, string, string) {
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "busy") || strings.Contains(lower, "database is locked") {
		return http.StatusServiceUnavailable, CodeBusy, "database is busy"
	}
	return http.StatusBadRequest, CodeSQLError, sanitizeErrorDetail(err.Error())
}

func sanitizeErrorDetail(message string) string {
	if strings.Contains(message, "/") {
		return "SQL execution failed due to invalid syntax, constraint violation, or resource limit"
	}
	return message
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
