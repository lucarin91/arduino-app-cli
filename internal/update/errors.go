package update

import "errors"

type ErrorCode string

// TODO: add the error to the openAPI spec as an enum
const (
	NoInternetConnection ErrorCode = "NO_INTERNET_CONNECTION"
	OperationInProgress  ErrorCode = "OPERATION_IN_PROGRESS"
	UnknownError         ErrorCode = "UNKNOWN_ERROR"
)

var (
	ErrOperationAlreadyInProgress = &UpdateError{
		Code:    OperationInProgress,
		Details: "an operation is already in progress",
	}
	ErrNoInternetConnection = &UpdateError{
		Code:    NoInternetConnection,
		Details: "no internet connection available",
	}
)

type UpdateError struct {
	Code    ErrorCode `json:"code"`
	Details string    `json:"details"`

	err error
}

func (e *UpdateError) Error() string {
	return e.Details
}

func (e *UpdateError) Unwrap() error {
	return e.err
}

func NewUnkownError(err error) *UpdateError {
	return &UpdateError{
		Details: err.Error(),
		err:     err,
	}
}

func GetUpdateErrorCode(err error) ErrorCode {
	var updateError *UpdateError
	if errors.As(err, &updateError) {
		if updateError.Code != "" {
			return updateError.Code
		}
	}
	return UnknownError
}
