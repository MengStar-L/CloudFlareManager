package webdavprotocol

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
)

type Identity struct {
	ID     string
	Scopes []string
}

func (i Identity) hasScope(scope string) bool {
	for _, candidate := range i.Scopes {
		if candidate == scope || candidate == "r2:*" || candidate == "*" {
			return true
		}
	}
	return false
}

type Verifier func(context.Context, string, string) (Identity, error)

type ObjectService interface {
	Put(context.Context, r2.PutRequest) (r2.Object, error)
	Get(context.Context, string, r2.GetOptions) (r2.GetResult, error)
	Stat(context.Context, string) (r2.Object, error)
	List(context.Context, r2.ListOptions) (r2.ObjectList, error)
	Delete(context.Context, string) error
	Copy(context.Context, string, string) (r2.Object, error)
}

type Handler struct {
	Objects    ObjectService
	Locks      *LockStore
	Verify     Verifier
	lockPrefix string
}

func (h Handler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	username, password, ok := request.BasicAuth()
	if !ok || h.Verify == nil {
		h.requireAuthentication(w)
		return
	}
	identity, err := h.Verify(request.Context(), username, password)
	if err != nil {
		h.requireAuthentication(w)
		return
	}
	prefix := r2.WebDAVMountPrefix(identity.ID)
	h.Objects = scopedObjects{base: h.Objects, prefix: prefix}
	h.lockPrefix = prefix
	writeMethod := request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != "PROPFIND"
	if writeMethod && !identity.hasScope("r2:write") || !writeMethod && !identity.hasScope("r2:read") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	key, err := requestKey(request.URL.Path)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if writeMethod && request.Method != "LOCK" && request.Method != "UNLOCK" && h.Locks != nil {
		if err := h.Locks.Check(request.Context(), h.lockKey(key), extractLockToken(request.Header.Get("If"))); err != nil {
			w.WriteHeader(http.StatusLocked)
			return
		}
	}
	switch request.Method {
	case http.MethodGet:
		h.get(w, request, key, false)
	case http.MethodHead:
		h.get(w, request, key, true)
	case http.MethodPut:
		h.put(w, request, key)
	case http.MethodDelete:
		h.remove(w, request, key)
	case "PROPFIND":
		h.propfind(w, request, key)
	case "MKCOL":
		h.mkcol(w, request, key)
	case "COPY":
		h.copyMove(w, request, key, false)
	case "MOVE":
		h.copyMove(w, request, key, true)
	case "LOCK":
		h.lock(w, request, key)
	case "UNLOCK":
		h.unlock(w, request)
	case http.MethodOptions:
		w.Header().Set("Allow", "OPTIONS, PROPFIND, GET, HEAD, PUT, MKCOL, DELETE, COPY, MOVE, LOCK, UNLOCK")
		w.Header().Set("DAV", "1, 2")
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h Handler) get(w http.ResponseWriter, request *http.Request, key string, head bool) {
	if key == "" || strings.HasSuffix(key, "/") {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	result, err := h.Objects.Get(request.Context(), key, r2.GetOptions{Range: request.Header.Get("Range")})
	if err != nil {
		writeObjectStatus(w, err)
		return
	}
	defer result.Body.Close()
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
	if result.ContentType != "" {
		w.Header().Set("Content-Type", result.ContentType)
	}
	if result.ETag != "" {
		w.Header().Set("ETag", `"`+strings.Trim(result.ETag, `"`)+`"`)
	}
	if !result.LastModified.IsZero() {
		w.Header().Set("Last-Modified", result.LastModified.UTC().Format(http.TimeFormat))
	}
	if result.ContentRange != "" {
		w.Header().Set("Content-Range", result.ContentRange)
		w.WriteHeader(http.StatusPartialContent)
	}
	if !head {
		_, _ = io.Copy(w, result.Body)
	}
}

func (h Handler) put(w http.ResponseWriter, request *http.Request, key string) {
	if key == "" || strings.HasSuffix(key, "/") {
		w.WriteHeader(http.StatusConflict)
		return
	}
	_, existingErr := h.Objects.Stat(request.Context(), key)
	object, err := h.Objects.Put(request.Context(), r2.PutRequest{
		Key: key, Body: request.Body, Size: request.ContentLength, ContentType: request.Header.Get("Content-Type"),
		Metadata: map[string]string{"webdav": "true"}, PayloadHash: "UNSIGNED-PAYLOAD",
	})
	if err != nil {
		writeObjectStatus(w, err)
		return
	}
	w.Header().Set("ETag", `"`+strings.Trim(object.ETag, `"`)+`"`)
	if errors.Is(existingErr, r2.ErrObjectNotFound) {
		w.WriteHeader(http.StatusCreated)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h Handler) mkcol(w http.ResponseWriter, request *http.Request, key string) {
	if request.ContentLength > 0 {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	if key == "" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	key = strings.TrimSuffix(key, "/") + "/"
	if _, err := h.Objects.Put(request.Context(), r2.PutRequest{
		Key: key, Body: strings.NewReader(""), Size: 0, ContentType: "httpd/unix-directory",
		Metadata: map[string]string{"webdav-directory": "true"}, PayloadHash: "UNSIGNED-PAYLOAD",
	}); err != nil {
		writeObjectStatus(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h Handler) remove(w http.ResponseWriter, request *http.Request, key string) {
	if key == "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if err := h.deleteTree(request.Context(), key); err != nil {
		writeObjectStatus(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h Handler) propfind(w http.ResponseWriter, request *http.Request, key string) {
	depth := request.Header.Get("Depth")
	if depth == "infinity" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	entries, err := h.properties(request.Context(), key, depth != "0")
	if err != nil {
		writeObjectStatus(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(multistatus{XMLNS: "DAV:", Responses: entries})
}

func (h Handler) properties(ctx context.Context, key string, includeChildren bool) ([]propertyResponse, error) {
	var object r2.Object
	directory := key == ""
	if !directory {
		var err error
		object, err = h.Objects.Stat(ctx, key)
		if errors.Is(err, r2.ErrObjectNotFound) {
			prefix := strings.TrimSuffix(key, "/") + "/"
			children, listErr := h.Objects.List(ctx, r2.ListOptions{Prefix: prefix, Limit: 1})
			if listErr != nil {
				return nil, listErr
			}
			if len(children.Objects) == 0 {
				return nil, r2.ErrObjectNotFound
			}
			key, directory = prefix, true
		} else if err != nil {
			return nil, err
		} else {
			directory = strings.HasSuffix(object.Key, "/") || object.Metadata["webdav-directory"] == "true"
		}
	}
	responses := []propertyResponse{makeProperty(key, object, directory)}
	if !includeChildren || !directory {
		return responses, nil
	}
	prefix := key
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	seen := make(map[string]bool)
	var after string
	for {
		list, err := h.Objects.List(ctx, r2.ListOptions{Prefix: prefix, After: after, Limit: 1000})
		if err != nil {
			return nil, err
		}
		for _, child := range list.Objects {
			relative := strings.TrimPrefix(child.Key, prefix)
			name, rest, nested := strings.Cut(relative, "/")
			childKey := prefix + name
			isDirectory := nested
			if isDirectory {
				childKey += "/"
			}
			if name == "" || seen[childKey] {
				continue
			}
			seen[childKey] = true
			if isDirectory && rest != "" {
				responses = append(responses, makeProperty(childKey, r2.Object{}, true))
			} else {
				responses = append(responses, makeProperty(childKey, child, strings.HasSuffix(child.Key, "/")))
			}
		}
		if list.NextMarker == "" {
			break
		}
		after = list.NextMarker
	}
	return responses, nil
}

func (h Handler) copyMove(w http.ResponseWriter, request *http.Request, source string, move bool) {
	destinationHeader := request.Header.Get("Destination")
	if destinationHeader == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	destinationURL, err := url.Parse(destinationHeader)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	destination, err := requestKey(destinationURL.Path)
	if err != nil || destination == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if request.Header.Get("Overwrite") == "F" {
		if _, err := h.Objects.Stat(request.Context(), destination); err == nil {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}
	if err := h.copyTree(request.Context(), source, destination); err != nil {
		writeObjectStatus(w, err)
		return
	}
	if move {
		if err := h.deleteTree(request.Context(), source); err != nil {
			writeObjectStatus(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusCreated)
}

func (h Handler) copyTree(ctx context.Context, source, destination string) error {
	if _, err := h.Objects.Stat(ctx, source); err == nil {
		_, err = h.Objects.Copy(ctx, source, destination)
		return err
	}
	prefix := strings.TrimSuffix(source, "/") + "/"
	after := ""
	copied := false
	for {
		list, err := h.Objects.List(ctx, r2.ListOptions{Prefix: prefix, After: after, Limit: 1000})
		if err != nil {
			return err
		}
		for _, object := range list.Objects {
			target := strings.TrimSuffix(destination, "/") + "/" + strings.TrimPrefix(object.Key, prefix)
			if _, err := h.Objects.Copy(ctx, object.Key, target); err != nil {
				return err
			}
			copied = true
		}
		if list.NextMarker == "" {
			break
		}
		after = list.NextMarker
	}
	if !copied {
		return r2.ErrObjectNotFound
	}
	return nil
}

func (h Handler) deleteTree(ctx context.Context, key string) error {
	directory := strings.HasSuffix(key, "/")
	if object, err := h.Objects.Stat(ctx, key); err == nil {
		directory = directory || object.Metadata["webdav-directory"] == "true"
		if !directory {
			return h.Objects.Delete(ctx, key)
		}
	} else if !errors.Is(err, r2.ErrObjectNotFound) {
		return err
	}
	prefix := strings.TrimSuffix(key, "/") + "/"
	after := ""
	deleted := false
	for {
		list, err := h.Objects.List(ctx, r2.ListOptions{Prefix: prefix, After: after, Limit: 1000})
		if err != nil {
			return err
		}
		for _, object := range list.Objects {
			if err := h.Objects.Delete(ctx, object.Key); err != nil {
				return err
			}
			deleted = true
		}
		if list.NextMarker == "" {
			break
		}
		after = list.NextMarker
	}
	if err := h.Objects.Delete(ctx, key); err == nil {
		deleted = true
	} else if !errors.Is(err, r2.ErrObjectNotFound) {
		return err
	}
	if !deleted {
		return r2.ErrObjectNotFound
	}
	return nil
}

func (h Handler) lock(w http.ResponseWriter, request *http.Request, key string) {
	if h.Locks == nil {
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	ttl := parseTimeout(request.Header.Get("Timeout"))
	if token := extractLockToken(request.Header.Get("If")); token != "" {
		lock, err := h.Locks.Refresh(request.Context(), token, ttl)
		if err != nil {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		h.writeLock(w, lock)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(request.Body, 64<<10))
	lock, err := h.Locks.Create(request.Context(), h.lockKey(key), string(body), request.Header.Get("Depth"), ttl)
	if err != nil {
		w.WriteHeader(http.StatusLocked)
		return
	}
	h.writeLock(w, lock)
}

func (h Handler) lockKey(key string) string {
	return h.lockPrefix + key
}

func (h Handler) writeLock(w http.ResponseWriter, lock Lock) {
	w.Header().Set("Lock-Token", "<"+lock.Token+">")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(lockDiscovery{XMLNS: "DAV:", Token: lock.Token, Depth: lock.Depth, Timeout: "Second-" + strconv.Itoa(int(time.Until(lock.ExpiresAt).Seconds()))})
}

func (h Handler) unlock(w http.ResponseWriter, request *http.Request) {
	if h.Locks == nil || h.Locks.Delete(request.Context(), strings.Trim(request.Header.Get("Lock-Token"), "<>")) != nil {
		w.WriteHeader(http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h Handler) requireAuthentication(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="CF-R2Manager WebDAV", charset="UTF-8"`)
	w.WriteHeader(http.StatusUnauthorized)
}

func requestKey(value string) (string, error) {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return "", err
	}
	trailing := strings.HasSuffix(decoded, "/")
	cleaned := path.Clean("/" + decoded)
	if cleaned == "/" {
		return "", nil
	}
	key := strings.TrimPrefix(cleaned, "/")
	if trailing {
		key += "/"
	}
	return key, nil
}

func extractLockToken(value string) string {
	start := strings.Index(value, "<opaquelocktoken:")
	if start < 0 {
		return ""
	}
	end := strings.Index(value[start:], ">")
	if end < 0 {
		return ""
	}
	return value[start+1 : start+end]
}

func parseTimeout(value string) time.Duration {
	if strings.HasPrefix(value, "Second-") {
		seconds, _ := strconv.Atoi(strings.TrimPrefix(value, "Second-"))
		if seconds > 0 && seconds <= 3600 {
			return time.Duration(seconds) * time.Second
		}
	}
	return time.Hour
}

func writeObjectStatus(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, r2.ErrObjectNotFound):
		w.WriteHeader(http.StatusNotFound)
	case errors.Is(err, r2.ErrQuotaExceeded):
		w.WriteHeader(http.StatusInsufficientStorage)
	case errors.Is(err, r2.ErrWriteInProgress):
		w.WriteHeader(http.StatusLocked)
	default:
		w.WriteHeader(http.StatusBadGateway)
	}
}

func makeProperty(key string, object r2.Object, directory bool) propertyResponse {
	href := "/" + strings.TrimPrefix(key, "/")
	if directory && !strings.HasSuffix(href, "/") {
		href += "/"
	}
	resourceType := resourceType{}
	if directory {
		resourceType.Collection = &struct{}{}
	}
	displayName := path.Base(strings.TrimSuffix(href, "/"))
	if href == "/" {
		displayName = "/"
	}
	lastModified := ""
	if !object.LastModified.IsZero() {
		lastModified = object.LastModified.UTC().Format(http.TimeFormat)
	}
	return propertyResponse{
		Href: href,
		PropStat: propertyStat{Properties: properties{
			DisplayName: displayName, ResourceType: resourceType,
			ContentLength: object.Size, LastModified: lastModified, ETag: object.ETag,
		}, Status: "HTTP/1.1 200 OK"},
	}
}

type multistatus struct {
	XMLName   xml.Name           `xml:"multistatus"`
	XMLNS     string             `xml:"xmlns,attr"`
	Responses []propertyResponse `xml:"response"`
}

type propertyResponse struct {
	Href     string       `xml:"href"`
	PropStat propertyStat `xml:"propstat"`
}

type propertyStat struct {
	Properties properties `xml:"prop"`
	Status     string     `xml:"status"`
}

type properties struct {
	DisplayName   string       `xml:"displayname"`
	ResourceType  resourceType `xml:"resourcetype"`
	ContentLength int64        `xml:"getcontentlength,omitempty"`
	LastModified  string       `xml:"getlastmodified,omitempty"`
	ETag          string       `xml:"getetag,omitempty"`
}

type resourceType struct {
	Collection *struct{} `xml:"collection,omitempty"`
}

type lockDiscovery struct {
	XMLName xml.Name `xml:"prop"`
	XMLNS   string   `xml:"xmlns,attr"`
	Token   string   `xml:"lockdiscovery>activelock>locktoken>href"`
	Depth   string   `xml:"lockdiscovery>activelock>depth"`
	Timeout string   `xml:"lockdiscovery>activelock>timeout"`
}
