package r2

import (
	"errors"
	"net/http"
)

// ErrRangeNotSatisfiable reports that a requested byte range cannot be
// satisfied for the current representation.
var ErrRangeNotSatisfiable = errors.New("requested range not satisfiable")

// ErrWriteRecoveryAmbiguous means the remote object cannot be proven to be
// either the previous committed version or the result of the pending write.
var ErrWriteRecoveryAmbiguous = errors.New("remote write state is ambiguous")

func isMultipartNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiError interface{ ErrorCode() string }
	if errors.As(err, &apiError) && apiError.ErrorCode() == "NoSuchUpload" {
		return true
	}
	var statusError interface{ HTTPStatusCode() int }
	return errors.As(err, &statusError) && statusError.HTTPStatusCode() == http.StatusNotFound
}
