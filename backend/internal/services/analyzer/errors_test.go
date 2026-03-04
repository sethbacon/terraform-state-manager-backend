package analyzer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// httpErrStub implements the unexported httpStatusError interface for testing.
type httpErrStub struct {
	code int
	msg  string
}

func (e *httpErrStub) StatusCode() int { return e.code }
func (e *httpErrStub) Error() string   { return e.msg }

func TestAnalysisError_Error_WithCause(t *testing.T) {
	cause := errors.New("root cause")
	ae := &AnalysisError{
		ErrorType: ErrorTypeException,
		Message:   "something broke",
		Err:       cause,
	}
	assert.Equal(t, "[EXCEPTION] something broke: root cause", ae.Error())
}

func TestAnalysisError_Error_WithoutCause(t *testing.T) {
	ae := &AnalysisError{
		ErrorType: ErrorTypeStateNotFound,
		Message:   "state missing",
	}
	assert.Equal(t, "[STATE_NOT_FOUND] state missing", ae.Error())
}

func TestAnalysisError_Unwrap(t *testing.T) {
	cause := errors.New("inner")
	ae := &AnalysisError{Err: cause}
	assert.Equal(t, cause, ae.Unwrap())
}

func TestAnalysisError_Unwrap_NilCause(t *testing.T) {
	ae := &AnalysisError{}
	assert.Nil(t, ae.Unwrap())
}

func TestNewAnalysisError(t *testing.T) {
	cause := errors.New("io")
	ae := NewAnalysisError(ErrorTypeException, "disk full", cause)
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeException, ae.ErrorType)
	assert.Equal(t, "disk full", ae.Message)
	assert.Equal(t, cause, ae.Err)
}

func TestClassifyError_Nil(t *testing.T) {
	assert.Nil(t, ClassifyError(nil))
}

func TestClassifyError_DeadlineExceeded(t *testing.T) {
	ae := ClassifyError(context.DeadlineExceeded)
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeTimeout, ae.ErrorType)
}

func TestClassifyError_Canceled(t *testing.T) {
	ae := ClassifyError(context.Canceled)
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeTimeout, ae.ErrorType)
}

func TestClassifyError_HTTPStatus_404(t *testing.T) {
	ae := ClassifyError(&httpErrStub{code: 404, msg: "not found"})
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeStateNotFound, ae.ErrorType)
}

func TestClassifyError_HTTPStatus_403(t *testing.T) {
	ae := ClassifyError(&httpErrStub{code: 403, msg: "forbidden"})
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypePermissionDenied, ae.ErrorType)
}

func TestClassifyError_HTTPStatus_401(t *testing.T) {
	ae := ClassifyError(&httpErrStub{code: 401, msg: "unauthorized"})
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeUnauthorized, ae.ErrorType)
}

func TestClassifyError_HTTPStatus_408(t *testing.T) {
	ae := ClassifyError(&httpErrStub{code: 408, msg: "timeout"})
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeTimeout, ae.ErrorType)
}

func TestClassifyError_HTTPStatus_504(t *testing.T) {
	ae := ClassifyError(&httpErrStub{code: 504, msg: "gateway timeout"})
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeTimeout, ae.ErrorType)
}

func TestClassifyError_HTTPStatus_500(t *testing.T) {
	ae := ClassifyError(&httpErrStub{code: 500, msg: "internal server error"})
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeException, ae.ErrorType)
}

func TestClassifyError_HTTPStatus_Unknown(t *testing.T) {
	ae := ClassifyError(&httpErrStub{code: 418, msg: "I'm a teapot"})
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeUnknown, ae.ErrorType)
}

func TestClassifyError_MessageBased_NotFound(t *testing.T) {
	ae := ClassifyError(fmt.Errorf("resource not found in backend"))
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeStateNotFound, ae.ErrorType)
}

func TestClassifyError_MessageBased_404(t *testing.T) {
	ae := ClassifyError(fmt.Errorf("received 404 from API"))
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeStateNotFound, ae.ErrorType)
}

func TestClassifyError_MessageBased_Forbidden(t *testing.T) {
	ae := ClassifyError(fmt.Errorf("403 forbidden"))
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypePermissionDenied, ae.ErrorType)
}

func TestClassifyError_MessageBased_Unauthorized(t *testing.T) {
	ae := ClassifyError(fmt.Errorf("authentication failed: 401 unauthorized"))
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeUnauthorized, ae.ErrorType)
}

func TestClassifyError_MessageBased_Timeout(t *testing.T) {
	ae := ClassifyError(fmt.Errorf("request timeout after 30s"))
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeTimeout, ae.ErrorType)
}

func TestClassifyError_MessageBased_Unknown(t *testing.T) {
	ae := ClassifyError(fmt.Errorf("unexpected error occurred"))
	require.NotNil(t, ae)
	assert.Equal(t, ErrorTypeUnknown, ae.ErrorType)
}
