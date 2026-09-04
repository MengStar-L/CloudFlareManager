package webdavprotocol

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
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
	PutConditional(context.Context, r2.PutRequest) (r2.PutResult, error)
	Get(context.Context, string, r2.GetOptions) (r2.GetResult, error)
	Stat(context.Context, string) (r2.Object, error)
	List(context.Context, r2.ListOptions) (r2.ObjectList, error)
	DeleteConditional(context.Context, string, r2.MutationConditions) error
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
		h.unlock(w, request, key)
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
	for attempt := 0; attempt < 2; attempt++ {
		prepared, err := h.prepareConditions(request, key)
		if err != nil {
			writeConditionError(w, err)
			return
		}
		if writeConditionResult(w, prepared) {
			return
		}
		rangeHeader := request.Header.Get("Range")
		if head || prepared.evaluation.IgnoreRange {
			rangeHeader = ""
		}
		selected := prepared.states[prepared.parsed.requestResource]
		options := r2.GetOptions{Range: rangeHeader, ExpectedObjectID: selected.ObjectID}
		if etag := strongETag(selected.ETag); etag != "" {
			options.IfMatch = etag
		}
		result, err := h.Objects.Get(request.Context(), key, options)
		if errors.Is(err, r2.ErrConditionalRequestConflict) && attempt == 0 {
			continue
		}
		if err != nil {
			if rangeHeader != "" && errors.Is(err, r2.ErrRangeNotSatisfiable) && selected.Size >= 0 {
				w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(selected.Size, 10))
			}
			writeObjectStatus(w, err)
			return
		}
		defer result.Body.Close()
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(result.Size, 10))
		if result.ContentType != "" {
			w.Header().Set("Content-Type", result.ContentType)
		}
		etag := strongETag(selected.ETag)
		if etag == "" {
			etag = strongETag(result.ETag)
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		lastModified := selected.LastModified
		if lastModified.IsZero() {
			lastModified = result.LastModified
		}
		if !lastModified.IsZero() {
			w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
		}
		if result.ContentRange != "" {
			w.Header().Set("Content-Range", result.ContentRange)
			w.WriteHeader(http.StatusPartialContent)
		}
		if !head {
			_, _ = io.Copy(w, result.Body)
		}
		return
	}
}

