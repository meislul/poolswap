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

## Performance

Here's a benchmark of `poolswap` against the three other concurrency patterns for updating shared data mentioned above:

1.  **`AtomicPtr` (Allocating):** A lock-free copy-on-write using `atomic.Pointer`. Reads are fast, but every update allocates a new object, creating GC pressure.
2.  **`MutexAlloc` (Allocating):** A copy-on-write protected by a `sync.RWMutex`. Similar to `AtomicPtr`, it creates garbage on every update.
3.  **`MutexInPlace` (Blocking):** In-place updates under a `sync.RWMutex` lock, alloc-free but blocking all readers during the update.

The benchmark simulates a heavy object (a `map[string]string` with 100k entries) being updated and read concurrently, with 1%, 10%, and 50% write ratios to simulate different levels of churn.

### Benchmark Results

*(go1.25.1 on an Apple M1 Pro, using 10 cores)*

#### Time per Operation (lower is better)
| Write Ratio | `poolswap` (baseline) | `AtomicPtr` (vs base) | `MutexAlloc` (vs base) | `MutexInPlace` (vs base) |
|:---:|:---:|:---:|:---:|:---:|
| **1%**  | `4.767µs` | `8.823µs` (+85%)  | `8.346µs` (+75%)   | `18.056µs` (+278%) |
| **10%** | `49.25µs` | `102.38µs` (+107%) | `96.22µs` (+95%)   | `207.41µs` (+321%) |
| **50%** | `243.3µs` | `476.3µs` (+95%)  | `492.5µs` (+102%)  | `1121.6µs` (+360%) |

#### Number of Allocations per Operation (lower is better)
| Write Ratio | `poolswap` (baseline) | `AtomicPtr` (vs base) | `MutexAlloc` (vs base) | `MutexInPlace` (vs base) |
|:---:|:---:|:---:|:---:|:---:|
| **1%**  | `0.00` | `2.00` | `2.00` | `0.00` |
| **10%** | `0.00` | `26.00`| `26.00`| `0.00` |
| **50%** | `0.00` | `146.5`| `145.0`| `0.00` |

#### Memory Allocations per Operation (lower is better)
| Write Ratio | `poolswap` (baseline) | `AtomicPtr` (vs base) | `MutexAlloc` (vs base) | `MutexInPlace` (vs base) |
|:---:|:---:|:---:|:---:|:---:|
| **1%**  | `272 B` | `52.5 KiB` (+19,166%) | `52.5 KiB` (+19,164%) | `83 B` (-69%) |
| **10%** | `2.6 KiB` | `545.7 KiB` (+20,319%)| `540.4 KiB` (+20,123%)| `962 B` (-64%)|
| **50%** | `12.6 KiB`| `2,915 KiB` (+22,949%)| `2,876 KiB` (+22,647%)| `5.3 KiB` (-57%)|

### Analysis

*   **Performance:** `poolswap` is consistently the fastest implementation, about **2x faster** than the pointer-swapping strategies (`AtomicPtr`, `MutexAlloc`) across all tested write ratios.
*   **GC Pressure:** The reason for the performance gap. `poolswap` can use a `sync.Pool` and so incurs (amortized) **zero allocations** per operation.
*   **Latency:** The `MutexInPlace` strategy never allocates but is **3-4x slower** because it forces all concurrent readers to wait while an update is in progress.

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

When the writer swaps the pointer, readers may still hold references to `oldCache`. If `oldCache` is immediately returned to the pool, a subsequent `pool.Get()` can return the same memory location while the original reader is still using it—a use-after-free race condition. This is what the reference-counting in `poolswap` fixes.

## License

MIT
