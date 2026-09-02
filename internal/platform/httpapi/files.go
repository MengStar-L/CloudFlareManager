package httpapi

import (
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/credentials"
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
	if a.deps.R2Service == nil || a.deps.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "R2 file service is unavailable")
		return
	}
	mountID := r.URL.Query().Get("mount_id")
	if mountID == "" {
		if r.URL.Query().Get("path") != "" {
			writeError(w, http.StatusBadRequest, "mount_required", "mount_id is required for a file path")
			return
		}
		a.listFileMounts(w, r)
		return
	}
	mount, ok := a.fileMount(w, r, mountID)
	if !ok {
		return
	}
	result, err := a.deps.R2Service.ListWebDAVDirectory(r.Context(), mount.ID, r2.DirectoryListOptions{
		Path: r.URL.Query().Get("path"), After: r.URL.Query().Get("after"),
		Kind: r2.EntryKind(r.URL.Query().Get("kind")), Limit: queryLimit(r, 100),
	})
	if err != nil {
		writeFileError(w, err)
		return
	}
	result.MountName = mount.Name
	writeJSON(w, http.StatusOK, result)
}

func (a *API) listFileMounts(w http.ResponseWriter, r *http.Request) {
	items, err := a.deps.Credentials.List(r.Context(), credentials.KindWebDAV)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not list WebDAV mounts")
		return
	}
	sort.Slice(items, func(i, j int) bool { return fileMountSortKey(items[i]) < fileMountSortKey(items[j]) })
	after := ""
	if encoded := r.URL.Query().Get("after"); encoded != "" {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(encoded)
		if decodeErr != nil {
			writeFileError(w, r2.ErrInvalidCursor)
			return
		}
		after = string(decoded)
	}
	limit := queryLimit(r, 100)
	entries := make([]r2.FileEntry, 0, limit+1)
	sortKeys := make([]string, 0, limit+1)
	for _, item := range items {
		sortKey := fileMountSortKey(item)
		if sortKey <= after {
			continue
		}
		entries = append(entries, r2.FileEntry{
			Name: item.Name, Kind: r2.EntryMount, ContentType: r2.DirectoryContentType,
			LastModified: item.UpdatedAt, MountID: item.ID, Disabled: item.Disabled,
		})
		sortKeys = append(sortKeys, sortKey)
		if len(entries) > limit {
			break
		}
	}
	result := r2.DirectoryList{Entries: entries, DirectoryCount: len(items)}
	if len(entries) > limit {
		result.NextMarker = base64.RawURLEncoding.EncodeToString([]byte(sortKeys[limit-1]))
		result.Entries = entries[:limit]
	}
	writeJSON(w, http.StatusOK, result)
}

func fileMountSortKey(value credentials.Credential) string {
	return strings.ToLower(value.Name) + "\x00" + value.ID
}

func (a *API) fileMount(w http.ResponseWriter, r *http.Request, id string) (credentials.Credential, bool) {
	if id == "" {
		writeError(w, http.StatusBadRequest, "mount_required", "mount_id is required")
		return credentials.Credential{}, false
	}
	mount, err := a.deps.Credentials.Get(r.Context(), id)
	if err != nil || mount.Kind != credentials.KindWebDAV {
		writeError(w, http.StatusNotFound, "mount_not_found", "WebDAV mount was not found")
		return credentials.Credential{}, false
	}
	return mount, true
}

func (a *API) getFileContent(w http.ResponseWriter, r *http.Request) {
	service := a.deps.R2Service
	if service == nil || a.deps.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "R2 file service is unavailable")
		return
	}
	mount, ok := a.fileMount(w, r, r.URL.Query().Get("mount_id"))
	if !ok {
		return
	}
	key := r.URL.Query().Get("key")
	entry, err := service.ResolveWebDAVEntry(r.Context(), mount.ID, key)
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
	internalKey, err := r2.WebDAVMountKey(mount.ID, key)
	if err != nil {
		writeFileError(w, err)
		return
	}
	result, err := service.Get(r.Context(), internalKey, options)
	if err != nil {
		if options.Range != "" && errors.Is(err, r2.ErrRangeNotSatisfiable) && entry.Size >= 0 {
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(entry.Size, 10))
		}
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
		a.record(r, "admin", "files.download", "files/"+key, "success", map[string]any{"size": entry.Size, "mount_id": mount.ID})
	}
}

