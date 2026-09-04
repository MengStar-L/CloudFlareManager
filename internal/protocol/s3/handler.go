package s3protocol

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
	"github.com/google/uuid"
)

type ObjectService interface {
	Put(context.Context, r2.PutRequest) (r2.Object, error)
	Get(context.Context, string, r2.GetOptions) (r2.GetResult, error)
	Stat(context.Context, string) (r2.Object, error)
	List(context.Context, r2.ListOptions) (r2.ObjectList, error)
	Delete(context.Context, string) error
	Copy(context.Context, string, string) (r2.Object, error)
}

type Handler struct {
	Bucket  string
	Auth    Authenticator
	Objects ObjectService
}

func (h Handler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	requestID := uuid.NewString()
	w.Header().Set("x-amz-request-id", requestID)
	identity, err := h.Auth.Authenticate(request)
	if err != nil {
		h.writeAuthError(w, request, requestID, err)
		return
	}
	requiredScope := "r2:read"
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		requiredScope = "r2:write"
	}
	if !identity.HasScope(requiredScope) {
		writeXMLError(w, request, requestID, http.StatusForbidden, "AccessDenied", "Access Denied")
		return
	}
	bucket, key := splitPath(request.URL.Path)
	if bucket == "" {
		if request.Method == http.MethodGet {
			h.listBuckets(w, requestID)
			return
		}
		writeXMLError(w, request, requestID, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed")
		return
	}
	if bucket != h.Bucket {
		writeXMLError(w, request, requestID, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
		return
	}
	if key != "" && r2.IsWebDAVInternalKey(key) {
		writeXMLError(w, request, requestID, http.StatusForbidden, "AccessDenied", "Access Denied")
		return
	}
	if key == "" {
		h.handleBucket(w, request, requestID)
		return
	}
	h.handleObject(w, request, requestID, key)
}

func (h Handler) handleBucket(w http.ResponseWriter, request *http.Request, requestID string) {
	query := request.URL.Query()
	if _, ok := query["uploads"]; ok && request.Method == http.MethodGet {
		h.listMultipartUploads(w, request, requestID)
		return
	}
	if hasUnsupportedFeature(query) {
		writeXMLError(w, request, requestID, http.StatusNotImplemented, "NotImplemented", "A requested S3 feature is not implemented")
		return
	}
	switch request.Method {
	case http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		h.listObjects(w, request, requestID)
	case http.MethodPost:
		if _, ok := query["delete"]; ok {
			h.deleteObjects(w, request, requestID)
			return
		}
		writeXMLError(w, request, requestID, http.StatusNotImplemented, "NotImplemented", "The requested bucket operation is not implemented")
	default:
		writeXMLError(w, request, requestID, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed")
	}
}

func (h Handler) handleObject(w http.ResponseWriter, request *http.Request, requestID, key string) {
	query := request.URL.Query()
	if _, ok := query["uploads"]; ok && request.Method == http.MethodPost {
		h.createMultipartUpload(w, request, requestID, key)
		return
	}
	if uploadID := query.Get("uploadId"); uploadID != "" {
		switch request.Method {
		case http.MethodPut:
			h.uploadPart(w, request, requestID, key, uploadID)
		case http.MethodGet:
			h.listParts(w, request, requestID, key, uploadID)
		case http.MethodPost:
			h.completeMultipartUpload(w, request, requestID, key, uploadID)
		case http.MethodDelete:
			h.abortMultipartUpload(w, request, requestID, key, uploadID)
		default:
			writeXMLError(w, request, requestID, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed")
		}
		return
	}
	if hasUnsupportedFeature(request.URL.Query()) {
		writeXMLError(w, request, requestID, http.StatusNotImplemented, "NotImplemented", "A requested S3 feature is not implemented")
		return
	}
	switch request.Method {
	case http.MethodPut:
		h.putObject(w, request, requestID, key)
	case http.MethodGet:
		h.getObject(w, request, requestID, key)
	case http.MethodHead:
		h.headObject(w, request, requestID, key)
	case http.MethodDelete:
		err := h.Objects.Delete(request.Context(), key)
		if err != nil && !errors.Is(err, r2.ErrObjectNotFound) {
			h.writeObjectError(w, request, requestID, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeXMLError(w, request, requestID, http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed")
	}
}

func (h Handler) putObject(w http.ResponseWriter, request *http.Request, requestID, key string) {
	if source := request.Header.Get("x-amz-copy-source"); source != "" {
		decoded, err := url.PathUnescape(strings.TrimPrefix(source, "/"))
		if err != nil {
			writeXMLError(w, request, requestID, http.StatusBadRequest, "InvalidArgument", "Invalid copy source")
			return
		}
		sourceBucket, sourceKey := splitPath("/" + decoded)
		if sourceBucket != h.Bucket || sourceKey == "" || r2.IsWebDAVInternalKey(sourceKey) {
			writeXMLError(w, request, requestID, http.StatusNotFound, "NoSuchKey", "The specified source key does not exist")
			return
		}
		object, err := h.Objects.Copy(request.Context(), sourceKey, key)
		if err != nil {
			h.writeObjectError(w, request, requestID, err)
			return
		}
		writeXML(w, http.StatusOK, copyObjectResult{LastModified: object.LastModified.UTC().Format(time.RFC3339), ETag: quoteETag(object.ETag)})
		return
	}
	metadata := make(map[string]string)
	for name, values := range request.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-meta-") {
			metadata[strings.TrimPrefix(lower, "x-amz-meta-")] = strings.Join(values, ",")
		}
	}
	object, err := h.Objects.Put(request.Context(), r2.PutRequest{
		Key: key, Body: request.Body, Size: request.ContentLength, ContentType: request.Header.Get("Content-Type"),
		Metadata: metadata, PayloadHash: request.Header.Get("X-Amz-Content-Sha256"),
	})
	if err != nil {
		h.writeObjectError(w, request, requestID, err)
		return
	}
	w.Header().Set("ETag", quoteETag(object.ETag))
	w.WriteHeader(http.StatusOK)
}

func (h Handler) getObject(w http.ResponseWriter, request *http.Request, requestID, key string) {
	for attempt := 0; attempt < 2; attempt++ {
		object, err := h.Objects.Stat(request.Context(), key)
		if err != nil {
			h.writeObjectError(w, request, requestID, err)
			return
		}
		evaluation, err := evaluateReadConditions(request, object)
		if err != nil {
			writeXMLError(w, request, requestID, http.StatusBadRequest, "InvalidArgument", "A conditional request header is invalid")
			return
		}
		if evaluation.status == http.StatusNotModified {
			setReadValidatorHeaders(w.Header(), object)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if evaluation.status == http.StatusPreconditionFailed {
			setReadValidatorHeaders(w.Header(), object)
			writeXMLError(w, request, requestID, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the preconditions you specified did not hold")
			return
		}
		result, err := h.Objects.Get(request.Context(), key, r2.GetOptions{
			Range: evaluation.range_, IfMatch: quoteETag(object.ETag), ExpectedObjectID: object.ObjectID,
		})
		if errors.Is(err, r2.ErrConditionalRequestConflict) && attempt == 0 {
			continue
		}
		if err != nil {
			if evaluation.range_ != "" && errors.Is(err, r2.ErrRangeNotSatisfiable) && object.Size >= 0 {
				w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(object.Size, 10))
			}
			h.writeObjectError(w, request, requestID, err)
			return
		}
		defer result.Body.Close()
		setObjectHeaders(w.Header(), result.ETag, result.ContentType, result.Size, result.LastModified, result.Metadata)
		if result.ContentRange != "" {
			w.Header().Set("Content-Range", result.ContentRange)
			w.WriteHeader(http.StatusPartialContent)
		}
		_, _ = io.Copy(w, result.Body)
		return
	}
}

func (h Handler) headObject(w http.ResponseWriter, request *http.Request, requestID, key string) {
	object, err := h.Objects.Stat(request.Context(), key)
	if err != nil {
		h.writeObjectError(w, request, requestID, err)
		return
	}
	evaluation, err := evaluateReadConditions(request, object)
	if err != nil {
		writeXMLError(w, request, requestID, http.StatusBadRequest, "InvalidArgument", "A conditional request header is invalid")
		return
	}
	setReadValidatorHeaders(w.Header(), object)
	if evaluation.status == http.StatusNotModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if evaluation.status == http.StatusPreconditionFailed {
		writeXMLError(w, request, requestID, http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the preconditions you specified did not hold")
		return
	}
	setObjectHeaders(w.Header(), object.ETag, object.ContentType, object.Size, object.LastModified, object.Metadata)
	w.WriteHeader(http.StatusOK)
}

func (h Handler) listBuckets(w http.ResponseWriter, requestID string) {
	now := time.Now().UTC().Format(time.RFC3339)
	writeXML(w, http.StatusOK, listAllMyBucketsResult{
		Owner:   s3Owner{ID: "cf-r2-manager", DisplayName: "CF-R2Manager"},
		Buckets: bucketCollection{Buckets: []bucketEntry{{Name: h.Bucket, CreationDate: now}}},
	})
}

func (h Handler) listObjects(w http.ResponseWriter, request *http.Request, requestID string) {
	query := request.URL.Query()
	limit := 1000
	if value := query.Get("max-keys"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			writeXMLError(w, request, requestID, http.StatusBadRequest, "InvalidArgument", "Invalid max-keys value")
			return
		}
		limit = parsed
	}
	if limit > 1000 {
		limit = 1000
	}
	after := query.Get("marker")
	isV2 := query.Get("list-type") == "2"
	if isV2 && query.Get("continuation-token") != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(query.Get("continuation-token"))
		if err != nil {
			writeXMLError(w, request, requestID, http.StatusBadRequest, "InvalidArgument", "Invalid continuation token")
			return
		}
		after = string(decoded)
	} else if isV2 {
		after = query.Get("start-after")
	}
	objects, prefixes, nextMarker, err := h.listObjectEntries(request.Context(), query.Get("prefix"), query.Get("delimiter"), after, limit)
	if err != nil {
		if errors.Is(err, r2.ErrR2CredentialsRequired) {
			writeXMLError(w, request, requestID, http.StatusServiceUnavailable, "ServiceUnavailable", "The configured Cloudflare account is missing R2 credentials")
			return
		}
		writeXMLError(w, request, requestID, http.StatusInternalServerError, "InternalError", "The object index could not be listed")
		return
	}
	result := listBucketResult{
		Name: h.Bucket, Prefix: query.Get("prefix"), Delimiter: query.Get("delimiter"),
		Marker: query.Get("marker"), StartAfter: query.Get("start-after"), MaxKeys: limit,
		IsTruncated: nextMarker != "", KeyCount: len(objects) + len(prefixes),
	}
	for _, object := range objects {
		result.Contents = append(result.Contents, objectEntry{
			Key: object.Key, LastModified: object.LastModified.UTC().Format(time.RFC3339), ETag: quoteETag(object.ETag),
			Size: object.Size, StorageClass: "STANDARD",
		})
	}
	for _, prefix := range prefixes {
		result.CommonPrefixes = append(result.CommonPrefixes, commonPrefix{Prefix: prefix})
	}
	if isV2 {
		result.ContinuationToken = query.Get("continuation-token")
		if nextMarker != "" {
			result.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(nextMarker))
		}
	} else {
		result.NextMarker = nextMarker
	}
	writeXML(w, http.StatusOK, result)
}

func (h Handler) listObjectEntries(ctx context.Context, prefix, delimiter, after string, limit int) ([]r2.Object, []string, string, error) {
	if limit == 0 {
		return nil, nil, "", nil
	}
	if r2.IsWebDAVInternalKey(prefix) {
		return nil, nil, "", nil
	}
	type entry struct {
		object *r2.Object
		prefix string
		marker string
	}
	entries := make([]entry, 0, limit+1)
	prefixEntries := make(map[string]int)
	cursor := after
	for len(entries) <= limit {
		page, err := h.Objects.List(ctx, r2.ListOptions{Prefix: prefix, After: cursor, Limit: 1000})
		if err != nil {
			return nil, nil, "", err
		}
		if len(page.Objects) == 0 {
			break
		}
		for index := range page.Objects {
			object := page.Objects[index]
			cursor = object.Key
			if r2.IsWebDAVInternalKey(object.Key) {
				continue
			}
			if delimiter != "" {
				remainder := strings.TrimPrefix(object.Key, prefix)
				if position := strings.Index(remainder, delimiter); position >= 0 {
					common := prefix + remainder[:position+len(delimiter)]
					if entryIndex, found := prefixEntries[common]; found {
						entries[entryIndex].marker = object.Key
						continue
					}
					prefixEntries[common] = len(entries)
					entries = append(entries, entry{prefix: common, marker: object.Key})
					if len(entries) > limit {
						break
					}
					continue
				}
			}
			copy := object
			entries = append(entries, entry{object: &copy, marker: object.Key})
			if len(entries) > limit {
				break
			}
		}
		if len(entries) > limit || page.NextMarker == "" {
			break
		}
		if page.NextMarker == cursor {
			continue
		}
		cursor = page.NextMarker
	}

	truncated := len(entries) > limit
	if truncated {
		entries = entries[:limit]
	}
	var objects []r2.Object
	var prefixes []string
	for _, item := range entries {
		if item.object != nil {
			objects = append(objects, *item.object)
		} else {
			prefixes = append(prefixes, item.prefix)
		}
	}
	if truncated && len(entries) != 0 {
		return objects, prefixes, entries[len(entries)-1].marker, nil
	}
	return objects, prefixes, "", nil
}

func (h Handler) deleteObjects(w http.ResponseWriter, request *http.Request, requestID string) {
	var input deleteObjectsRequest
	decoder := xml.NewDecoder(io.LimitReader(request.Body, 2<<20))
	if err := decoder.Decode(&input); err != nil || len(input.Objects) > 1000 {
		writeXMLError(w, request, requestID, http.StatusBadRequest, "MalformedXML", "The XML request body is invalid")
		return
	}
	result := deleteObjectsResult{}
	for _, object := range input.Objects {
		if r2.IsWebDAVInternalKey(object.Key) {
			result.Errors = append(result.Errors, deleteErrorEntry{Key: object.Key, Code: "AccessDenied", Message: "Access Denied"})
			continue
		}
		err := h.Objects.Delete(request.Context(), object.Key)
		if err == nil || errors.Is(err, r2.ErrObjectNotFound) {
			if !input.Quiet {
				result.Deleted = append(result.Deleted, deletedEntry{Key: object.Key})
			}
			continue
		}
		if errors.Is(err, r2.ErrR2CredentialsRequired) {
			result.Errors = append(result.Errors, deleteErrorEntry{Key: object.Key, Code: "ServiceUnavailable", Message: "The configured Cloudflare account is missing R2 credentials"})
		} else {
			result.Errors = append(result.Errors, deleteErrorEntry{Key: object.Key, Code: "InternalError", Message: "The object could not be deleted"})
		}
	}
	writeXML(w, http.StatusOK, result)
}

func (h Handler) writeObjectError(w http.ResponseWriter, request *http.Request, requestID string, err error) {
	switch {
	case errors.Is(err, r2.ErrObjectNotFound):
		writeXMLError(w, request, requestID, http.StatusNotFound, "NoSuchKey", "The specified key does not exist")
	case errors.Is(err, r2.ErrQuotaExceeded):
		writeXMLError(w, request, requestID, http.StatusInsufficientStorage, "QuotaExceeded", "The unified R2 pool soft quota is exceeded")
	case errors.Is(err, r2.ErrR2CredentialsRequired):
		writeXMLError(w, request, requestID, http.StatusServiceUnavailable, "ServiceUnavailable", "The configured Cloudflare account is missing R2 credentials")
	case errors.Is(err, r2.ErrWriteInProgress):
		writeXMLError(w, request, requestID, http.StatusConflict, "OperationAborted", "A conflicting operation is in progress for this key")
	case errors.Is(err, r2.ErrConditionalRequestConflict):
		writeXMLError(w, request, requestID, http.StatusConflict, "OperationAborted", "The object changed while the conditional operation was in progress")
	case errors.Is(err, r2.ErrBucketDeleting):
		writeXMLError(w, request, requestID, http.StatusServiceUnavailable, "ServiceUnavailable", "The bucket is temporarily unavailable while deletion is in progress")
	case errors.Is(err, r2.ErrRateLimited):
		writeXMLError(w, request, requestID, http.StatusServiceUnavailable, "SlowDown", "Please reduce your request rate")
	case errors.Is(err, r2.ErrRangeNotSatisfiable):
		writeXMLError(w, request, requestID, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable")
	case errors.Is(err, r2.ErrPayloadHashMismatch):
		writeXMLError(w, request, requestID, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The provided payload hash does not match")
	default:
		writeXMLError(w, request, requestID, http.StatusBadGateway, "InternalError", "The upstream object operation failed")
	}
}

func (h Handler) writeAuthError(w http.ResponseWriter, request *http.Request, requestID string, err error) {
	switch {
	case errors.Is(err, ErrAuthorizationRequired):
		writeXMLError(w, request, requestID, http.StatusForbidden, "AccessDenied", "AWS authentication is required")
	case errors.Is(err, ErrInvalidAccessKey):
		writeXMLError(w, request, requestID, http.StatusForbidden, "InvalidAccessKeyId", "The AWS access key ID does not exist")
	case errors.Is(err, ErrRequestTimeSkewed), errors.Is(err, ErrRequestExpired):
		writeXMLError(w, request, requestID, http.StatusForbidden, "RequestTimeTooSkewed", "The difference between request time and server time is too large")
	case errors.Is(err, ErrSignatureMismatch):
		writeXMLError(w, request, requestID, http.StatusForbidden, "SignatureDoesNotMatch", "The request signature does not match")
	default:
		writeXMLError(w, request, requestID, http.StatusBadRequest, "AuthorizationHeaderMalformed", "The authorization header is malformed")
	}
}

func splitPath(path string) (string, string) {
	value := strings.TrimPrefix(path, "/")
	bucket, key, found := strings.Cut(value, "/")
	if !found {
		return bucket, ""
	}
	return bucket, key
}

func hasUnsupportedFeature(query url.Values) bool {
	for _, feature := range []string{"acl", "versioning", "versions", "versionId", "tagging", "torrent"} {
		if _, found := query[feature]; found {
			return true
		}
	}
	return false
}

func setObjectHeaders(headers http.Header, etag, contentType string, size int64, modified time.Time, metadata map[string]string) {
	headers.Set("Accept-Ranges", "bytes")
	headers.Set("Content-Length", strconv.FormatInt(size, 10))
	if etag != "" {
		headers.Set("ETag", quoteETag(etag))
	}
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	if !modified.IsZero() {
		headers.Set("Last-Modified", modified.UTC().Format(http.TimeFormat))
	}
	for key, value := range metadata {
		headers.Set("x-amz-meta-"+key, value)
	}
}

func setReadValidatorHeaders(headers http.Header, object r2.Object) {
	if object.ETag != "" {
		headers.Set("ETag", quoteETag(object.ETag))
	}
	if !object.LastModified.IsZero() {
		headers.Set("Last-Modified", object.LastModified.UTC().Format(http.TimeFormat))
	}
}

func parseHTTPDate(value string) *time.Time {
	if value == "" {
		return nil
	}
	parsed, err := http.ParseTime(value)
	if err != nil {
		return nil
	}
	return &parsed
}

func quoteETag(value string) string {
	return `"` + strings.Trim(value, `"`) + `"`
}

func writeXML(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(value)
}

func writeXMLError(w http.ResponseWriter, request *http.Request, requestID string, status int, code, message string) {
	value := s3Error{Code: code, Message: message, Resource: request.URL.Path, RequestID: requestID}
	if request.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	writeXML(w, status, value)
}

type s3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
}

type s3Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type bucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type bucketCollection struct {
	Buckets []bucketEntry `xml:"Bucket"`
}

type listAllMyBucketsResult struct {
	XMLName xml.Name         `xml:"ListAllMyBucketsResult"`
	Owner   s3Owner          `xml:"Owner"`
	Buckets bucketCollection `xml:"Buckets"`
}

type objectEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type listBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	Marker                string         `xml:"Marker,omitempty"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	NextMarker            string         `xml:"NextMarker,omitempty"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	KeyCount              int            `xml:"KeyCount,omitempty"`
	MaxKeys               int            `xml:"MaxKeys"`
	IsTruncated           bool           `xml:"IsTruncated"`
	Contents              []objectEntry  `xml:"Contents"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
}

type copyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

type deleteObjectIdentifier struct {
	Key string `xml:"Key"`
}

type deleteObjectsRequest struct {
	Objects []deleteObjectIdentifier `xml:"Object"`
	Quiet   bool                     `xml:"Quiet"`
}

type deletedEntry struct {
	Key string `xml:"Key"`
}

type deleteErrorEntry struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

type deleteObjectsResult struct {
	XMLName xml.Name           `xml:"DeleteResult"`
	Deleted []deletedEntry     `xml:"Deleted,omitempty"`
	Errors  []deleteErrorEntry `xml:"Error,omitempty"`
}
