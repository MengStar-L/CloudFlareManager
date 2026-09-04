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

// ErrR2CredentialsRequired means the selected Cloudflare account cannot
// access its mapped objects until R2 S3 credentials are configured again.
var ErrR2CredentialsRequired = errors.New("account does not have R2 S3 credentials")

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