func (a *API) putFileContent(w http.ResponseWriter, r *http.Request) {
	service := a.deps.R2Service
	if service == nil || a.deps.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "R2 file service is unavailable")
		return
	}
	mount, ok := a.fileMount(w, r, r.URL.Query().Get("mount_id"))
	if !ok {
		return
	}
	key := r.URL.Query().Get("key")
	internalKey, err := r2.WebDAVMountKey(mount.ID, key)
	if err != nil {
		writeFileError(w, err)
		return
	}
	overwrite := queryBool(r.URL.Query().Get("overwrite"))
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, previousErr := service.Stat(r.Context(), internalKey)
	object, err := service.PutFile(r.Context(), r2.PutRequest{
		Key: internalKey, Body: r.Body, Size: r.ContentLength, ContentType: contentType,
	}, overwrite)
	if err != nil {
		a.record(r, "admin", "files.upload", "files/"+key, "failure", map[string]any{"error": err.Error()})
		writeFileError(w, err)
		return
	}
	entry, err := service.ResolveWebDAVEntry(r.Context(), mount.ID, key)
	if err != nil {
		writeFileError(w, err)
		return
	}
	status := http.StatusCreated
	if previousErr == nil {
		status = http.StatusOK
	}
	a.record(r, "admin", "files.upload", "files/"+key, "success", map[string]any{"size": object.Size, "overwrite": overwrite, "mount_id": mount.ID})
	writeJSON(w, status, map[string]any{"entry": entry})
}

func (a *API) createFileDirectory(w http.ResponseWriter, r *http.Request) {
	if a.deps.R2Service == nil || a.deps.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "R2 file service is unavailable")
		return
	}
	var input struct {
		MountID string `json:"mount_id"`
		Path    string `json:"path"`
	}
	if decodeJSON(w, r, &input) != nil {
		return
	}
	mount, ok := a.fileMount(w, r, input.MountID)
	if !ok {
		return
	}
	entry, err := a.deps.R2Service.CreateWebDAVDirectory(r.Context(), mount.ID, input.Path)
	if err != nil {
		a.record(r, "admin", "files.directory.create", "files/"+input.Path, "failure", map[string]any{"error": err.Error()})
		writeFileError(w, err)
		return
	}
	a.record(r, "admin", "files.directory.create", "files/"+input.Path, "success", map[string]any{"mount_id": mount.ID})
	writeJSON(w, http.StatusCreated, map[string]any{"entry": entry})
}

func (a *API) fileOperation(w http.ResponseWriter, r *http.Request) {
	service := a.deps.R2Service
	if service == nil || a.deps.Credentials == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "R2 file service is unavailable")
		return
	}
	var input struct {
		MountID     string `json:"mount_id"`
		Operation   string `json:"operation"`
		Source      string `json:"source"`
		Destination string `json:"destination,omitempty"`
		Overwrite   bool   `json:"overwrite,omitempty"`
	}
	if decodeJSON(w, r, &input) != nil {
		return
	}
	mount, ok := a.fileMount(w, r, input.MountID)
	if !ok {
		return
	}
	entry, err := service.ResolveWebDAVEntry(r.Context(), mount.ID, input.Source)
	if err != nil {
		writeFileError(w, err)
		return
	}
	internalSource, err := r2.WebDAVMountKey(mount.ID, input.Source)
	if err != nil {
		writeFileError(w, err)
		return
	}
	internalDestination := ""
	if input.Destination != "" {
		internalDestination, err = r2.WebDAVMountKey(mount.ID, input.Destination)
		if err != nil {
			writeFileError(w, err)
			return
		}
	}
	detail := map[string]any{"destination": input.Destination, "overwrite": input.Overwrite, "mount_id": mount.ID}
	jobPayload := r2.FileJobPayload{Source: internalSource, Destination: internalDestination, Overwrite: input.Overwrite}

	switch input.Operation {
	case "delete":
		if entry.Kind == r2.EntryFile {
			err = service.Delete(r.Context(), internalSource)
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
			err = service.MoveFile(r.Context(), internalSource, internalDestination, input.Overwrite)
			break
		}
		if err = service.ValidateDirectoryMove(r.Context(), internalSource, internalDestination, input.Overwrite); err != nil {
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
	case errors.Is(err, r2.ErrRangeNotSatisfiable):
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "range_not_satisfiable", "the requested byte range is outside the file")
	case errors.Is(err, r2.ErrBucketDeleting):
		writeError(w, http.StatusServiceUnavailable, "bucket_deleting", "存储桶正在删除，当前不能读取、写入或执行维护操作。")
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
