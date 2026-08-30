package r2

import (
	"context"
	"errors"
	"reflect"
	"strings"
)

// WebDAVMutationGuard is implemented by the WebDAV lock store without
// introducing a dependency from the storage layer back to the protocol
// package. The admin file manager and background jobs use the same guard as
// WebDAV requests so tree mutations cannot interleave.
type WebDAVMutationGuard interface {
	Release()
	DeletePaths(context.Context, []string) error
	DeleteExactPaths(context.Context, []string) error
}

type WebDAVMutationCoordinator interface {
	GuardExternalPaths(context.Context, []string) (WebDAVMutationGuard, error)
	RelevantLockRoots(context.Context, []string) ([]string, error)
	DeletePaths(context.Context, []string) error
	DeleteExactPaths(context.Context, []string) error
}

type webDAVMutationContextKey struct{}

type webDAVMutationScope struct {
	coordinator WebDAVMutationCoordinator
	paths       []string
}

// WithWebDAVMutationGuard marks a context whose caller already holds the
// shared namespace guard. Nested storage calls still perform lock cleanup,
// but do not acquire the same non-reentrant path guard again.
func WithWebDAVMutationGuard(ctx context.Context, coordinator WebDAVMutationCoordinator, paths []string) context.Context {
	return context.WithValue(ctx, webDAVMutationContextKey{}, webDAVMutationScope{
		coordinator: coordinator,
		paths:       uniqueWebDAVPaths(paths),
	})
}

