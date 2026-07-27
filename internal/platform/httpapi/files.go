package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
)

const maxTextPreviewBytes = 1 << 20

func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	if a.deps.Jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "job storage is unavailable")
		return
	}
	job, err := a.deps.Jobs.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, jobs.ErrNotFound) {
		writeError(w, http.StatusNotFound, "job_not_found", "job was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not load the job")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

func (a *API) listFiles(w http.ResponseWriter, r *http.Request) {
	if a.deps.R2Service == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "R2 file service is unavailable")
		return
	}
	result, err := a.deps.R2Service.ListDirectory(r.Context(), r2.DirectoryListOptions{
		Path: r.URL.Query().Get("path"), After: r.URL.Query().Get("after"),
		Kind: r2.EntryKind(r.URL.Query().Get("kind")), Limit: queryLimit(r, 100),
	})
	if err != nil {
		writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) getFileContent(w http.ResponseWriter, r *http.Request) {
	service := a.deps.R2Service
	if service == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "R2 file service is unavailable")
		return
	}
	key := r.URL.Query().Get("key")
	entry, err := service.ResolveEntry(r.Context(), key)
	if err != nil {
		writeFileError(w, err)
		return
	}
	if entry.Kind != r2.EntryFile {
		writeError(w, http.StatusBadRequest, "not_a_file", "the selected path is not a file")
		return
	}
	mode := defaultQuery(r.URL.Query().Get("mode"), "download")
	contentType := effectiveContentType(key, entry.ContentType)
	preview := ""
	if mode == "preview" {
		preview = previewType(contentType)
		if preview == "" {
			writeError(w, http.StatusUnsupportedMediaType, "preview_unsupported", "this file type cannot be previewed safely")
			return
		}
		if preview == "text" && entry.Size > maxTextPreviewBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "preview_too_large", "text preview is limited to 1 MiB")
			return
		}
	} else if mode != "download" {
		writeError(w, http.StatusBadRequest, "invalid_mode", "mode must be preview or download")
		return
	}

	options := r2.GetOptions{}
	if mode == "download" || preview == "media" {
		options.Range = r.Header.Get("Range")
	}
	result, err := service.Get(r.Context(), key, options)
	if err != nil {
		writeFileError(w, err)
		return
	}
	defer result.Body.Close()

	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if result.ETag != "" {
		w.Header().Set("ETag", strconv.Quote(strings.Trim(result.ETag, `"`)))
	}
	if !result.LastModified.IsZero() {
		w.Header().Set("Last-Modified", result.LastModified.UTC().Format(http.TimeFormat))
	}
	if result.ContentRange != "" {
		w.Header().Set("Content-Range", result.ContentRange)
	}
	if result.Size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	}
	if mode == "download" {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", contentDisposition("attachment", fileName(key)))
	} else {
		w.Header().Set("Content-Disposition", contentDisposition("inline", fileName(key)))
		if preview == "text" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", contentType)
		}
	}
	status := http.StatusOK
	if result.ContentRange != "" {
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	if _, err := io.Copy(w, result.Body); err == nil && mode == "download" {
		a.record(r, "admin", "files.download", "files/"+key, "success", map[string]any{"size": entry.Size})
	}
}

func (a *API) putFileContent(w http.ResponseWriter, r *http.Request) {
	service := a.deps.R2Service
	if service == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "R2 file service is unavailable")
		return
	}
	key := r.URL.Query().Get("key")
	overwrite := queryBool(r.URL.Query().Get("overwrite"))
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, previousErr := service.Stat(r.Context(), key)
	object, err := service.PutFile(r.Context(), r2.PutRequest{
		Key: key, Body: r.Body, Size: r.ContentLength, ContentType: contentType,
	}, overwrite)
	if err != nil {
		a.record(r, "admin", "files.upload", "files/"+key, "failure", map[string]any{"error": err.Error()})
		writeFileError(w, err)
		return
	}
	entry, err := service.ResolveEntry(r.Context(), object.Key)
	if err != nil {
		writeFileError(w, err)
		return
	}
	status := http.StatusCreated
	if previousErr == nil {
		status = http.StatusOK
	}
	a.record(r, "admin", "files.upload", "files/"+key, "success", map[string]any{"size": object.Size, "overwrite": overwrite})
	writeJSON(w, status, map[string]any{"entry": entry})
}

func (a *API) createFileDirectory(w http.ResponseWriter, r *http.Request) {
	if a.deps.R2Service == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "R2 file service is unavailable")
		return
	}
	var input struct {
		Path string `json:"path"`
	}
	if decodeJSON(w, r, &input) != nil {
		return
	}
	entry, err := a.deps.R2Service.CreateDirectory(r.Context(), input.Path)
	if err != nil {
		a.record(r, "admin", "files.directory.create", "files/"+input.Path, "failure", map[string]any{"error": err.Error()})
		writeFileError(w, err)
		return
	}
	a.record(r, "admin", "files.directory.create", "files/"+input.Path, "success", nil)
	writeJSON(w, http.StatusCreated, map[string]any{"entry": entry})
}

