package submissions

import "fmt"

// Error is a rule the request broke. Code is stable and machine-readable;
// the HTTP layer maps it to a status.
type Error struct {
	Code    string
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Error codes returned to clients.
const (
	CodeNotFound            = "not_found"
	CodeInvalidInput        = "invalid_input"
	CodeIdempotencyMismatch = "idempotency_mismatch"
	CodeFileTooLarge        = "file_too_large"
	CodeDeadlinePassed      = "deadline_passed"
	CodeGraceOver           = "grace_over"
	CodeUploadsDisabled     = "uploads_disabled"
	CodeVersionLimit        = "version_limit"
	CodeStorageFull         = "storage_full"
	CodeWrongState          = "wrong_state"
	CodePartsMissing        = "parts_missing"
	CodeUploadMismatch      = "upload_mismatch"
	CodeStorageUnavailable  = "storage_unavailable"
)

func errf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

var errNotFound = errf(CodeNotFound, "not found")
