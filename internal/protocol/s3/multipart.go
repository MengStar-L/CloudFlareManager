package s3protocol

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
)

type MultipartObjectService interface {
	CreateMultipart(context.Context, r2.CreateMultipartInput) (r2.MultipartUpload, error)
	UploadPart(context.Context, r2.UploadPartRequest) (r2.MultipartPart, error)
	ListParts(context.Context, string, string, int32, int) (r2.MultipartPartList, error)
	CompleteMultipart(context.Context, r2.CompleteMultipartRequest) (r2.Object, error)
	AbortMultipart(context.Context, string, string) error
	ListMultipart(context.Context, r2.ListMultipartOptions) (r2.MultipartUploadList, error)
}

func (h Handler) createMultipartUpload(w http.ResponseWriter, request *http.Request, requestID, key string) {
	service, ok := h.Objects.(MultipartObjectService)
	if !ok {
		writeXMLError(w, request, requestID, http.StatusNotImplemented, "NotImplemented", "Multipart uploads are unavailable")
		return
	}
	metadata := make(map[string]string)
	for name, values := range request.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-meta-") {
			metadata[strings.TrimPrefix(lower, "x-amz-meta-")] = strings.Join(values, ",")
		}
	}
	upload, err := service.CreateMultipart(request.Context(), r2.CreateMultipartInput{
		Key: key, ContentType: request.Header.Get("Content-Type"), Metadata: metadata,
	})
	if err != nil {
		h.writeMultipartError(w, request, requestID, err)
		return
	}
	writeXML(w, http.StatusOK, initiateMultipartUploadResult{Bucket: h.Bucket, Key: key, UploadID: upload.ID})
}

func (h Handler) uploadPart(w http.ResponseWriter, request *http.Request, requestID, key, uploadID string) {
	service, ok := h.Objects.(MultipartObjectService)
	if !ok {
		writeXMLError(w, request, requestID, http.StatusNotImplemented, "NotImplemented", "Multipart uploads are unavailable")
		return
	}
	partNumber, err := strconv.ParseInt(request.URL.Query().Get("partNumber"), 10, 32)
	if err != nil || partNumber < 1 || partNumber > 10000 {
		writeXMLError(w, request, requestID, http.StatusBadRequest, "InvalidArgument", "partNumber must be between 1 and 10000")
		return
	}
	body := io.Reader(request.Body)
	size := request.ContentLength
	payloadHash := request.Header.Get("X-Amz-Content-Sha256")
	isCopy := false
	if source := request.Header.Get("x-amz-copy-source"); source != "" {
		decoded, decodeErr := url.PathUnescape(strings.TrimPrefix(source, "/"))
		if decodeErr != nil {
			writeXMLError(w, request, requestID, http.StatusBadRequest, "InvalidArgument", "Invalid copy source")
			return
		}
		sourceBucket, sourceKey := splitPath("/" + decoded)
		if sourceBucket != h.Bucket || sourceKey == "" {
			writeXMLError(w, request, requestID, http.StatusNotFound, "NoSuchKey", "The specified source key does not exist")
			return
		}
		result, getErr := h.Objects.Get(request.Context(), sourceKey, r2.GetOptions{Range: request.Header.Get("x-amz-copy-source-range")})
		if getErr != nil {
			h.writeObjectError(w, request, requestID, getErr)
			return
		}
		defer result.Body.Close()
		body = result.Body
		size = result.Size
		payloadHash = "UNSIGNED-PAYLOAD"
		isCopy = true
	}
	part, err := service.UploadPart(request.Context(), r2.UploadPartRequest{
		Key: key, UploadID: uploadID, PartNumber: int32(partNumber), Body: body,
		Size: size, PayloadHash: payloadHash,
	})
	if err != nil {
		h.writeMultipartError(w, request, requestID, err)
		return
	}
	if isCopy {
		writeXML(w, http.StatusOK, copyPartResult{
			LastModified: part.LastModified.UTC().Format(time.RFC3339), ETag: quoteETag(part.ETag),
		})
		return
	}
	w.Header().Set("ETag", quoteETag(part.ETag))
	w.WriteHeader(http.StatusOK)
}

