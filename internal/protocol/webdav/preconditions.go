package webdavprotocol

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
)

type preparedConditions struct {
	parsed     requestConditions
	states     map[string]conditionState
	evaluation conditionEvaluation
}

func (h Handler) prepareConditions(request *http.Request, key string) (preparedConditions, error) {
	parsed, err := parseRequestConditions(request)
	if err != nil {
		return preparedConditions{}, err
	}
	tokens := parsed.referencedLockTokens()
	states := make(map[string]conditionState)
	current, err := h.conditionState(request.Context(), key, tokens)
	if err != nil {
		return preparedConditions{}, err
	}
	states[parsed.requestResource] = current
	resolve := func(resource string) (conditionState, error) {
		if state, ok := states[resource]; ok {
			return state, nil
		}
		resourceKey, err := conditionResourceKey(resource)
		if err != nil {
			return conditionState{}, err
		}
		state, err := h.conditionState(request.Context(), resourceKey, tokens)
		if err != nil {
			return conditionState{}, err
		}
		states[resource] = state
		return state, nil
	}
	evaluation, err := parsed.evaluate(request.Method, current, request.Header.Get("Range") != "", resolve)
	if err != nil {
		return preparedConditions{}, err
	}
	return preparedConditions{parsed: parsed, states: states, evaluation: evaluation}, nil
}

func (h Handler) conditionState(ctx context.Context, key string, tokens []string) (conditionState, error) {
	state := conditionState{}
	object, err := h.Objects.Stat(ctx, key)
	switch {
	case err == nil:
		state.Exists = true
		state.Size = object.Size
		state.ObjectID = object.ObjectID
		state.Collection = strings.HasSuffix(object.Key, "/") || object.Metadata["webdav-directory"] == "true"
		state.ETag = object.ETag
		state.LastModified = object.LastModified
	case errors.Is(err, r2.ErrObjectNotFound):
		if key == "" {
			state.Exists = true
			state.Collection = true
		} else {
			prefix := strings.TrimSuffix(key, "/") + "/"
			marker, markerErr := h.Objects.Stat(ctx, prefix)
			if markerErr == nil {
				state.Exists = true
				state.Collection = true
				state.ObjectID = marker.ObjectID
				state.ETag = marker.ETag
				state.LastModified = marker.LastModified
				break
			}
			if !errors.Is(markerErr, r2.ErrObjectNotFound) {
				return conditionState{}, markerErr
			}
			children, listErr := h.Objects.List(ctx, r2.ListOptions{Prefix: prefix, Limit: 1})
			if listErr != nil {
				return conditionState{}, listErr
			}
			state.Exists = len(children.Objects) != 0
			state.Collection = state.Exists
		}
	default:
		return conditionState{}, err
	}
	if h.Locks == nil || len(tokens) == 0 {
		return state, nil
	}
	state.LockTokens = make(map[string]struct{})
	for _, token := range tokens {
		covered, err := h.Locks.TokenCovers(ctx, token, h.lockKey(canonicalLockKey(key, state.Collection)))
		if errors.Is(err, ErrLockNotFound) {
			continue
		}
		if err != nil {
			return conditionState{}, err
		}
		if covered {
			state.LockTokens[token] = struct{}{}
		}
	}
	return state, nil
}

func (prepared preparedConditions) mutationConditions(resource string, includeHTTP bool) r2.MutationConditions {
	conditions := r2.MutationConditions{}
	if includeHTTP {
		conditions.IfMatch = toR2EntityTagSet(prepared.parsed.ifMatch)
		conditions.IfNoneMatch = toR2EntityTagSet(prepared.parsed.ifNoneMatch)
		conditions.IfUnmodifiedSince = prepared.parsed.ifUnmodifiedSince
	}
	if prepared.parsed.davIf == nil || !prepared.parsed.davIf.hasEntityTag(resource) {
		return conditions
	}
	state, ok := prepared.states[resource]
	if !ok {
		return conditions
	}
	if !state.Exists {
		conditions.IfNoneMatch = &r2.EntityTagSet{Wildcard: true}
		return conditions
	}
	current, ok := currentEntityTag(state.ETag)
	if ok && !current.Weak {
		conditions.IfMatch = &r2.EntityTagSet{Tags: []r2.EntityTag{{Value: current.Opaque}}}
	}
	return conditions
}

func (prepared preparedConditions) requestMutationConditions() r2.MutationConditions {
	return prepared.mutationConditions(prepared.parsed.requestResource, true)
}

func (prepared preparedConditions) submittedLockTokens() []string {
	if prepared.parsed.davIf == nil {
		return nil
	}
	return prepared.parsed.davIf.submittedLockTokens()
}

func (prepared *preparedConditions) reevaluateCurrent(method string, current conditionState) error {
	current.LockTokens = prepared.states[prepared.parsed.requestResource].LockTokens
	resolve := func(resource string) (conditionState, error) {
		if resource == prepared.parsed.requestResource {
			return current, nil
		}
		state, ok := prepared.states[resource]
		if !ok {
			return conditionState{}, errInvalidConditionalHeader
		}
		return state, nil
	}
	evaluation, err := prepared.parsed.evaluate(method, current, false, resolve)
	if err != nil {
		return err
	}
	prepared.states[prepared.parsed.requestResource] = current
	prepared.evaluation = evaluation
	return nil
}

