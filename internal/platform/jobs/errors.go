package jobs

import "errors"

type HandlerError struct {
	Code      string
	Permanent bool
	Err       error
}

func (e *HandlerError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Code != "" {
		return e.Code
	}
	return "job handler failed"
}

func (e *HandlerError) Unwrap() error {
	return e.Err
}

func NewFailure(code string, err error, permanent bool) error {
	return &HandlerError{Code: code, Err: err, Permanent: permanent}
}

func classifyFailure(err error) (code string, permanent bool) {
	var failure *HandlerError
	if errors.As(err, &failure) {
		return failure.Code, failure.Permanent
	}
	return "", false
}
