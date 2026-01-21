package singleflight

import "golang.org/x/sync/singleflight"

// Group is a typed wrapper around singleflight.Group.
type Group[T any] struct {
	singleflight.Group
}

// Do executes fn and returns its result. If multiple callers call Do with the
// same key concurrently, only one will execute fn and all will receive its result.
func (g *Group[T]) Do(key string, fn func() (T, error)) (v T, err error, shared bool) {
	untypedV, err, shared := g.Group.Do(key, func() (any, error) {
		return fn()
	})

	return untypedV.(T), err, shared
}