func (prepared *preparedConditions) reevaluateConditions(ctx context.Context, h Handler, method string) error {
	tokens := prepared.parsed.referencedLockTokens()
	resources := make(map[string]struct{}, len(prepared.states)+1)
	resources[prepared.parsed.requestResource] = struct{}{}
	for resource := range prepared.states {
		resources[resource] = struct{}{}
	}
	if prepared.parsed.davIf != nil {
		for _, list := range prepared.parsed.davIf.lists {
			resources[list.resource] = struct{}{}
		}
	}
	for resource := range resources {
		key, err := conditionResourceKey(resource)
		if err != nil {
			return err
		}
		state, err := h.conditionState(ctx, key, tokens)
		if err != nil {
			return err
		}
		prepared.states[resource] = state
	}
	current := prepared.states[prepared.parsed.requestResource]
	resolve := func(resource string) (conditionState, error) {
		state, ok := prepared.states[resource]
		if !ok {
			return conditionState{}, errInvalidConditionalHeader
		}
		return state, nil
	}
	evaluation, err := prepared.parsed.evaluate(method, current, false, resolve)
	if err != nil {
		return err
	}
	prepared.evaluation = evaluation
	return nil
}

func (conditions requestConditions) referencedLockTokens() []string {
	if conditions.davIf == nil {
		return nil
	}
	set := make(map[string]struct{})
	for _, list := range conditions.davIf.lists {
		for _, condition := range list.conditions {
			if condition.token != "" {
				set[condition.token] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(set))
	for token := range set {
		result = append(result, token)
	}
	return result
}

func (header davIfHeader) hasEntityTag(resource string) bool {
	for _, list := range header.lists {
		if list.resource != resource {
			continue
		}
		for _, condition := range list.conditions {
			if condition.tag != nil {
				return true
			}
		}
	}
	return false
}

func toR2EntityTagSet(set *entityTagSet) *r2.EntityTagSet {
	if set == nil {
		return nil
	}
	result := &r2.EntityTagSet{Wildcard: set.Any, Tags: make([]r2.EntityTag, 0, len(set.Tags))}
	for _, tag := range set.Tags {
		result.Tags = append(result.Tags, r2.EntityTag{Value: tag.Opaque, Weak: tag.Weak})
	}
	return result
}

func conditionResourceKey(resource string) (string, error) {
	parsed, err := url.ParseRequestURI(resource)
	if err != nil || parsed.RawQuery != "" || parsed.ForceQuery {
		return "", errInvalidConditionalHeader
	}
	return requestKey(parsed.Path)
}

func writeConditionResult(w http.ResponseWriter, prepared preparedConditions) bool {
	switch prepared.evaluation.Outcome {
	case conditionNotModified:
		state := prepared.states[prepared.parsed.requestResource]
		if etag := strongETag(state.ETag); etag != "" {
			w.Header().Set("ETag", etag)
		}
		if !state.LastModified.IsZero() {
			w.Header().Set("Last-Modified", state.LastModified.UTC().Format(http.TimeFormat))
		}
		w.WriteHeader(http.StatusNotModified)
		return true
	case conditionPreconditionFailed:
		w.WriteHeader(http.StatusPreconditionFailed)
		return true
	default:
		return false
	}
}

func writeConditionError(w http.ResponseWriter, err error) {
	if errors.Is(err, errInvalidConditionalHeader) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	writeObjectStatus(w, err)
}

func (h Handler) beginMutation(w http.ResponseWriter, request *http.Request, keys []string, prepared *preparedConditions, extraTokens ...string) (*lockMutationGuard, bool) {
	return h.beginMutationWithDependencies(w, request, keys, keys, prepared, extraTokens...)
}

func (h Handler) beginMutationWithDependencies(w http.ResponseWriter, request *http.Request, lockKeys, mutationKeys []string, prepared *preparedConditions, extraTokens ...string) (*lockMutationGuard, bool) {
	if h.Locks == nil {
		return nil, false
	}
	if prepared.parsed.davIf != nil {
		for _, list := range prepared.parsed.davIf.lists {
			key, err := conditionResourceKey(list.resource)
			if err != nil {
				writeConditionError(w, err)
				return nil, true
			}
			mutationKeys = append(mutationKeys, key)
		}
	}
	mutationKeys = uniquePaths(mutationKeys)
	internalLockKeys := make([]string, 0, len(lockKeys))
	for _, key := range lockKeys {
		internalLockKeys = append(internalLockKeys, h.lockKey(key))
	}
	internalMutationKeys := make([]string, 0, len(mutationKeys))
	for _, key := range mutationKeys {
		internalMutationKeys = append(internalMutationKeys, h.lockKey(key))
	}
	providedTokens := append(prepared.submittedLockTokens(), extraTokens...)
	guard, err := h.Locks.GuardPaths(request.Context(), internalLockKeys, internalMutationKeys, providedTokens, func() error {
		if err := prepared.reevaluateConditions(request.Context(), h, request.Method); err != nil {
			return err
		}
		if prepared.evaluation.Outcome != conditionProceed {
			return r2.ErrPreconditionFailed
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrLocked) {
			w.WriteHeader(http.StatusLocked)
		} else {
			writeConditionError(w, err)
		}
		return nil, true
	}
	*request = *request.WithContext(r2.WithWebDAVMutationGuard(request.Context(), h.Locks, internalMutationKeys))
	return guard, false
}