func (h Handler) listParts(w http.ResponseWriter, request *http.Request, requestID, key, uploadID string) {
	service, ok := h.Objects.(MultipartObjectService)
	if !ok {
		writeXMLError(w, request, requestID, http.StatusNotImplemented, "NotImplemented", "Multipart uploads are unavailable")
		return
	}
	after, err := strconv.ParseInt(defaultString(request.URL.Query().Get("part-number-marker"), "0"), 10, 32)
	if err != nil || after < 0 {
		writeXMLError(w, request, requestID, http.StatusBadRequest, "InvalidArgument", "Invalid part-number-marker")
		return
	}
	limit := parseBoundedLimit(request.URL.Query().Get("max-parts"), 1000)
	parts, err := service.ListParts(request.Context(), key, uploadID, int32(after), limit)
	if err != nil {
		h.writeMultipartError(w, request, requestID, err)
		return
	}
	result := listPartsResult{
		Bucket: h.Bucket, Key: key, UploadID: uploadID, PartNumberMarker: int32(after),
		MaxParts: limit, IsTruncated: parts.NextPartNumber != 0, NextPartNumberMarker: parts.NextPartNumber,
	}
	for _, part := range parts.Parts {
		result.Parts = append(result.Parts, multipartPartEntry{
			PartNumber: part.PartNumber, LastModified: part.LastModified.UTC().Format(time.RFC3339),
			ETag: quoteETag(part.ETag), Size: part.Size,
		})
	}
	writeXML(w, http.StatusOK, result)
}

func (h Handler) completeMultipartUpload(w http.ResponseWriter, request *http.Request, requestID, key, uploadID string) {
	service, ok := h.Objects.(MultipartObjectService)
	if !ok {
		writeXMLError(w, request, requestID, http.StatusNotImplemented, "NotImplemented", "Multipart uploads are unavailable")
		return
	}
	var input completeMultipartUploadRequest
	decoder := xml.NewDecoder(io.LimitReader(request.Body, 2<<20))
	if err := decoder.Decode(&input); err != nil || len(input.Parts) == 0 || len(input.Parts) > 10000 {
		writeXMLError(w, request, requestID, http.StatusBadRequest, "MalformedXML", "The XML request body is invalid")
		return
	}
	parts := make([]r2.CompletedPart, 0, len(input.Parts))
	for _, part := range input.Parts {
		parts = append(parts, r2.CompletedPart{PartNumber: part.PartNumber, ETag: part.ETag})
	}
	object, err := service.CompleteMultipart(request.Context(), r2.CompleteMultipartRequest{
		Key: key, UploadID: uploadID, Parts: parts,
	})
	if err != nil {
		h.writeMultipartError(w, request, requestID, err)
		return
	}
	writeXML(w, http.StatusOK, completeMultipartUploadResult{
		Location: "/" + h.Bucket + "/" + key, Bucket: h.Bucket, Key: key, ETag: quoteETag(object.ETag),
	})
}