func (h Handler) put(w http.ResponseWriter, request *http.Request, key string) {
	if key == "" || strings.HasSuffix(key, "/") {
		w.WriteHeader(http.StatusConflict)
		return
	}
	prepared, err := h.prepareConditions(request, key)
	if err != nil {
		writeConditionError(w, err)
		return
	}
	if writeConditionResult(w, prepared) {
		return
	}
	state := prepared.states[prepared.parsed.requestResource]
	if state.Exists {
		object, statErr := h.Objects.Stat(request.Context(), key)
		switch {
		case statErr == nil:
			if strings.HasSuffix(object.Key, "/") || object.Metadata["webdav-directory"] == "true" {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
		case errors.Is(statErr, r2.ErrObjectNotFound):
			// A resource that exists only through descendants is a collection.
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		default:
			writeObjectStatus(w, statErr)
			return
		}
	}
	affected := []string{key}
	if !state.Exists {
		affected, err = h.creationMutationPaths(request.Context(), key)
		if err != nil {
			writeObjectStatus(w, err)
			return
		}
	}
	guard, stopped := h.beginMutation(w, request, affected, &prepared)
	if stopped {
		return
	}
	if guard != nil {
		defer guard.Release()
	}
	currentState := prepared.states[prepared.parsed.requestResource]
	if currentState.Collection {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if state.Exists && !currentState.Exists {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if !currentState.Exists {
		required, pathErr := h.creationMutationPaths(request.Context(), key)
		if pathErr != nil {
			writeObjectStatus(w, pathErr)
			return
		}
		if !containsAllPaths(affected, required) {
			w.WriteHeader(http.StatusConflict)
			return
		}
	}
	result, err := h.Objects.PutConditional(request.Context(), r2.PutRequest{
		Key: key, Body: request.Body, Size: request.ContentLength, ContentType: request.Header.Get("Content-Type"),
		Metadata: map[string]string{"webdav": "true"}, PayloadHash: "UNSIGNED-PAYLOAD", Conditions: prepared.requestMutationConditions(),
	})
	if err != nil {
		writeObjectStatus(w, err)
		return
	}
	if etag := strongETag(result.Object.ETag); etag != "" {
		w.Header().Set("ETag", etag)
	}
	if result.Created {
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
	if request.ContentLength < 0 {
		var probe [1]byte
		read, err := request.Body.Read(probe[:])
		if read != 0 {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		if err != nil && !errors.Is(err, io.EOF) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	if key == "" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	key = strings.TrimSuffix(key, "/") + "/"
	prepared, err := h.prepareConditions(request, key)
	if err != nil {
		writeConditionError(w, err)
		return
	}
	if writeConditionResult(w, prepared) {
		return
	}
	state := prepared.states[prepared.parsed.requestResource]
	if state.Exists {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, err := h.Objects.Stat(request.Context(), strings.TrimSuffix(key, "/")); err == nil {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	} else if !errors.Is(err, r2.ErrObjectNotFound) {
		writeObjectStatus(w, err)
		return
	}
	affected, err := h.creationMutationPaths(request.Context(), key)
	if err != nil {
		writeObjectStatus(w, err)
		return
	}
	guard, stopped := h.beginMutation(w, request, affected, &prepared)
	if stopped {
		return
	}
	if guard != nil {
		defer guard.Release()
	}
	currentState := prepared.states[prepared.parsed.requestResource]
	if currentState.Exists {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	required, pathErr := h.creationMutationPaths(request.Context(), key)
	if pathErr != nil {
		writeObjectStatus(w, pathErr)
		return
	}
	if !containsAllPaths(affected, required) {
		w.WriteHeader(http.StatusConflict)
		return
	}
	conditions := prepared.requestMutationConditions()
	conditions.IfNoneMatch = &r2.EntityTagSet{Wildcard: true}
	if _, err := h.Objects.PutConditional(request.Context(), r2.PutRequest{
		Key: key, Body: strings.NewReader(""), Size: 0, ContentType: "httpd/unix-directory",
		Metadata: map[string]string{"webdav-directory": "true"}, PayloadHash: "UNSIGNED-PAYLOAD", Conditions: conditions,
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
	prepared, err := h.prepareConditions(request, key)
	if err != nil {
		writeConditionError(w, err)
		return
	}
	if writeConditionResult(w, prepared) {
		return
	}
	objects, err := h.treeObjects(request.Context(), key)
	if err != nil {
		writeObjectStatus(w, err)
		return
	}
	collection := treeIsCollection(key, objects)
	if collection {
		depth := request.Header.Get("Depth")
		if depth != "" && depth != "infinity" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	affected, err := h.deletionMutationPaths(request.Context(), key, objects, collection)
	if err != nil {
		writeObjectStatus(w, err)
		return
	}
	guard, stopped := h.beginMutation(w, request, affected, &prepared)
	if stopped {
		return
	}
	if guard != nil {
		defer guard.Release()
	}
	currentAffected, err := h.deletionMutationPaths(request.Context(), key, objects, collection)
	if err != nil {
		writeObjectStatus(w, err)
		return
	}
	if !containsAllPaths(affected, currentAffected) {
		w.WriteHeader(http.StatusConflict)
		return
	}
	currentObjects, err := h.treeObjects(request.Context(), key)
	if err != nil || !sameTreeSnapshot(objects, currentObjects) {
		if err != nil && !errors.Is(err, r2.ErrObjectNotFound) {
			writeObjectStatus(w, err)
		} else {
			w.WriteHeader(http.StatusConflict)
		}
		return
	}
	failures, deleted := h.deleteObjects(request.Context(), key, objects, prepared.requestMutationConditions())
	if err := h.deleteUnmappedLocks(request.Context(), deleted, guard); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if len(failures) != 0 {
		if !collection && len(objects) == 1 {
			w.WriteHeader(failures[0].Code)
		} else {
			writeOperationMultistatus(w, failures)
		}
		return
	}
	if err := h.deleteLocks(request.Context(), key, guard); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h Handler) propfind(w http.ResponseWriter, request *http.Request, key string) {
	prepared, err := h.prepareConditions(request, key)
	if err != nil {
		writeConditionError(w, err)
		return
	}
	if writeConditionResult(w, prepared) {
		return
	}
	depth := request.Header.Get("Depth")
	if depth == "infinity" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if depth != "" && depth != "0" && depth != "1" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	entries, err := h.properties(request.Context(), key, depth != "0")
	if err != nil {
		writeObjectStatus(w, err)
		return
	}
	if err := h.decorateLockProperties(request.Context(), request, entries); err != nil {
		w.WriteHeader(http.StatusBadGateway)
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
			marker, markerErr := h.Objects.Stat(ctx, prefix)
			if markerErr == nil {
				object, key, directory = marker, prefix, true
			} else if !errors.Is(markerErr, r2.ErrObjectNotFound) {
				return nil, markerErr
			} else {
				children, listErr := h.Objects.List(ctx, r2.ListOptions{Prefix: prefix, Limit: 1})
				if listErr != nil {
					return nil, listErr
				}
				if len(children.Objects) == 0 {
					return nil, r2.ErrObjectNotFound
				}
				key, directory = prefix, true
			}
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
				if strongETag(child.ETag) == "" {
					repaired, statErr := h.Objects.Stat(ctx, child.Key)
					if errors.Is(statErr, r2.ErrObjectNotFound) {
						continue
					}
					if statErr != nil {
						return nil, statErr
					}
					child = repaired
				}
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
	if err != nil || destinationURL.Fragment != "" || destinationURL.User != nil || destinationURL.RawQuery != "" || destinationURL.ForceQuery {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if destinationURL.IsAbs() {
		if (destinationURL.Scheme != "http" && destinationURL.Scheme != "https") || !strings.EqualFold(destinationURL.Host, request.Host) ||
			request.URL.Scheme != "" && !strings.EqualFold(destinationURL.Scheme, request.URL.Scheme) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
	} else if destinationURL.Host != "" || !strings.HasPrefix(destinationURL.Path, "/") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	destination, err := requestKey(destinationURL.Path)
	if err != nil || destination == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	overwrite := request.Header.Get("Overwrite")
	if overwrite != "" && overwrite != "T" && overwrite != "F" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if strings.TrimSuffix(source, "/") == strings.TrimSuffix(destination, "/") {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	prepared, err := h.prepareConditions(request, source)
	if err != nil {
		writeConditionError(w, err)
		return
	}
	if writeConditionResult(w, prepared) {
		return
	}
	objects, err := h.treeObjects(request.Context(), source)
	if err != nil {
		writeObjectStatus(w, err)
		return
	}
	sourceSnapshot := append([]r2.Object(nil), objects...)
	collection := treeIsCollection(source, objects)
	if !collection && strings.HasSuffix(destination, "/") {
		w.WriteHeader(http.StatusConflict)
		return
	}
	rootMapped := false
	sourceState := conditionState{Exists: true, Collection: collection}
	for _, object := range objects {
		if strings.TrimSuffix(object.Key, "/") == strings.TrimSuffix(source, "/") {
			rootMapped = true
			sourceState.ETag = object.ETag
			sourceState.LastModified = object.LastModified
			break
		}
	}
	if err := prepared.reevaluateCurrent(request.Method, sourceState); err != nil {
		writeConditionError(w, err)
		return
	}
	if writeConditionResult(w, prepared) {
		return
	}
	depth := request.Header.Get("Depth")
	if collection {
		if move {
			if depth != "" && depth != "infinity" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		} else {
			if depth == "" {
				depth = "infinity"
			}
			if depth != "0" && depth != "infinity" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if depth == "0" {
				rootObjects := objects[:0]
				for _, object := range objects {
					if strings.TrimSuffix(object.Key, "/") == strings.TrimSuffix(source, "/") {
						rootObjects = append(rootObjects, object)
					}
				}
				objects = rootObjects
			}
		}
	}
	syntheticCollectionMarker := collection && !rootMapped
	sourcePrefix := strings.TrimSuffix(source, "/") + "/"
	destinationPrefix := strings.TrimSuffix(destination, "/") + "/"
	if strings.HasPrefix(sourcePrefix, destinationPrefix) || strings.HasPrefix(destinationPrefix, sourcePrefix) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	destinationResource, err := canonicalConditionResource(destinationURL)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	destinationState, ok := prepared.states[destinationResource]
	if !ok {
		destinationState, err = h.conditionState(request.Context(), destination, prepared.parsed.referencedLockTokens())
		if err != nil {
			writeObjectStatus(w, err)
			return
		}
		prepared.states[destinationResource] = destinationState
	}
	if overwrite == "F" && destinationState.Exists {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	var destinationObjects []r2.Object
	if destinationState.Exists {
		destinationObjects, err = h.treeObjects(request.Context(), destination)
		if errors.Is(err, r2.ErrObjectNotFound) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if err != nil {
			writeObjectStatus(w, err)
			return
		}
	}

	targets := make([]string, len(objects))
	for index, object := range objects {
		targets[index] = copyTarget(source, destination, object.Key, collection)
	}
	targetKeys := append([]string(nil), targets...)
	if syntheticCollectionMarker {
		targetKeys = append(targetKeys, destinationPrefix)
	}
	directReplace := !collection && destinationState.Exists && len(destinationObjects) == 1 && !treeIsCollection(destination, destinationObjects)
	destinationAffected, err := h.copyDestinationMutationPaths(request.Context(), destination, destinationState.Exists, destinationObjects, directReplace, targetKeys)
	if err != nil {
		writeObjectStatus(w, err)
		return
	}
	lockAffected := uniquePaths(destinationAffected)
	if move {
		var sourceAffected []string
		sourceAffected, err = h.deletionMutationPaths(request.Context(), source, sourceSnapshot, collection)
		if err != nil {
			writeObjectStatus(w, err)
			return
		}
		lockAffected = uniquePaths(append(lockAffected, sourceAffected...))
	}
	serialAffected := uniquePaths(append(append([]string(nil), lockAffected...), source))
	guard, stopped := h.beginMutationWithDependencies(w, request, lockAffected, serialAffected, &prepared)
	if stopped {
		return
	}
	if guard != nil {
		defer guard.Release()
	}
	currentSource, snapshotErr := h.treeObjects(request.Context(), source)
	if snapshotErr != nil || !sameTreeSnapshot(sourceSnapshot, currentSource) {
		if snapshotErr != nil && !errors.Is(snapshotErr, r2.ErrObjectNotFound) {
			writeObjectStatus(w, snapshotErr)
		} else {
			w.WriteHeader(http.StatusConflict)
		}
		return
	}
	currentDestinationState := prepared.states[destinationResource]
	if overwrite == "F" && currentDestinationState.Exists {
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	if currentDestinationState.Exists != destinationState.Exists {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if destinationState.Exists {
		currentDestination, snapshotErr := h.treeObjects(request.Context(), destination)
		if snapshotErr != nil || !sameTreeSnapshot(destinationObjects, currentDestination) {
			if snapshotErr != nil && !errors.Is(snapshotErr, r2.ErrObjectNotFound) {
				writeObjectStatus(w, snapshotErr)
			} else {
				w.WriteHeader(http.StatusConflict)
			}
			return
		}
	} else if _, snapshotErr := h.treeObjects(request.Context(), destination); !errors.Is(snapshotErr, r2.ErrObjectNotFound) {
		if snapshotErr != nil {
			writeObjectStatus(w, snapshotErr)
		} else {
			w.WriteHeader(http.StatusConflict)
		}
		return
	}
	requiredAffected, pathErr := h.copyDestinationMutationPaths(request.Context(), destination, destinationState.Exists, destinationObjects, directReplace, targetKeys)
	if pathErr == nil && move {
		var sourceRequired []string
		sourceRequired, pathErr = h.deletionMutationPaths(request.Context(), source, sourceSnapshot, collection)
		requiredAffected = append(requiredAffected, sourceRequired...)
	}
	if pathErr != nil {
		writeObjectStatus(w, pathErr)
		return
	}
	if !containsAllPaths(lockAffected, uniquePaths(requiredAffected)) {
		w.WriteHeader(http.StatusConflict)
		return
	}
	destinationConditions := prepared.mutationConditions(destinationResource, false)
	if overwrite == "F" {
		destinationConditions.IfNoneMatch = &r2.EntityTagSet{Wildcard: true}
	}
	var failures []operationResponse
	if destinationState.Exists && !directReplace {
		var deleted []string
		failures, deleted = h.deleteObjects(request.Context(), destination, destinationObjects, destinationConditions)
		if err := h.deleteUnmappedLocks(request.Context(), deleted, guard); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if len(failures) != 0 {
			if len(destinationObjects) == 1 {
				w.WriteHeader(failures[0].Code)
			} else {
				writeOperationMultistatus(w, failures)
			}
			return
		}
		if err := h.deleteLocks(request.Context(), destination, guard); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		destinationConditions = r2.MutationConditions{IfNoneMatch: &r2.EntityTagSet{Wildcard: true}}
	} else if !destinationState.Exists {
		destinationConditions = r2.MutationConditions{IfNoneMatch: &r2.EntityTagSet{Wildcard: true}}
	}
	operationCount := len(objects)
	if syntheticCollectionMarker {
		operationCount++
		if _, err := h.putCollectionMarker(request.Context(), destinationPrefix, destinationConditions); err != nil {
			if operationCount == 1 {
				writeObjectStatus(w, err)
				return
			}
			failures = append(failures, operationFailure(destinationPrefix, err))
		}
	}
	copied := make([]bool, len(objects))
	for index, object := range objects {
		conditions := r2.MutationConditions{IfNoneMatch: &r2.EntityTagSet{Wildcard: true}}
		if strings.TrimSuffix(object.Key, "/") == strings.TrimSuffix(source, "/") {
			conditions = destinationConditions
		}
		if _, err := h.copyObject(request.Context(), object, targets[index], conditions); err != nil {
			if operationCount == 1 {
				writeObjectStatus(w, err)
				return
			}
			failures = append(failures, operationFailure(targets[index], err))
			continue
		}
		copied[index] = true
	}
	if directReplace && copied[0] {
		if err := h.deleteLocks(request.Context(), destination, guard); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
	}
	if move {
		var deleted []string
		for index := len(objects) - 1; index >= 0; index-- {
			if !copied[index] {
				continue
			}
			if err := h.Objects.DeleteConditional(request.Context(), objects[index].Key, objectFence(objects[index])); err != nil {
				if !collection && len(objects) == 1 {
					writeObjectStatus(w, err)
					return
				}
				failures = append(failures, operationFailure(objects[index].Key, err))
				continue
			}
			deleted = append(deleted, objects[index].Key)
		}
		if err := h.deleteUnmappedLocks(request.Context(), deleted, guard); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
	}
	if len(failures) != 0 {
		writeOperationMultistatus(w, failures)
		return
	}
	if move {
		if err := h.deleteLocks(request.Context(), source, guard); err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
	}
	if destinationState.Exists {
		w.WriteHeader(http.StatusNoContent)
	} else {
		w.WriteHeader(http.StatusCreated)
	}
}

func copyTarget(source, destination, objectKey string, collection bool) string {
	if !collection {
		return destination
	}
	sourcePrefix := strings.TrimSuffix(source, "/") + "/"
	destinationPrefix := strings.TrimSuffix(destination, "/") + "/"
	if strings.TrimSuffix(objectKey, "/") == strings.TrimSuffix(source, "/") {
		return destinationPrefix
	}
	return destinationPrefix + strings.TrimPrefix(objectKey, sourcePrefix)
}

func (h Handler) copyObject(ctx context.Context, source r2.Object, destination string, conditions r2.MutationConditions) (r2.PutResult, error) {
	options := r2.GetOptions{ExpectedObjectID: source.ObjectID}
	if etag := strongETag(source.ETag); etag != "" {
		options.IfMatch = etag
	}
	result, err := h.Objects.Get(ctx, source.Key, options)
	if err != nil {
		return r2.PutResult{}, err
	}
	defer result.Body.Close()
	return h.Objects.PutConditional(ctx, r2.PutRequest{
		Key: destination, Body: result.Body, Size: source.Size, ContentType: source.ContentType,
		Metadata: source.Metadata, PayloadHash: "UNSIGNED-PAYLOAD", Conditions: conditions,
	})
}

func (h Handler) putCollectionMarker(ctx context.Context, key string, conditions r2.MutationConditions) (r2.PutResult, error) {
	return h.Objects.PutConditional(ctx, r2.PutRequest{
		Key: key, Body: strings.NewReader(""), Size: 0, ContentType: "httpd/unix-directory",
		Metadata: map[string]string{"webdav-directory": "true"}, PayloadHash: "UNSIGNED-PAYLOAD", Conditions: conditions,
	})
}

func treeIsCollection(key string, objects []r2.Object) bool {
	if strings.HasSuffix(key, "/") {
		return true
	}
	root := strings.TrimSuffix(key, "/")
	for _, object := range objects {
		if strings.TrimSuffix(object.Key, "/") != root {
			continue
		}
		return strings.HasSuffix(object.Key, "/") || object.Metadata["webdav-directory"] == "true"
	}
	return len(objects) != 0
}

func parentKey(key string) string {
	key = strings.TrimSuffix(key, "/")
	separator := strings.LastIndex(key, "/")
	if separator < 0 {
		return ""
	}
	return key[:separator+1]
}

// mutationPaths includes each changed resource and its direct parent, whose
// collection membership changes when that resource is created or removed.
func mutationPaths(keys ...string) []string {
	seen := make(map[string]struct{})
	paths := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		for _, candidate := range []string{key, parentKey(key)} {
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			paths = append(paths, candidate)
		}
	}
	return paths
}

func (h Handler) creationMutationPaths(ctx context.Context, key string) ([]string, error) {
	paths := []string{key}
	parent := parentKey(key)
	for {
		paths = append(paths, parent)
		if parent == "" {
			break
		}
		state, err := h.conditionState(ctx, parent, nil)
		if err != nil {
			return nil, err
		}
		if state.Exists {
			break
		}
		parent = parentKey(parent)
	}
	return uniquePaths(paths), nil
}

func (h Handler) deletionMutationPaths(ctx context.Context, root string, objects []r2.Object, collection bool) ([]string, error) {
	keys := make([]string, 0, len(objects)+1)
	keys = append(keys, root)
	for _, object := range objects {
		keys = append(keys, object.Key)
	}
	paths := mutationPaths(keys...)
	if collection {
		rootPath := strings.TrimSuffix(root, "/") + "/"
		for _, object := range objects {
			for parent := parentKey(object.Key); parent != "" && pathAtOrBelow(parent, rootPath); parent = parentKey(parent) {
				paths = append(paths, parent)
				if sameLockPath(parent, rootPath) {
					break
				}
			}
		}
	}

	deleting := make(map[string]struct{}, len(objects))
	for _, object := range objects {
		deleting[object.Key] = struct{}{}
	}
	for parent := parentKey(root); parent != ""; parent = parentKey(parent) {
		remains, err := h.collectionRemainsAfterDelete(ctx, parent, deleting)
		if err != nil {
			return nil, err
		}
		if remains {
			break
		}
		paths = append(paths, parent, parentKey(parent))
	}
	return uniquePaths(paths), nil
}

func (h Handler) collectionRemainsAfterDelete(ctx context.Context, key string, deleting map[string]struct{}) (bool, error) {
	if object, err := h.Objects.Stat(ctx, key); err == nil {
		if _, removed := deleting[object.Key]; !removed && (strings.HasSuffix(object.Key, "/") || object.Metadata["webdav-directory"] == "true") {
			return true, nil
		}
	} else if !errors.Is(err, r2.ErrObjectNotFound) {
		return false, err
	}
	after := ""
	for {
		list, err := h.Objects.List(ctx, r2.ListOptions{Prefix: strings.TrimSuffix(key, "/") + "/", After: after, Limit: 1000})
		if err != nil {
			return false, err
		}
		for _, object := range list.Objects {
			if _, removed := deleting[object.Key]; !removed {
				return true, nil
			}
		}
		if list.NextMarker == "" {
			return false, nil
		}
		after = list.NextMarker
	}
}

func subtreeMutationPaths(root string, keys ...string) []string {
	paths := mutationPaths(append([]string{root}, keys...)...)
	rootPath := strings.TrimSuffix(root, "/") + "/"
	for _, key := range keys {
		for parent := parentKey(key); parent != "" && pathAtOrBelow(parent, rootPath); parent = parentKey(parent) {
			paths = append(paths, parent)
			if sameLockPath(parent, rootPath) {
				break
			}
		}
	}
	return uniquePaths(paths)
}

func (h Handler) copyDestinationMutationPaths(ctx context.Context, destination string, exists bool, objects []r2.Object, directReplace bool, targets []string) ([]string, error) {
	if directReplace {
		return []string{destination}, nil
	}
	var affected []string
	if exists {
		paths, err := h.deletionMutationPaths(ctx, destination, objects, treeIsCollection(destination, objects))
		if err != nil {
			return nil, err
		}
		affected = append(affected, paths...)
		affected = append(affected, subtreeMutationPaths(destination, targets...)...)
		return uniquePaths(affected), nil
	}
	for _, target := range targets {
		paths, err := h.creationMutationPaths(ctx, target)
		if err != nil {
			return nil, err
		}
		affected = append(affected, paths...)
	}
	return uniquePaths(affected), nil
}

func uniquePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, key := range paths {
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result
}

func containsAllPaths(registered, required []string) bool {
	for _, candidate := range required {
		found := false
		for _, existing := range registered {
			if sameLockPath(candidate, existing) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sameTreeSnapshot(expected, current []r2.Object) bool {
	if len(expected) != len(current) {
		return false
	}
	byKey := make(map[string]r2.Object, len(current))
	for _, object := range current {
		byKey[object.Key] = object
	}
	for _, object := range expected {
		actual, ok := byKey[object.Key]
		if !ok || actual.ObjectID != object.ObjectID || actual.ETag != object.ETag || actual.Size != object.Size ||
			actual.ContentType != object.ContentType || !actual.LastModified.Equal(object.LastModified) {
			return false
		}
	}
	return true
}

func (h Handler) deleteLocks(ctx context.Context, key string, guard *lockMutationGuard) error {
	if h.Locks == nil {
		return nil
	}
	roots := []string{strings.TrimSuffix(h.lockKey(key), "/")}
	if guard != nil {
		return guard.DeletePaths(ctx, roots)
	}
	return h.Locks.DeletePaths(ctx, roots)
}

func (h Handler) deleteUnmappedLocks(ctx context.Context, deleted []string, guard *lockMutationGuard) error {
	if h.Locks == nil || len(deleted) == 0 {
		return nil
	}
	deletedLockKeys := make([]string, 0, len(deleted))
	for _, key := range deleted {
		deletedLockKeys = append(deletedLockKeys, h.lockKey(key))
	}
	candidates, err := h.Locks.RelevantLockRoots(ctx, deletedLockKeys)
	if err != nil {
		return err
	}
	var roots []string
	for _, candidate := range candidates {
		visible, ok := strings.CutPrefix(candidate, h.lockPrefix)
		if !ok {
			continue
		}
		state, err := h.conditionState(ctx, visible, nil)
		if err != nil {
			return err
		}
		if !state.Exists {
			roots = append(roots, candidate)
		}
	}
	if guard != nil {
		return guard.DeleteExactPaths(ctx, roots)
	}
	return h.Locks.DeleteExactPaths(ctx, roots)
}

func (h Handler) treeObjects(ctx context.Context, key string) ([]r2.Object, error) {
	var objects []r2.Object
	exactFile := false
	object, statErr := h.Objects.Stat(ctx, key)
	if statErr == nil {
		objects = append(objects, object)
		exactFile = !strings.HasSuffix(object.Key, "/") && object.Metadata["webdav-directory"] != "true"
	} else if !errors.Is(statErr, r2.ErrObjectNotFound) {
		return nil, statErr
	}
	prefix := strings.TrimSuffix(key, "/") + "/"
	after := ""
	for {
		list, err := h.Objects.List(ctx, r2.ListOptions{Prefix: prefix, After: after, Limit: 1000})
		if err != nil {
			return nil, err
		}
		for _, child := range list.Objects {
			if child.Key == key {
				continue
			}
			objects = append(objects, child)
		}
		if list.NextMarker == "" {
			break
		}
		after = list.NextMarker
	}
	if len(objects) == 0 {
		return nil, r2.ErrObjectNotFound
	}
	if exactFile && len(objects) > 1 {
		return nil, r2.ErrFileConflict
	}
	return objects, nil
}

func (h Handler) deleteObjects(ctx context.Context, requestKey string, objects []r2.Object, requestConditions r2.MutationConditions) ([]operationResponse, []string) {
	var failures []operationResponse
	var deleted []string
	for index := len(objects) - 1; index >= 0; index-- {
		object := objects[index]
		conditions := requestConditions
		if object.Key != requestKey && strings.TrimSuffix(object.Key, "/") != strings.TrimSuffix(requestKey, "/") {
			conditions = objectFence(object)
		}
		if err := h.Objects.DeleteConditional(ctx, object.Key, conditions); err != nil {
			failures = append(failures, operationFailure(object.Key, err))
			continue
		}
		deleted = append(deleted, object.Key)
	}
	return failures, deleted
}

func objectFence(object r2.Object) r2.MutationConditions {
	if object.ETag == "" {
		return r2.MutationConditions{}
	}
	return r2.MutationConditions{IfMatch: &r2.EntityTagSet{Tags: []r2.EntityTag{{Value: strings.Trim(object.ETag, `"`)}}}}
}

func (h Handler) lock(w http.ResponseWriter, request *http.Request, key string) {
	if h.Locks == nil {
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	prepared, err := h.prepareConditions(request, key)
	if err != nil {
		writeConditionError(w, err)
		return
	}
	if writeConditionResult(w, prepared) {
		return
	}
	const maxLockBody = 64 << 10
	body, err := io.ReadAll(io.LimitReader(request.Body, maxLockBody+1))
	if err != nil || len(body) > maxLockBody {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ttl := parseTimeout(request.Header.Get("Timeout"))
	if len(body) == 0 {
		token, ok := refreshLockToken(prepared.parsed.davIf, prepared.parsed.requestResource)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		lock, err := h.Locks.Refresh(request.Context(), token, ttl)
		if err != nil {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		rootKey, ok := strings.CutPrefix(lock.Key, h.lockPrefix)
		if !ok {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		h.writeLock(w, request, rootKey, lock, http.StatusOK, false)
		return
	}
	depth := request.Header.Get("Depth")
	if depth == "" {
		depth = "infinity"
	}
	if depth != "0" && depth != "infinity" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	owner, err := parseLockInfo(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !prepared.states[prepared.parsed.requestResource].Exists && strings.HasSuffix(key, "/") {
		w.WriteHeader(http.StatusConflict)
		return
	}
	lockKey := canonicalLockKey(key, prepared.states[prepared.parsed.requestResource].Collection)
	lock, err := h.Locks.Create(request.Context(), h.lockKey(lockKey), owner, depth, ttl)
	if err != nil {
		if errors.Is(err, ErrLocked) {
			w.WriteHeader(http.StatusLocked)
		} else {
			w.WriteHeader(http.StatusBadGateway)
		}
		return
	}
	current, err := h.conditionState(request.Context(), key, prepared.parsed.referencedLockTokens())
	if err != nil {
		_ = h.Locks.Delete(request.Context(), lock.Token)
		writeObjectStatus(w, err)
		return
	}
	prepared.states[prepared.parsed.requestResource] = current
	affected := []string{key}
	if !current.Exists {
		affected, err = h.creationMutationPaths(request.Context(), key)
		if err != nil {
			_ = h.Locks.Delete(request.Context(), lock.Token)
			writeObjectStatus(w, err)
			return
		}
	}
	guard, stopped := h.beginMutation(w, request, affected, &prepared, lock.Token)
	if stopped {
		_ = h.Locks.Delete(request.Context(), lock.Token)
		return
	}
	if guard != nil {
		defer guard.Release()
	}
	current = prepared.states[prepared.parsed.requestResource]
	currentLockKey := canonicalLockKey(key, current.Collection)
	if h.lockKey(currentLockKey) != lock.Key || !current.Exists && strings.HasSuffix(key, "/") {
		if guard != nil {
			_ = guard.Delete(request.Context(), lock.Token)
		} else {
			_ = h.Locks.Delete(request.Context(), lock.Token)
		}
		w.WriteHeader(http.StatusConflict)
		return
	}
	if !current.Exists {
		required, pathErr := h.creationMutationPaths(request.Context(), key)
		if pathErr != nil || !containsAllPaths(affected, required) {
			if guard != nil {
				_ = guard.Delete(request.Context(), lock.Token)
			} else {
				_ = h.Locks.Delete(request.Context(), lock.Token)
			}
			if pathErr != nil {
				writeObjectStatus(w, pathErr)
			} else {
				w.WriteHeader(http.StatusConflict)
			}
			return
		}
	}
	status := http.StatusOK
	if !current.Exists {
		status = http.StatusCreated
		_, putErr := h.Objects.PutConditional(request.Context(), r2.PutRequest{
			Key: key, Body: strings.NewReader(""), Size: 0, ContentType: "application/octet-stream",
			Metadata: map[string]string{"webdav": "true"}, PayloadHash: "UNSIGNED-PAYLOAD",
			Conditions: r2.MutationConditions{IfNoneMatch: &r2.EntityTagSet{Wildcard: true}},
		})
		if putErr != nil {
			if guard != nil {
				_ = guard.Delete(request.Context(), lock.Token)
			} else {
				_ = h.Locks.Delete(request.Context(), lock.Token)
			}
			writeObjectStatus(w, putErr)
			return
		}
	}
	rootKey, ok := strings.CutPrefix(lock.Key, h.lockPrefix)
	if !ok {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	h.writeLock(w, request, rootKey, lock, status, true)
}

func (h Handler) lockKey(key string) string {
	return h.lockPrefix + key
}

func canonicalLockKey(key string, collection bool) string {
	if !collection || key == "" {
		return key
	}
	return strings.TrimSuffix(key, "/") + "/"
}

func (h Handler) writeLock(w http.ResponseWriter, request *http.Request, key string, lock Lock, status int, includeToken bool) {
	if includeToken {
		w.Header().Set("Lock-Token", "<"+lock.Token+">")
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(lockResponse{
		XMLNS: "DAV:", Discovery: lockDiscoveryProperty{ActiveLocks: []activeLock{makeActiveLock(lock, lockRootURL(request, key))}},
	})
}

func (h Handler) unlock(w http.ResponseWriter, request *http.Request, key string) {
	if h.Locks == nil {
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	prepared, err := h.prepareConditions(request, key)
	if err != nil {
		writeConditionError(w, err)
		return
	}
	if writeConditionResult(w, prepared) {
		return
	}
	token, ok := parseLockTokenHeader(request.Header.Get("Lock-Token"))
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	state := prepared.states[prepared.parsed.requestResource]
	covered, err := h.Locks.TokenCovers(request.Context(), token, h.lockKey(canonicalLockKey(key, state.Collection)))
	if err != nil || !covered {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if err := h.Locks.Delete(request.Context(), token); err != nil {
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
	trailing := strings.HasSuffix(value, "/")
	cleaned := path.Clean("/" + value)
	if cleaned == "/" {
		return "", nil
	}
	key := strings.TrimPrefix(cleaned, "/")
	if trailing {
		key += "/"
	}
	return key, nil
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

func refreshLockToken(header *davIfHeader, resource string) (string, bool) {
	if header == nil || len(header.lists) != 1 {
		return "", false
	}
	list := header.lists[0]
	if list.resource != resource || len(list.conditions) != 1 {
		return "", false
	}
	condition := list.conditions[0]
	return condition.token, condition.token != "" && !condition.not && condition.tag == nil
}

func parseLockTokenHeader(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < 3 || value[0] != '<' || value[len(value)-1] != '>' {
		return "", false
	}
	token := value[1 : len(value)-1]
	parsed, err := url.Parse(token)
	if err != nil || !parsed.IsAbs() || parsed.Scheme == "" || parsed.Fragment != "" || strings.TrimSpace(token) != token {
		return "", false
	}
	return token, true
}

func parseLockInfo(body []byte) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader(string(body)))
	depth := 0
	rootSeen := false
	scopeDepth, typeDepth := -1, -1
	scopeCount, typeCount, ownerCount := 0, 0, 0
	exclusive, write := false, false
	owner := ""
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", errors.New("invalid lockinfo XML")
		}
		switch value := token.(type) {
		case xml.StartElement:
			if depth == 1 && value.Name.Space == "DAV:" && value.Name.Local == "owner" {
				ownerCount++
				if ownerCount > 1 {
					return "", errors.New("duplicate lockinfo element")
				}
				owner, err = readOwnerElement(decoder, value)
				if err != nil {
					return "", err
				}
				continue
			}
			switch depth {
			case 0:
				if rootSeen || value.Name.Space != "DAV:" || value.Name.Local != "lockinfo" {
					return "", errors.New("lockinfo must be a DAV: element")
				}
				rootSeen = true
			case 1:
				if value.Name.Space != "DAV:" {
					return "", errors.New("lockinfo children must be DAV: elements")
				}
				switch value.Name.Local {
				case "lockscope":
					scopeCount++
					scopeDepth = depth + 1
				case "locktype":
					typeCount++
					typeDepth = depth + 1
				default:
					return "", errors.New("unsupported lockinfo element")
				}
				if scopeCount > 1 || typeCount > 1 || ownerCount > 1 {
					return "", errors.New("duplicate lockinfo element")
				}
			case 2:
				switch {
				case depth == scopeDepth:
					if value.Name.Space != "DAV:" || value.Name.Local != "exclusive" || exclusive {
						return "", errors.New("only exclusive locks are supported")
					}
					exclusive = true
				case depth == typeDepth:
					if value.Name.Space != "DAV:" || value.Name.Local != "write" || write {
						return "", errors.New("only write locks are supported")
					}
					write = true
				default:
					return "", errors.New("invalid lockinfo structure")
				}
			default:
				return "", errors.New("lockscope and locktype values must be empty")
			}
			depth++
		case xml.EndElement:
			if depth == scopeDepth && value.Name.Space == "DAV:" && value.Name.Local == "lockscope" {
				scopeDepth = -1
			}
			if depth == typeDepth && value.Name.Space == "DAV:" && value.Name.Local == "locktype" {
				typeDepth = -1
			}
			depth--
			if depth < 0 {
				return "", errors.New("invalid lockinfo XML")
			}
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return "", errors.New("invalid lockinfo text")
			}
		}
	}
	if !rootSeen || depth != 0 || scopeCount != 1 || typeCount != 1 || !exclusive || !write {
		return "", errors.New("only exclusive write locks are supported")
	}
	return owner, nil
}

func readOwnerElement(decoder *xml.Decoder, start xml.StartElement) (string, error) {
	return readOwnerXML(decoder, &start)
}

func readOwnerContents(decoder *xml.Decoder) (string, error) {
	return readOwnerXML(decoder, nil)
}

func readOwnerXML(decoder *xml.Decoder, root *xml.StartElement) (string, error) {
	var contents strings.Builder
	encoder := xml.NewEncoder(&contents)
	if root != nil {
		start := ownerStartElement(*root)
		if err := encoder.EncodeToken(start); err != nil {
			return "", err
		}
	}
	depth := 1
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", errors.New("invalid lockinfo owner XML")
		}
		switch value := token.(type) {
		case xml.StartElement:
			depth++
			if err := encoder.EncodeToken(ownerStartElement(value)); err != nil {
				return "", err
			}
		case xml.EndElement:
			depth--
			if depth == 0 {
				if root != nil {
					if err := encoder.EncodeToken(value); err != nil {
						return "", err
					}
				}
				if err := encoder.Flush(); err != nil {
					return "", err
				}
				return contents.String(), nil
			}
			if err := encoder.EncodeToken(value); err != nil {
				return "", err
			}
		default:
			if err := encoder.EncodeToken(token); err != nil {
				return "", err
			}
		}
	}
}

func ownerStartElement(start xml.StartElement) xml.StartElement {
	attributes := make([]xml.Attr, 0, len(start.Attr))
	for _, attribute := range start.Attr {
		if attribute.Name.Space == "xmlns" || attribute.Name.Space == "" && attribute.Name.Local == "xmlns" {
			continue
		}
		attributes = append(attributes, attribute)
	}
	start.Attr = attributes
	return start
}

func writeObjectStatus(w http.ResponseWriter, err error) {
	w.WriteHeader(objectStatusCode(err))
}

func objectStatusCode(err error) int {
	switch {
	case errors.Is(err, r2.ErrObjectNotFound):
		return http.StatusNotFound
	case errors.Is(err, r2.ErrQuotaExceeded):
		return http.StatusInsufficientStorage
	case errors.Is(err, r2.ErrR2CredentialsRequired):
		return http.StatusServiceUnavailable
	case errors.Is(err, r2.ErrWriteInProgress):
		return http.StatusLocked
	case errors.Is(err, r2.ErrPreconditionFailed):
		return http.StatusPreconditionFailed
	case errors.Is(err, r2.ErrConditionalRequestConflict):
		return http.StatusConflict
	case errors.Is(err, r2.ErrFileConflict):
		return http.StatusConflict
	case errors.Is(err, r2.ErrBucketDeleting):
		return http.StatusServiceUnavailable
	case errors.Is(err, r2.ErrRateLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, r2.ErrRangeNotSatisfiable):
		return http.StatusRequestedRangeNotSatisfiable
	default:
		return http.StatusBadGateway
	}
}

func makeProperty(key string, object r2.Object, directory bool) propertyResponse {
	href := davHref(key)
	if directory && !strings.HasSuffix(href, "/") {
		href += "/"
	}
	resourceType := resourceType{}
	if directory {
		resourceType.Collection = &struct{}{}
	}
	displayName := path.Base(strings.TrimSuffix("/"+strings.TrimPrefix(key, "/"), "/"))
	if href == "/" {
		displayName = "/"
	}
	lastModified := ""
	if !object.LastModified.IsZero() {
		lastModified = object.LastModified.UTC().Format(http.TimeFormat)
	}
	var contentLength *int64
	if !directory {
		size := object.Size
		contentLength = &size
	}
	return propertyResponse{
		Href: href, Key: key,
		PropStat: propertyStat{Properties: properties{
			DisplayName: displayName, ResourceType: resourceType,
			ContentLength: contentLength, LastModified: lastModified, ETag: strongETag(object.ETag),
		}, Status: "HTTP/1.1 200 OK"},
	}
}

func davHref(key string) string {
	return (&url.URL{Path: "/" + strings.TrimPrefix(key, "/")}).EscapedPath()
}

func (h Handler) decorateLockProperties(ctx context.Context, request *http.Request, responses []propertyResponse) error {
	if h.Locks == nil {
		return nil
	}
	locks, err := h.Locks.activeLocks(ctx, time.Now().Unix())
	if err != nil {
		return err
	}
	for index := range responses {
		responseKey := responses[index].Key
		properties := &responses[index].PropStat.Properties
		properties.SupportedLock = &supportedLock{Entries: []lockEntry{{
			Scope: lockScope{Exclusive: &struct{}{}}, Type: lockType{Write: &struct{}{}},
		}}}
		properties.LockDiscovery = &lockDiscoveryProperty{}
		for _, lock := range locks {
			if !lockCovers(lock, h.lockKey(responseKey)) {
				continue
			}
			rootKey, ok := strings.CutPrefix(lock.Key, h.lockPrefix)
			if !ok {
				continue
			}
			properties.LockDiscovery.ActiveLocks = append(properties.LockDiscovery.ActiveLocks, makeActiveLock(lock, lockRootURL(request, rootKey)))
		}
	}
	return nil
}

func makeActiveLock(lock Lock, root string) activeLock {
	seconds := int(time.Until(lock.ExpiresAt).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	return activeLock{
		Scope: lockScope{Exclusive: &struct{}{}}, Type: lockType{Write: &struct{}{}}, Depth: lock.Depth,
		Timeout: "Second-" + strconv.Itoa(seconds), Owner: makeLockOwner(lock.Owner),
		Token: lockHref{Href: lock.Token}, Root: lockHref{Href: root},
	}
}

func makeLockOwner(stored string) *lockOwner {
	trimmed := strings.TrimSpace(stored)
	if trimmed == "" {
		return nil
	}
	decoder := xml.NewDecoder(strings.NewReader(trimmed))
	first, err := decoder.Token()
	if err == nil {
		if start, ok := first.(xml.StartElement); ok {
			switch {
			case start.Name.Space == "DAV:" && start.Name.Local == "lockinfo":
				if owner, parseErr := parseLockInfo([]byte(trimmed)); parseErr == nil && owner != "" {
					return makeLockOwner(owner)
				}
			case start.Name.Space == "DAV:" && start.Name.Local == "owner":
				if owner, parseErr := readOwnerContents(decoder); parseErr == nil {
					return &lockOwner{Attrs: ownerStartElement(start).Attr, InnerXML: owner}
				}
			}
		}
	}
	if strings.HasPrefix(trimmed, "<") && validOwnerFragment(trimmed) {
		return &lockOwner{InnerXML: trimmed}
	}
	var escaped strings.Builder
	_ = xml.EscapeText(&escaped, []byte(trimmed))
	return &lockOwner{InnerXML: escaped.String()}
}

func validOwnerFragment(value string) bool {
	decoder := xml.NewDecoder(strings.NewReader("<wrapper>" + value + "</wrapper>"))
	for {
		_, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			return false
		}
	}
}

func lockRootURL(request *http.Request, key string) string {
	scheme := request.URL.Scheme
	if scheme == "" {
		scheme = strings.ToLower(strings.TrimSpace(request.Header.Get("X-Forwarded-Proto")))
	}
	if scheme != "http" && scheme != "https" {
		if request.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	root := &url.URL{Scheme: scheme, Host: request.Host, Path: "/" + strings.TrimPrefix(key, "/")}
	return root.String()
}

type multistatus struct {
	XMLName   xml.Name           `xml:"multistatus"`
	XMLNS     string             `xml:"xmlns,attr"`
	Responses []propertyResponse `xml:"response"`
}

type operationMultistatus struct {
	XMLName   xml.Name            `xml:"multistatus"`
	XMLNS     string              `xml:"xmlns,attr"`
	Responses []operationResponse `xml:"response"`
}

type operationResponse struct {
	Href   string `xml:"href"`
	Status string `xml:"status"`
	Code   int    `xml:"-"`
}

func operationFailure(key string, err error) operationResponse {
	status := objectStatusCode(err)
	return operationResponse{Href: davHref(key), Status: fmt.Sprintf("HTTP/1.1 %d %s", status, http.StatusText(status)), Code: status}
}

func writeOperationMultistatus(w http.ResponseWriter, responses []operationResponse) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(operationMultistatus{XMLNS: "DAV:", Responses: responses})
}

type propertyResponse struct {
	Href     string       `xml:"href"`
	PropStat propertyStat `xml:"propstat"`
	Key      string       `xml:"-"`
}

type propertyStat struct {
	Properties properties `xml:"prop"`
	Status     string     `xml:"status"`
}

type properties struct {
	DisplayName   string                 `xml:"displayname"`
	ResourceType  resourceType           `xml:"resourcetype"`
	ContentLength *int64                 `xml:"getcontentlength,omitempty"`
	LastModified  string                 `xml:"getlastmodified,omitempty"`
	ETag          string                 `xml:"getetag,omitempty"`
	SupportedLock *supportedLock         `xml:"supportedlock,omitempty"`
	LockDiscovery *lockDiscoveryProperty `xml:"lockdiscovery,omitempty"`
}

type resourceType struct {
	Collection *struct{} `xml:"collection,omitempty"`
}

type lockResponse struct {
	XMLName   xml.Name              `xml:"prop"`
	XMLNS     string                `xml:"xmlns,attr"`
	Discovery lockDiscoveryProperty `xml:"lockdiscovery"`
}

type lockDiscoveryProperty struct {
	ActiveLocks []activeLock `xml:"activelock"`
}

type activeLock struct {
	Scope   lockScope  `xml:"lockscope"`
	Type    lockType   `xml:"locktype"`
	Depth   string     `xml:"depth"`
	Timeout string     `xml:"timeout"`
	Owner   *lockOwner `xml:"owner,omitempty"`
	Token   lockHref   `xml:"locktoken"`
	Root    lockHref   `xml:"lockroot"`
}

type lockOwner struct {
	Attrs    []xml.Attr `xml:",any,attr"`
	InnerXML string     `xml:",innerxml"`
}

type supportedLock struct {
	Entries []lockEntry `xml:"lockentry"`
}

type lockEntry struct {
	Scope lockScope `xml:"lockscope"`
	Type  lockType  `xml:"locktype"`
}

type lockScope struct {
	Exclusive *struct{} `xml:"exclusive"`
}

type lockType struct {
	Write *struct{} `xml:"write"`
}

type lockHref struct {
	Href string `xml:"href"`
}
