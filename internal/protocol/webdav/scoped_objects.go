package webdavprotocol

import (
	"context"
	"strings"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
)

type scopedObjects struct {
	base   ObjectService
	prefix string
}

func (s scopedObjects) Put(ctx context.Context, request r2.PutRequest) (r2.Object, error) {
	request.Key = s.prefix + request.Key
	object, err := s.base.Put(ctx, request)
	return s.visibleObject(object, err)
}

func (s scopedObjects) Get(ctx context.Context, key string, options r2.GetOptions) (r2.GetResult, error) {
	return s.base.Get(ctx, s.prefix+key, options)
}

func (s scopedObjects) Stat(ctx context.Context, key string) (r2.Object, error) {
	object, err := s.base.Stat(ctx, s.prefix+key)
	return s.visibleObject(object, err)
}

func (s scopedObjects) List(ctx context.Context, options r2.ListOptions) (r2.ObjectList, error) {
	options.Prefix = s.prefix + options.Prefix
	if options.After != "" {
		options.After = s.prefix + options.After
	}
	result, err := s.base.List(ctx, options)
	if err != nil {
		return r2.ObjectList{}, err
	}
	for index := range result.Objects {
		visible, ok := strings.CutPrefix(result.Objects[index].Key, s.prefix)
		if !ok {
			return r2.ObjectList{}, r2.ErrInvalidPath
		}
		result.Objects[index].Key = visible
	}
	if result.NextMarker != "" {
		visible, ok := strings.CutPrefix(result.NextMarker, s.prefix)
		if !ok {
			return r2.ObjectList{}, r2.ErrInvalidPath
		}
		result.NextMarker = visible
	}
	return result, nil
}

func (s scopedObjects) Delete(ctx context.Context, key string) error {
	return s.base.Delete(ctx, s.prefix+key)
}

func (s scopedObjects) Copy(ctx context.Context, source, destination string) (r2.Object, error) {
	object, err := s.base.Copy(ctx, s.prefix+source, s.prefix+destination)
	return s.visibleObject(object, err)
}

func (s scopedObjects) visibleObject(object r2.Object, err error) (r2.Object, error) {
	if err != nil {
		return r2.Object{}, err
	}
	visible, ok := strings.CutPrefix(object.Key, s.prefix)
	if !ok {
		return r2.Object{}, r2.ErrInvalidPath
	}
	object.Key = visible
	return object, nil
}
