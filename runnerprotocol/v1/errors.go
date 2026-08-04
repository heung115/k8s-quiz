package v1

import (
	"errors"
	"fmt"
	"net/http"
)

type ErrorCode string

const (
	CodeInvalidRequest      ErrorCode = "INVALID_REQUEST"
	CodeUnauthorized        ErrorCode = "UNAUTHORIZED"
	CodeControllerFenced    ErrorCode = "CONTROLLER_FENCED"
	CodeIdempotencyConflict ErrorCode = "IDEMPOTENCY_CONFLICT"
	CodeGenerationStale     ErrorCode = "GENERATION_STALE"
	CodeAllocationNotFound  ErrorCode = "ALLOCATION_NOT_FOUND"
	CodeProviderUnavailable ErrorCode = "PROVIDER_UNAVAILABLE"
	CodeCleanupRequired     ErrorCode = "CLEANUP_REQUIRED"
	CodeDeadlineExceeded    ErrorCode = "DEADLINE_EXCEEDED"
	CodeOutcomeUnknown      ErrorCode = "OUTCOME_UNKNOWN"
	CodeUnsupported         ErrorCode = "UNSUPPORTED"
	CodeInternal            ErrorCode = "INTERNAL"
)

var (
	ErrInvalidRequest      = errors.New("private Runner request is invalid")
	ErrUnauthorized        = errors.New("private Runner caller is unauthorized")
	ErrControllerFenced    = errors.New("private Runner controller is fenced")
	ErrIdempotencyConflict = errors.New("private Runner idempotency conflict")
	ErrGenerationStale     = errors.New("private Runner generation is stale")
	ErrAllocationNotFound  = errors.New("private Runner allocation was not found")
	ErrProviderUnavailable = errors.New("private Runner provider is unavailable")
	ErrCleanupRequired     = errors.New("private Runner cleanup is required")
	ErrDeadlineExceeded    = errors.New("private Runner deadline was exceeded")
	ErrOutcomeUnknown      = errors.New("private Runner operation outcome is unknown")
	ErrUnsupported         = errors.New("private Runner operation is unsupported")
	ErrProtocolViolation   = errors.New("private Runner protocol violation")
)

type RemoteError struct {
	Code      ErrorCode
	Retryable bool
	Message   string
}

func (e *RemoteError) Error() string { return fmt.Sprintf("private Runner error %s", e.Code) }
func (e *RemoteError) Unwrap() error { return SentinelForCode(e.Code) }
func (e *RemoteError) PublicMessage() string {
	return e.Message
}
func (e *RemoteError) String() string { return fmt.Sprintf("%s retryable=%t", e.Code, e.Retryable) }

// ErrorContractEntry is the exact, stable HTTP representation for one wire
// error. Both client validation and the private server encoder use this table.
type ErrorContractEntry struct {
	Status    int
	Retryable bool
	Message   string
}

func ErrorContract(code ErrorCode) (ErrorContractEntry, bool) {
	switch code {
	case CodeInvalidRequest:
		return ErrorContractEntry{http.StatusBadRequest, false, "The request is invalid."}, true
	case CodeUnauthorized:
		return ErrorContractEntry{http.StatusUnauthorized, false, "The caller is not authorized."}, true
	case CodeControllerFenced:
		return ErrorContractEntry{http.StatusConflict, false, "The controller epoch is not authoritative."}, true
	case CodeIdempotencyConflict:
		return ErrorContractEntry{http.StatusConflict, false, "The operation key is bound to another request."}, true
	case CodeGenerationStale:
		return ErrorContractEntry{http.StatusConflict, false, "The allocation generation conflicts with current state."}, true
	case CodeAllocationNotFound:
		return ErrorContractEntry{http.StatusNotFound, false, "The allocation was not found."}, true
	case CodeProviderUnavailable:
		return ErrorContractEntry{http.StatusServiceUnavailable, true, "The provider could not complete the request."}, true
	case CodeCleanupRequired:
		return ErrorContractEntry{http.StatusConflict, true, "Exact cleanup remains required."}, true
	case CodeDeadlineExceeded:
		return ErrorContractEntry{http.StatusGatewayTimeout, true, "The request deadline was exceeded."}, true
	case CodeOutcomeUnknown:
		return ErrorContractEntry{http.StatusGatewayTimeout, true, "The operation outcome is unknown; retry the exact request."}, true
	case CodeUnsupported:
		return ErrorContractEntry{http.StatusNotImplemented, false, "The operation is not supported."}, true
	case CodeInternal:
		return ErrorContractEntry{http.StatusInternalServerError, false, "The Runner could not complete the request."}, true
	default:
		return ErrorContractEntry{}, false
	}
}

func SentinelForCode(code ErrorCode) error {
	switch code {
	case CodeInvalidRequest:
		return ErrInvalidRequest
	case CodeUnauthorized:
		return ErrUnauthorized
	case CodeControllerFenced:
		return ErrControllerFenced
	case CodeIdempotencyConflict:
		return ErrIdempotencyConflict
	case CodeGenerationStale:
		return ErrGenerationStale
	case CodeAllocationNotFound:
		return ErrAllocationNotFound
	case CodeProviderUnavailable:
		return ErrProviderUnavailable
	case CodeCleanupRequired:
		return ErrCleanupRequired
	case CodeDeadlineExceeded:
		return ErrDeadlineExceeded
	case CodeOutcomeUnknown:
		return ErrOutcomeUnknown
	case CodeUnsupported:
		return ErrUnsupported
	default:
		return ErrProtocolViolation
	}
}