func webDAVMutationScopeCovers(ctx context.Context, coordinator WebDAVMutationCoordinator, paths []string) bool {
	scope, ok := ctx.Value(webDAVMutationContextKey{}).(webDAVMutationScope)
	if !ok || !sameWebDAVMutationCoordinator(scope.coordinator, coordinator) {
		return false
	}
	for _, requested := range uniqueWebDAVPaths(paths) {
		covered := false
		for _, root := range scope.paths {
			if webDAVPathAtOrBelow(requested, root) {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func sameWebDAVMutationCoordinator(left, right WebDAVMutationCoordinator) bool {
	leftType, rightType := reflect.TypeOf(left), reflect.TypeOf(right)
	return leftType != nil && leftType == rightType && leftType.Comparable() && left == right
}

func (s Service) beginWebDAVMutation(ctx context.Context, paths []string) (context.Context, WebDAVMutationGuard, error) {
	if s.WebDAVCoordinator == nil {
		return ctx, nil, nil
	}
	paths = uniqueWebDAVPaths(paths)
	if len(paths) == 0 {
		return ctx, nil, nil
	}
	if webDAVMutationScopeCovers(ctx, s.WebDAVCoordinator, paths) {
		return ctx, nil, nil
	}
	guard, err := s.WebDAVCoordinator.GuardExternalPaths(ctx, paths)
	if err != nil {
		return ctx, nil, err
	}
	return WithWebDAVMutationGuard(ctx, s.WebDAVCoordinator, paths), guard, nil
}

func (s Service) validateWebDAVMutationScope(ctx context.Context, paths []string) error {
	if s.WebDAVCoordinator == nil || len(paths) == 0 || webDAVMutationScopeCovers(ctx, s.WebDAVCoordinator, paths) {
		return nil
	}
	return ErrWriteInProgress
}

// BeginWebDAVTreeMutation reserves complete mount-relative trees for an admin
// or background operation. The returned context must be used for nested
// Service calls and the guard released when the operation finishes.
func (s Service) BeginWebDAVTreeMutation(ctx context.Context, keys ...string) (context.Context, WebDAVMutationGuard, error) {
	return s.beginWebDAVMutation(ctx, s.webDAVTreeMutationPaths(keys...))
}

func (s Service) webDAVCreationMutationPaths(ctx context.Context, key string) ([]string, error) {
	if !IsWebDAVInternalKey(key) {
		return nil, nil
	}
	if _, err := s.Index.GetObject(ctx, key); err == nil {
		return []string{key}, nil
	} else if !errors.Is(err, ErrObjectNotFound) {
		return nil, err
	}
	paths := []string{key}
	mount, ok := webDAVMountPrefixFromKey(key)
	if !ok {
		return paths, nil
	}
	for parent := internalParentKey(key, mount); parent != ""; parent = internalParentKey(parent, mount) {
		paths = append(paths, parent)
		exists, err := s.webDAVResourceExists(ctx, parent)
		if err != nil {
			return nil, err
		}
		if exists || parent == mount {
			break
		}
	}
	return uniqueWebDAVPaths(paths), nil
}

func (s Service) webDAVDeletionMutationPaths(ctx context.Context, key string) ([]string, error) {
	if !IsWebDAVInternalKey(key) {
		return nil, nil
	}
	paths := []string{key}
	mount, ok := webDAVMountPrefixFromKey(key)
	if !ok {
		return paths, nil
	}
	for parent := internalParentKey(key, mount); parent != ""; parent = internalParentKey(parent, mount) {
		paths = append(paths, parent)
		remains, err := s.webDAVCollectionRemainsAfterDelete(ctx, parent, key)
		if err != nil {
			return nil, err
		}
		if remains || parent == mount {
			break
		}
	}
	return uniqueWebDAVPaths(paths), nil
}

func (s Service) webDAVTreeMutationPaths(keys ...string) []string {
	var paths []string
	for _, key := range keys {
		if !IsWebDAVInternalKey(key) {
			continue
		}
		mount, ok := webDAVMountPrefixFromKey(key)
		if !ok {
			continue
		}
		paths = append(paths, key)
		for parent := internalParentKey(key, mount); parent != ""; parent = internalParentKey(parent, mount) {
			paths = append(paths, parent)
			if parent == mount {
				break
			}
		}
	}
	return uniqueWebDAVPaths(paths)
}

func (s Service) webDAVResourceExists(ctx context.Context, key string) (bool, error) {
	if mount, ok := webDAVMountPrefixFromKey(key); ok && key == mount {
		// An authenticated WebDAV mount root exists even when it has no objects.
		return true, nil
	}
	if _, err := s.Index.GetObject(ctx, key); err == nil {
		return true, nil
	} else if !errors.Is(err, ErrObjectNotFound) {
		return false, err
	}
	prefix := strings.TrimSuffix(key, "/") + "/"
	objects, err := s.Index.ListObjects(ctx, ListOptions{Prefix: prefix, Limit: 1})
	if err != nil {
		return false, err
	}
	return len(objects.Objects) != 0, nil
}

func (s Service) webDAVCollectionRemainsAfterDelete(ctx context.Context, collection, deleting string) (bool, error) {
	if object, err := s.Index.GetObject(ctx, collection); err == nil {
		if object.Key != deleting {
			return true, nil
		}
	} else if !errors.Is(err, ErrObjectNotFound) {
		return false, err
	}
	prefix := strings.TrimSuffix(collection, "/") + "/"
	after := ""
	for {
		objects, err := s.Index.ListObjects(ctx, ListOptions{Prefix: prefix, After: after, Limit: 100})
		if err != nil {
			return false, err
		}
		for _, object := range objects.Objects {
			if object.Key != deleting {
				return true, nil
			}
		}
		if objects.NextMarker == "" {
			return false, nil
		}
		after = objects.NextMarker
	}
}

func (s Service) cleanupDeletedWebDAVLocks(ctx context.Context, key string, guard WebDAVMutationGuard) error {
	if s.WebDAVCoordinator == nil || !IsWebDAVInternalKey(key) {
		return nil
	}
	mount, ok := webDAVMountPrefixFromKey(key)
	if !ok {
		return nil
	}
	candidates, err := s.WebDAVCoordinator.RelevantLockRoots(ctx, []string{key})
	if err != nil {
		return err
	}
	var roots []string
	for _, candidate := range uniqueWebDAVPaths(candidates) {
		if !strings.HasPrefix(candidate, mount) {
			continue
		}
		exists, err := s.webDAVResourceExists(ctx, candidate)
		if err != nil {
			return err
		}
		if !exists {
			roots = append(roots, candidate)
		}
	}
	if len(roots) == 0 {
		return nil
	}
	if guard != nil {
		return guard.DeleteExactPaths(ctx, roots)
	}
	return s.WebDAVCoordinator.DeleteExactPaths(ctx, roots)
}

func internalParentKey(key, mount string) string {
	trimmed := strings.TrimSuffix(key, "/")
	separator := strings.LastIndex(trimmed, "/")
	if separator < 0 {
		return ""
	}
	parent := trimmed[:separator+1]
	if len(parent) < len(mount) {
		return ""
	}
	return parent
}

func uniqueWebDAVPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, key := range paths {
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, key)
	}
	return result
}

func webDAVPathAtOrBelow(key, root string) bool {
	return strings.TrimSuffix(key, "/") == strings.TrimSuffix(root, "/") ||
		root == "" || strings.HasPrefix(key, strings.TrimSuffix(root, "/")+"/")
}