func (h Handler) abortMultipartUpload(w http.ResponseWriter, request *http.Request, requestID, key, uploadID string) {
	service, ok := h.Objects.(MultipartObjectService)
	if !ok {
		writeXMLError(w, request, requestID, http.StatusNotImplemented, "NotImplemented", "Multipart uploads are unavailable")
		return
	}
	if err := service.AbortMultipart(request.Context(), key, uploadID); err != nil {
		h.writeMultipartError(w, request, requestID, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h Handler) listMultipartUploads(w http.ResponseWriter, request *http.Request, requestID string) {
	service, ok := h.Objects.(MultipartObjectService)
	if !ok {
		writeXMLError(w, request, requestID, http.StatusNotImplemented, "NotImplemented", "Multipart uploads are unavailable")
		return
	}
	query := request.URL.Query()
	limit := parseBoundedLimit(query.Get("max-uploads"), 1000)
	prefix := query.Get("prefix")
	visible := make([]r2.MultipartUpload, 0, limit+1)
	cursor := query.Get("key-marker")
	if !r2.IsWebDAVInternalKey(prefix) {
		for len(visible) <= limit {
			page, err := service.ListMultipart(request.Context(), r2.ListMultipartOptions{
				Prefix: prefix, After: cursor, Limit: 1000,
			})
			if err != nil {
				h.writeMultipartError(w, request, requestID, err)
				return
			}
			for _, upload := range page.Uploads {
				cursor = upload.Key
				if r2.IsWebDAVInternalKey(upload.Key) {
					continue
				}
				visible = append(visible, upload)
				if len(visible) > limit {
					break
				}
			}
			if len(visible) > limit || page.NextMarker == "" {
				break
			}
			cursor = page.NextMarker
		}
	}
	truncated := len(visible) > limit
	if truncated {
		visible = visible[:limit]
	}
	result := listMultipartUploadsResult{
		Bucket: h.Bucket, Prefix: prefix, KeyMarker: query.Get("key-marker"),
		MaxUploads: limit, IsTruncated: truncated,
	}
	if truncated && len(visible) > 0 {
		result.NextKeyMarker = visible[len(visible)-1].Key
	}
	for _, upload := range visible {
		result.Uploads = append(result.Uploads, multipartUploadEntry{
			Key: upload.Key, UploadID: upload.ID, Initiated: upload.CreatedAt.UTC().Format(time.RFC3339),
			Initiator: s3Owner{ID: "cf-r2-manager", DisplayName: "CF-R2Manager"},
			Owner:     s3Owner{ID: "cf-r2-manager", DisplayName: "CF-R2Manager"}, StorageClass: "STANDARD",
		})
	}
	writeXML(w, http.StatusOK, result)
}

func (h Handler) writeMultipartError(w http.ResponseWriter, request *http.Request, requestID string, err error) {
	switch {
	case errors.Is(err, r2.ErrMultipartNotFound):
		writeXMLError(w, request, requestID, http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist")
	case errors.Is(err, r2.ErrInvalidPartOrder):
		writeXMLError(w, request, requestID, http.StatusBadRequest, "InvalidPartOrder", "The list of parts was not in ascending order")
	case errors.Is(err, r2.ErrInvalidPart):
		writeXMLError(w, request, requestID, http.StatusBadRequest, "InvalidPart", "One or more specified parts could not be found")
	case errors.Is(err, r2.ErrQuotaExceeded):
		writeXMLError(w, request, requestID, http.StatusInsufficientStorage, "QuotaExceeded", "The unified R2 pool soft quota is exceeded")
	case errors.Is(err, r2.ErrWriteInProgress):
		writeXMLError(w, request, requestID, http.StatusConflict, "OperationAborted", "A conflicting operation is in progress for this key")
	case errors.Is(err, r2.ErrConditionalRequestConflict):
		writeXMLError(w, request, requestID, http.StatusConflict, "OperationAborted", "The object changed while the conditional operation was in progress")
	case errors.Is(err, r2.ErrRateLimited):
		writeXMLError(w, request, requestID, http.StatusServiceUnavailable, "SlowDown", "Please reduce your request rate")
	case errors.Is(err, r2.ErrPayloadHashMismatch):
		writeXMLError(w, request, requestID, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The provided payload hash does not match")
	default:
		writeXMLError(w, request, requestID, http.StatusBadGateway, "InternalError", "The upstream multipart operation failed")
	}
}

func parseBoundedLimit(value string, fallback int) int {
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 0 || limit > 1000 {
		return fallback
	}
	return limit
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type completeMultipartPart struct {
	PartNumber int32  `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartUploadRequest struct {
	Parts []completeMultipartPart `xml:"Part"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type copyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

type multipartPartEntry struct {
	PartNumber   int32  `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

type listPartsResult struct {
	XMLName              xml.Name             `xml:"ListPartsResult"`
	Bucket               string               `xml:"Bucket"`
	Key                  string               `xml:"Key"`
	UploadID             string               `xml:"UploadId"`
	PartNumberMarker     int32                `xml:"PartNumberMarker"`
	NextPartNumberMarker int32                `xml:"NextPartNumberMarker,omitempty"`
	MaxParts             int                  `xml:"MaxParts"`
	IsTruncated          bool                 `xml:"IsTruncated"`
	Parts                []multipartPartEntry `xml:"Part"`
}

type multipartUploadEntry struct {
	Key          string  `xml:"Key"`
	UploadID     string  `xml:"UploadId"`
	Initiator    s3Owner `xml:"Initiator"`
	Owner        s3Owner `xml:"Owner"`
	StorageClass string  `xml:"StorageClass"`
	Initiated    string  `xml:"Initiated"`
}

type listMultipartUploadsResult struct {
	XMLName       xml.Name               `xml:"ListMultipartUploadsResult"`
	Bucket        string                 `xml:"Bucket"`
	KeyMarker     string                 `xml:"KeyMarker,omitempty"`
	NextKeyMarker string                 `xml:"NextKeyMarker,omitempty"`
	Prefix        string                 `xml:"Prefix,omitempty"`
	MaxUploads    int                    `xml:"MaxUploads"`
	IsTruncated   bool                   `xml:"IsTruncated"`
	Uploads       []multipartUploadEntry `xml:"Upload"`
}
