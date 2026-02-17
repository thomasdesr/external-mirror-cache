// Package singleflight provides a typed wrapper around x/sync/singleflight.
package singleflight

import "golang.org/x/sync/singleflight"

// Group is a typed wrapper around singleflight.Group.
type Group[T any] struct {
	group singleflight.Group
}

// Do executes fn and returns its result. If multiple callers call Do with the
// same key concurrently, only one will execute fn and all will receive its result.
//
//nolint:revive // error-return: matches stdlib singleflight.Group.Do signature
func (g *Group[T]) Do(key string, fn func() (T, error)) (v T, err error, shared bool) {
	untypedV, err, shared := g.group.Do(key, func() (any, error) {
		return fn()
	})

	v, _ = untypedV.(T)

	return v, err, shared //nolint:wrapcheck // transparent typed wrapper, error originates in fn
}
