# poolswap

[![Go Reference](https://pkg.go.dev/badge/github.com/keilerkonzept/poolswap.svg)](https://pkg.go.dev/github.com/keilerkonzept/poolswap)
[![Go Report Card](https://goreportcard.com/badge/github.com/keilerkonzept/poolswap?)](https://goreportcard.com/report/github.com/keilerkonzept/poolswap)

A goroutine-safe container for hot-swapping heavy objects (e.g., caches, configurations) without blocking readers or generating GC pressure.

Combines `sync.Pool` with atomic reference counting to enable non-blocking reads while ensuring old objects are only recycled after all readers finish.

**Contents**
- [Why?](#why)
- [Features](#features)
- [Usage](#usage)
- [Performance](#performance)
- [Notes](#notes)

## Why?

Read-mostly shared resources that need periodic updates present a tradeoff:

- **In-place update under lock (e.g. `sync.RWMutex`)**: Zero-alloc - but blocks all readers during updates
- **Pointer swap + Copy-on-Write (e.g. `atomic.Pointer` or `sync.RWMutex`)**: Fast and non-blocking - but forces you to allocate a new object on each update, causing GC pressure
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
    data: map[string]string{"key": "value"},
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

## Performance

To illustrate the kind of scenario where `poolswap` is useful, here's a benchmark against three other concurrency patterns for updating shared data:

1.  **`AtomicPtr` (Allocating):** A lock-free copy-on-write using `atomic.Pointer`. Reads are fast, but every update allocates a new object, creating GC pressure.
2.  **`MutexAlloc` (Allocating):** A copy-on-write protected by a `sync.RWMutex`. Similar to `AtomicPtr`, it creates garbage on every update.
3.  **`MutexInPlace` (Blocking):** In-place updates under a `sync.RWMutex` lock, alloc-free but blocking all readers during the update.

The benchmark simulates a heavy object (a `map[string]string` with 100k entries) being updated and read concurrently, with 1%, 10%, and 50% write ratios to simulate different levels of churn. Each configuration runs with a set `GOMEMLIMIT` (512MiB, 256Mib, 50Mib) to simulate deployment environments with constrained memory.

*(go1.25.1 on an Apple M1 Pro, 10 cores)*

### 1% Writes (read-heavy, typical cache scenario)

| GOMEMLIMIT | PoolSwap | AtomicPtr | MutexAlloc | MutexInPlace |
|:-----------|:---------|:----------|:-----------|:-------------|
| **Time (µs/op)** |
| 512 MiB    | 5.2      | 9.2 (+76%)| 8.7 (+66%) | 18.0 (+244%) |
| 256 MiB    | 5.2      | 8.4 (+63%)| 8.3 (+60%) | 17.8 (+245%) |
| 50 MiB     | 6.6      | 27.9 (+321%)| 25.6 (+286%) | 17.8 (+169%) |
| **Allocated (B/op)** |
| 512 MiB    | 276      | 52,495 (+18,954%) | 52,489 (+18,952%) | 81 (-71%) |
| 256 MiB    | 305      | 52,490 (+17,138%) | 52,488 (+17,137%) | 77 (-75%) |
| 50 MiB     | 563      | 52,515 (+9,236%)  | 52,519 (+9,237%)  | 79 (-86%) |

### 10% Writes

| GOMEMLIMIT | PoolSwap | AtomicPtr | MutexAlloc | MutexInPlace |
|:-----------|:---------|:----------|:-----------|:-------------|
| **Time (µs/op)** |
| 512 MiB    | 51.3     | 105.9 (+106%) | 92.9 (+81%) | 198.9 (+287%) |
| 256 MiB    | 49.6     | 88.2 (+78%)   | 88.5 (+79%) | 200.8 (+305%) |
| 50 MiB     | 67.4     | 349.7 (+419%) | 295.5 (+339%) | 200.5 (+198%) |
| **Allocated (B/op)** |
| 512 MiB    | 2,880    | 547,803 (+18,921%) | 541,441 (+18,700%) | 958 (-67%) |
| 256 MiB    | 2,569    | 543,597 (+21,060%) | 542,299 (+21,009%) | 943 (-63%) |
| 50 MiB     | 3,628    | 586,419 (+16,064%) | 583,806 (+15,992%) | 963 (-73%) |

### 50% Writes

| GOMEMLIMIT | PoolSwap | AtomicPtr | MutexAlloc | MutexInPlace |
|:-----------|:---------|:----------|:-----------|:-------------|
| **Time (µs/op)** |
| 512 MiB    | 277      | 570 (+106%)    | 550 (+98%)     | 1,211 (+337%) |
| 256 MiB    | 315      | 496 (+57%)     | 507 (+61%)     | 1,289 (+309%) |
| 50 MiB     | 336      | 2,997 (+793%)  | 2,789 (+731%)  | 1,092 (+225%) |
| **Allocated (B/op)** |
| 512 MiB    | 13.4 KiB | 2.9 MiB (+21,444%) | 2.9 MiB (+21,413%) | 6.2 KiB (-54%) |
| 256 MiB    | 16.4 KiB | 2.9 MiB (+17,753%) | 2.9 MiB (+17,865%) | 6.0 KiB (-63%) |
| 50 MiB     | 16.3 KiB | 5.1 MiB (+31,498%) | 5.1 MiB (+31,498%) | 5.3 KiB (-67%) |

### Analysis


- **GC pressure amplifies allocation costs**: Under tight memory constraints, the performance of allocating pointer-swap approaches degrades severely - up to *8x* slower than `poolswap`, which can use a `sync.Pool` and so incurs **(amortized) zero allocations** per op. This saves both on actual allocation work as well as on GC pause durations.
- **Read-heavy workloads**: At 1% writes, `poolswap` is *~1.5x-2x* faster than (allocating) pointer-swaps under relaxed memory limits (512 MiB), but dramatically outperforms them when memory is scarce (50 MiB: *4-5x* faster).
* **Latency:** The `MutexInPlace` strategy never allocates but is *3-4x* slower because it forces all concurrent readers to wait while an update is in progress.


## Notes

### Why not `sync.Pool` + `atomic.Pointer`?

The naive combination is unsafe:

```go
var current atomic.Pointer[MyCache]
var pool = sync.Pool{...}

func update() {
    newCache := pool.Get().(*MyCache)
    // populate newCache
    oldCache := current.Swap(newCache)
    pool.Put(oldCache) // RACE CONDITION: readers may still be using oldCache
}
```

When the writer swaps the pointer, readers may still hold references to `oldCache`. If `oldCache` is immediately returned to the pool, a subsequent `pool.Get()` can return the same memory location while the original reader is still using it - a use-after-free race condition. This is what the reference-counting in `poolswap` fixes.

## License

MIT
