package analyzer

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Error type constants matching Python error classification.
const (
	ErrorTypeStateNotFound    = "STATE_NOT_FOUND"
	ErrorTypePermissionDenied = "PERMISSION_DENIED"
	ErrorTypeUnauthorized     = "UNAUTHORIZED"
	ErrorTypeTimeout          = "TIMEOUT"
	ErrorTypeException        = "EXCEPTION"
	ErrorTypeUnknown          = "UNKNOWN"
)

// AnalysisError wraps an error with classification.
type AnalysisError struct {
	ErrorType string
	Message   string
	Err       error
}

// Error implements the error interface.
func (e *AnalysisError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("[%s] %s: %v", e.ErrorType, e.Message, e.Err)
	}
	return fmt.Sprintf("[%s] %s", e.ErrorType, e.Message)
}

// Unwrap returns the underlying error.
func (e *AnalysisError) Unwrap() error {
	return e.Err
}

// NewAnalysisError creates a new AnalysisError with the given type, message, and cause.
func NewAnalysisError(errorType, message string, err error) *AnalysisError {
	return &AnalysisError{
		ErrorType: errorType,
		Message:   message,
		Err:       err,
	}
}

// httpStatusError is an interface for errors that carry an HTTP status code.
type httpStatusError interface {
	StatusCode() int
}

// ClassifyError classifies an error based on HTTP status or message content.
func ClassifyError(err error) *AnalysisError {
	if err == nil {
		return nil
	}

	// Check for context deadline exceeded (timeout).
	if errors.Is(err, context.DeadlineExceeded) {
		return &AnalysisError{
			ErrorType: ErrorTypeTimeout,
			Message:   "operation timed out",
			Err:       err,
		}
	}

	// Check for context cancellation.
	if errors.Is(err, context.Canceled) {
		return &AnalysisError{
			ErrorType: ErrorTypeTimeout,
			Message:   "operation was canceled",
			Err:       err,
		}
	}

	// Check if the error carries an HTTP status code.
	var httpErr httpStatusError
	if errors.As(err, &httpErr) {
		return classifyHTTPStatus(httpErr.StatusCode(), err)
	}

	// Fall back to message-based classification.
	msg := strings.ToLower(err.Error())

	if strings.Contains(msg, "404") || strings.Contains(msg, "not found") || strings.Contains(msg, "state not found") {
		return &AnalysisError{
			ErrorType: ErrorTypeStateNotFound,
			Message:   "state file not found",
			Err:       err,
		}
	}

	if strings.Contains(msg, "403") || strings.Contains(msg, "forbidden") || strings.Contains(msg, "permission denied") {
		return &AnalysisError{
			ErrorType: ErrorTypePermissionDenied,
			Message:   "permission denied",
			Err:       err,
		}
	}

	if strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "authentication") {
		return &AnalysisError{
			ErrorType: ErrorTypeUnauthorized,
			Message:   "unauthorized",
			Err:       err,
		}
	}

	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return &AnalysisError{
			ErrorType: ErrorTypeTimeout,
			Message:   "operation timed out",
			Err:       err,
		}
	}

	return &AnalysisError{
		ErrorType: ErrorTypeUnknown,
		Message:   err.Error(),
		Err:       err,
	}
}

// classifyHTTPStatus maps an HTTP status code to an AnalysisError.
func classifyHTTPStatus(statusCode int, err error) *AnalysisError {
	switch statusCode {
	case 404:
		return &AnalysisError{
			ErrorType: ErrorTypeStateNotFound,
			Message:   "state file not found (HTTP 404)",
			Err:       err,
		}
	case 403:
		return &AnalysisError{
			ErrorType: ErrorTypePermissionDenied,
			Message:   "permission denied (HTTP 403)",
			Err:       err,
		}
	case 401:
		return &AnalysisError{
			ErrorType: ErrorTypeUnauthorized,
			Message:   "unauthorized (HTTP 401)",
			Err:       err,
		}
	case 408, 504:
		return &AnalysisError{
			ErrorType: ErrorTypeTimeout,
			Message:   fmt.Sprintf("request timeout (HTTP %d)", statusCode),
			Err:       err,
		}
	default:
		if statusCode >= 500 {
			return &AnalysisError{
				ErrorType: ErrorTypeException,
				Message:   fmt.Sprintf("server error (HTTP %d)", statusCode),
				Err:       err,
			}
		}
		return &AnalysisError{
			ErrorType: ErrorTypeUnknown,
			Message:   fmt.Sprintf("unexpected HTTP status %d", statusCode),
			Err:       err,
		}
	}
}