func (a *API) fileOperation(w http.ResponseWriter, r *http.Request) {
	service := a.deps.R2Service
	if service == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "R2 file service is unavailable")
		return
	}
	var input struct {
		Operation   string `json:"operation"`
		Source      string `json:"source"`
		Destination string `json:"destination,omitempty"`
		Overwrite   bool   `json:"overwrite,omitempty"`
	}
	if decodeJSON(w, r, &input) != nil {
		return
	}
	entry, err := service.ResolveEntry(r.Context(), input.Source)
	if err != nil {
		writeFileError(w, err)
		return
	}
	detail := map[string]any{"destination": input.Destination, "overwrite": input.Overwrite}
	jobPayload := r2.FileJobPayload{Source: input.Source, Destination: input.Destination, Overwrite: input.Overwrite}

	switch input.Operation {
	case "delete":
		if entry.Kind == r2.EntryFile {
			err = service.Delete(r.Context(), input.Source)
			break
		}
		if a.deps.Jobs == nil {
			writeError(w, http.StatusServiceUnavailable, "not_configured", "job storage is unavailable")
			return
		}
		job, enqueueErr := a.deps.Jobs.Enqueue(r.Context(), r2.FileDeleteJobType, jobPayload, 3)
		if enqueueErr != nil {
			writeFileError(w, enqueueErr)
			return
		}
		a.record(r, "admin", "files.delete", "files/"+input.Source, "success", map[string]any{"job_id": job.ID})
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
		return
	case "move":
		if entry.Kind == r2.EntryFile {
			err = service.MoveFile(r.Context(), input.Source, input.Destination, input.Overwrite)
			break
		}
		if err = service.ValidateDirectoryMove(r.Context(), input.Source, input.Destination, input.Overwrite); err != nil {
			writeFileError(w, err)
			return
		}
		if a.deps.Jobs == nil {
			writeError(w, http.StatusServiceUnavailable, "not_configured", "job storage is unavailable")
			return
		}
		job, enqueueErr := a.deps.Jobs.Enqueue(r.Context(), r2.FileMoveJobType, jobPayload, 3)
		if enqueueErr != nil {
			writeFileError(w, enqueueErr)
			return
		}
		a.record(r, "admin", "files.move", "files/"+input.Source, "success", map[string]any{"job_id": job.ID, "destination": input.Destination})
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "job": job})
		return
	default:
		writeError(w, http.StatusBadRequest, "invalid_operation", "operation must be move or delete")
		return
	}
	if err != nil {
		a.record(r, "admin", "files."+input.Operation, "files/"+input.Source, "failure", map[string]any{"error": err.Error()})
		writeFileError(w, err)
		return
	}
	a.record(r, "admin", "files."+input.Operation, "files/"+input.Source, "success", detail)
	writeJSON(w, http.StatusOK, map[string]string{"status": "completed"})
}

func writeFileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, r2.ErrInvalidPath), errors.Is(err, r2.ErrInvalidCursor):
		writeError(w, http.StatusBadRequest, "invalid_path", "the file path or cursor is invalid")
	case errors.Is(err, r2.ErrFileConflict):
		writeError(w, http.StatusConflict, "file_conflict", "a file or directory already exists at the destination")
	case errors.Is(err, r2.ErrObjectNotFound):
		writeError(w, http.StatusNotFound, "file_not_found", "the file or directory was not found")
	case errors.Is(err, r2.ErrQuotaExceeded):
		writeError(w, http.StatusInsufficientStorage, "storage_limit", "the R2 storage limit has been reached")
	default:
		writeError(w, http.StatusBadGateway, "r2_error", err.Error())
	}
}

func queryBool(value string) bool {
	parsed, _ := strconv.ParseBool(value)
	return parsed || value == "1"
}

func effectiveContentType(key, stored string) string {
	value := strings.TrimSpace(strings.Split(stored, ";")[0])
	if value != "" && value != "application/octet-stream" {
		return value
	}
	if detected := mime.TypeByExtension(strings.ToLower(filepath.Ext(key))); detected != "" {
		if mediaType, _, err := mime.ParseMediaType(detected); err == nil {
			return mediaType
		}
	}
	return "application/octet-stream"
}

func previewType(contentType string) string {
	contentType = strings.ToLower(contentType)
	if contentType == "application/json" || strings.HasSuffix(contentType, "+json") ||
		(strings.HasPrefix(contentType, "text/") && contentType != "text/html") {
		return "text"
	}
	switch contentType {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/avif", "image/bmp", "image/x-icon",
		"audio/mpeg", "audio/ogg", "audio/wav", "audio/x-wav", "audio/mp4", "audio/webm",
		"video/mp4", "video/webm", "video/ogg", "video/quicktime":
		return "media"
	default:
		return ""
	}
}

func contentDisposition(disposition, name string) string {
	value := mime.FormatMediaType(disposition, map[string]string{"filename": name})
	if value == "" {
		return disposition
	}
	return value
}

func fileName(key string) string {
	trimmed := strings.TrimSuffix(key, "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}
