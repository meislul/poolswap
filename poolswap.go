// Package poolswap provides a goroutine-safe container for hot-swapping
// heavy objects (e.g. caches or configs) without blocking readers or generating GC pressure.
//
// This works by wrapping a sync.Pool with atomic reference counting. This
// allows readers to safely hold references to an object while a writer swaps
// it out. Old objects are automatically returned to the pool once all readers
// are done.
package poolswap

import (
	"sync"
	"sync/atomic"
)

// Ref should be embedded as the first field in your cache structs.
// Includes cache-line padding to prevent false sharing on the counter.
type Ref struct {
	count atomic.Int64
	_     [56]byte // Padding to fill 64-byte cache line
}

func (r *Ref) addRef(delta int64) int64 { return r.count.Add(delta) }
func (r *Ref) setRef(v int64)           { r.count.Store(v) }

// DebugPeekRef returns the current reference count; for testing and debugging only.
func (r *Ref) DebugPeekRef() int64 { return r.count.Load() }

// Same as Ref, but without the padding.
type RefNoPadding struct {
	count atomic.Int64
}

func (r *RefNoPadding) addRef(delta int64) int64 { return r.count.Add(delta) }
func (r *RefNoPadding) setRef(v int64)           { r.count.Store(v) }

// DebugPeekRef returns the current reference count; for testing and debugging only.
func (r *RefNoPadding) DebugPeekRef() int64 { return r.count.Load() }

// Referenceable defines the contract for objects managed by this library.
// The only way to implement this is to embed our Ref struct.
type Referenceable interface {
	addRef(delta int64) int64
	setRef(v int64)
}

type PtrRef[T any] interface {
	*T
	Referenceable
}

// Pool wraps a sync.Pool with reference counting.
//
// When an object's reference count hits zero, the Pool cleans it via the Reset
// function and returns it to the underlying sync.Pool.
//
// T is the struct type (e.g., VariantCache).
// PT is the pointer type (e.g., *VariantCache).
type Pool[T any, PT PtrRef[T]] struct {
	internal sync.Pool
	// Reset is called when refs hit 0.
	// It should clear the object's state (e.g. clear maps, reset slices).
	// Return true to put it back in the pool, false to discard (GC).
	Reset func(PT) bool
}

// NewPool creates a pool for type T.
// factory allocates a new, empty T.
// resetter prepares a used T for reuse (or returns false to discard it).
func NewPool[T any, PT PtrRef[T]](factory func() PT, resetter func(PT) bool) *Pool[T, PT] {
	return &Pool[T, PT]{
		internal: sync.Pool{
			New: func() any { return factory() },
		},
		Reset: resetter,
	}
}

// Release decrements the ref count. If it hits 0, the object is returned to the pool.
// Safe to call with nil.
func (p *Pool[T, PT]) Release(obj PT) {
	if obj == nil {
		return
	}
	if obj.addRef(-1) == 0 {
		p.returnToPool(obj)
	}
}

// Get acquires a fresh object from the pool with Ref=1.
func (p *Pool[T, PT]) Get() PT {
	r := p.internal.Get().(PT)
	r.setRef(1)
	return r
}

func (p *Pool[T, PT]) returnToPool(obj PT) {
	if p.Reset(obj) {
		p.internal.Put(obj)
	}
}

// Container manages a "current" active pointer.
type Container[T any, PT PtrRef[T]] struct {
	pool    *Pool[T, PT]
	mu      sync.RWMutex
	current PT
}

// NewEmptyContainer creates a container for objects from the given Pool.
// The container starts empty (current is nil) until Update is called.
func NewEmptyContainer[T any, PT PtrRef[T]](pool *Pool[T, PT]) *Container[T, PT] {
	return &Container[T, PT]{
		pool: pool,
	}
}

// NewContainer creates a container for objects from the given Pool, initialized
// with the init object.
//
// The object must be not be owned by another instance of poolswap.Container;
// The container takes ownership of the given initial value (reference count set to 1).
func NewContainer[T any, PT PtrRef[T]](pool *Pool[T, PT], init PT) *Container[T, PT] {
	if init != nil {
		init.setRef(1)
	}
	return &Container[T, PT]{
		pool:    pool,
		current: init,
	}
}

// Update performs a hot-swap.
//
// It sets the new object as current and releases the old object.
// The old object will be returned to the pool once all existing readers release it.
func (c *Container[T, PT]) Update(newObj PT) {
	c.mu.Lock()
	oldObj := c.current
	c.current = newObj
	c.mu.Unlock()

	if oldObj != nil {
		c.pool.Release(oldObj)
	}
}

// Release is a convenience proxy to the underlying Pool's Release.
func (c *Container[T, PT]) Release(obj PT) {
	c.pool.Release(obj)
}

// GetNew is a convenience proxy to the underlying Pool's Get.
func (c *Container[T, PT]) GetNew() PT {
	return c.pool.Get()
}

// Acquire returns the current active object with its reference count incremented.
// The caller owns this reference and must call Release() when finished.
//
// Returns nil if the container is empty.
func (c *Container[T, PT]) Acquire() PT {
	c.mu.RLock()
	obj := c.current
	// check for nil in case the container hasn't been initialized yet
	if obj != nil {
		obj.addRef(1)
	}
	c.mu.RUnlock()
	return obj
}

// WithAcquire is a helper that executes fn with the current object (can be nil) and
// automatically releases it afterwards.
func (c *Container[T, PT]) WithAcquire(fn func(obj PT)) {
	obj := c.Acquire()
	if obj != nil {
		defer c.Release(obj)
	}
	fn(obj)
}
