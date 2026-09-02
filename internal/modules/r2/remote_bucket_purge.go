package r2

import (
	"context"
	"fmt"
	"strings"
)

type RemoteMultipart struct {
	Key      string
	UploadID string
}

type RemoteMultipartPage struct {
	Uploads            []RemoteMultipart
	NextKeyMarker      string
	NextUploadIDMarker string
	Truncated          bool
}

type RemoteObjectDeleteFailure struct {
	Key     string
	Code    string
	Message string
}

type RemoteBatchDeleteError struct {
	Failures []RemoteObjectDeleteFailure
}

func (e *RemoteBatchDeleteError) Error() string {
	details := make([]string, 0, len(e.Failures))
	for _, failure := range e.Failures {
		details = append(details, fmt.Sprintf("%q (%s)", failure.Key, failure.Code))
	}
	return "remote object batch delete failed: " + strings.Join(details, ", ")
}

// BucketClearBackend is deliberately separate from Backend so ordinary object
// operations and their test doubles do not need destructive bucket APIs.
type BucketClearBackend interface {
	ListRemote(context.Context, Target, string, string, int32) (RemoteObjectList, error)
	DeleteRemoteBatch(context.Context, Target, []string) (int, error)
	ListRemoteMultipart(context.Context, Target, string, string, int32) (RemoteMultipartPage, error)
	AbortMultipart(context.Context, Target, string, string) error
}
