# poolswap

[![Go Reference](https://pkg.go.dev/badge/github.com/keilerkonzept/poolswap.svg)](https://pkg.go.dev/github.com/keilerkonzept/poolswap)
[![Go Report Card](https://goreportcard.com/badge/github.com/keilerkonzept/poolswap?)](https://goreportcard.com/report/github.com/keilerkonzept/poolswap)

A goroutine-safe container for hot-swapping heavy objects (e.g., caches, configurations) without blocking readers or generating GC pressure.

Combines `sync.Pool` with atomic reference counting to enable non-blocking reads while ensuring old objects are only recycled after all readers finish.

**Contents**
- [Why?](#why)
- [Features](#features)
- [Usage](#usage)
- [Notes](#notes)

## Why?

Read-mostly shared resources that need periodic updates present a tradeoff:

- **In-place update under lock (e.g. `sync.RWMutex`)**: Blocks all readers during updates
- **Pointer swap (e.g. `atomic.Pointer` or `sync.RWMutex`)**: Fast but forces you to allocate a new object on each update, causing GC pressure
- **`sync.Pool` + `atomic.Pointer`**: Seems ideal but is unsafe (see [Notes](#why-not-syncpool--atomicpointer) below)

`poolswap` solves this through reference counting. Objects return to the pool only when all readers release them.

## Features

- Non-blocking reads (lock held only during pointer acquisition)
- Object reuse via `sync.Pool` (zero-allocation at steady state)
- Type-safe API using Go generics

## Usage

### Define Your Object

Embed `poolswap.Ref` as the first field:

```go
import "github.com/keilerkonzept/poolswap"

type MyCache struct {
    poolswap.Ref
    data map[string]string
}
```

### Create a Pool

Provide a factory function and a reset function:

```go
pool := poolswap.NewPool(
    func() *MyCache {
        return &MyCache{data: make(map[string]string)}
    },
    func(c *MyCache) bool {
        clear(c.data)
        return true // true = return to pool, false = discard
    },
)
```

### Create a Container

```go
container := poolswap.NewEmptyContainer(pool)

// Or initialize with an object:
container := poolswap.NewContainer(pool, &MyObject{
    data : map[string]string{"key": "value"},
})
```

### Read from Container

Always `Release` after `Acquire`:

```go
func read(container *poolswap.Container[MyCache, *MyCache]) {
    cache := container.Acquire()
    if cache == nil {
        return // Container empty
    }
    defer container.Release(cache)

    // Use cache safely
    val := cache.data["key"]
}

// Or use the helper (but this may allocate for the closure):
container.WithAcquire(func(cache *MyCache) {
    if cache != nil {
        val := cache.data["key"]
    }
})
```

### Update Container

```go
func update(container *poolswap.Container[MyCache, *MyCache]) {
    newCache := pool.Get()
    newCache.data["key"] = "new_value"

    container.Update(newCache)
    // Old cache automatically returned to pool once all readers finish
}
```

## License

MIT
