package poolswap

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

const mapSize = 100_000

var (
	// Pre-compute the data to populate our heavy object. This ensures that the
	// "work" of filling the map is CPU-bound, not allocation-bound.
	precomputedKeys   []string
	precomputedValues []string
	setupOnce         sync.Once
)

func setupPrecomputedData() {
	setupOnce.Do(func() {
		precomputedKeys = make([]string, mapSize)
		precomputedValues = make([]string, mapSize)
		for i := range mapSize {
			k := strconv.Itoa(i)
			precomputedKeys[i] = k
			precomputedValues[i] = "value-" + k
		}
	})
}

// Heavy represents a config or cache object that is expensive to create.
type Heavy struct {
	Ref  // Embedded for poolswap
	Data map[string]string
}

// simulateFill populates the map from a pre-computed data source.
func (h *Heavy) simulateFill() {
	if h.Data == nil {
		h.Data = make(map[string]string, mapSize)
	}
	for i := range mapSize {
		h.Data[precomputedKeys[i]] = precomputedValues[i]
	}
}

// simulateRead accesses the data.
func (h *Heavy) simulateRead() {
	idx := rand.Intn(mapSize)
	_ = h.Data[precomputedKeys[idx]]
}

func (h *Heavy) reset() bool {
	clear(h.Data)
	return true
}

func runPoolSwap(b *testing.B, writeRatio int) {
	setupPrecomputedData()
	p := NewPool(
		func() *Heavy { return &Heavy{} },
		func(h *Heavy) bool { return h.reset() },
	)

	initObj := p.Get()
	initObj.simulateFill()
	c := NewContainer(p, initObj)

	b.RunParallel(func(pb *testing.PB) {
		iter := 0
		for pb.Next() {
			iter++
			if iter%100 < writeRatio {
				// WRITE: Get new from pool, fill, update.
				// This should have ~0 allocs/op.
				newObj := c.GetNew()
				newObj.simulateFill()
				c.Update(newObj)
			} else {
				// READ: Acquire, read, release.
				obj := c.Acquire()
				if obj != nil {
					obj.simulateRead()
					c.Release(obj)
				}
			}
		}
	})
}

// 2. Atomic Pointer (Allocating)
func runAtomicPointer(b *testing.B, writeRatio int) {
	setupPrecomputedData()
	var ptr atomic.Pointer[Heavy]

	h := &Heavy{}
	h.simulateFill()
	ptr.Store(h)

	b.RunParallel(func(pb *testing.PB) {
		iter := 0
		for pb.Next() {
			iter++
			if iter%100 < writeRatio {
				// WRITE: Allocate new object, Fill, Swap.
				newObj := &Heavy{}
				newObj.simulateFill()
				ptr.Store(newObj)
			} else {
				// READ: Load, Read.
				obj := ptr.Load()
				obj.simulateRead()
			}
		}
	})
}

// 3. RWMutex (Allocating)
func runRWMutexAlloc(b *testing.B, writeRatio int) {
	setupPrecomputedData()
	var mu sync.RWMutex
	current := &Heavy{}
	current.simulateFill()

	b.RunParallel(func(pb *testing.PB) {
		iter := 0
		for pb.Next() {
			iter++
			if iter%100 < writeRatio {
				// WRITE: Allocate new object, Fill, Lock, Swap.
				newObj := &Heavy{}
				newObj.simulateFill()

				mu.Lock()
				current = newObj
				mu.Unlock()
			} else {
				// READ: RLock, grab ptr, RUnlock, Read.
				mu.RLock()
				obj := current
				mu.RUnlock()
				obj.simulateRead()
			}
		}
	})
}

// 4. RWMutex (In-Place)
func runRWMutexInPlace(b *testing.B, writeRatio int) {
	setupPrecomputedData()
	var mu sync.RWMutex
	current := &Heavy{}
	current.simulateFill()

	b.RunParallel(func(pb *testing.PB) {
		iter := 0
		for pb.Next() {
			iter++
			if iter%100 < writeRatio {
				// WRITE: Lock, Reset, Fill, Unlock.
				// This has ~0 allocs but blocks all readers during the fill.
				mu.Lock()
				current.reset()
				current.simulateFill()
				mu.Unlock()
			} else {
				// READ: RLock, Read, RUnlock.
				mu.RLock()
				current.simulateRead()
				mu.RUnlock()
			}
		}
	})
}

func BenchmarkHotSwap(b *testing.B) {
	scenarios := []struct {
		name string
		fn   func(*testing.B, int)
	}{
		{"PoolSwap", runPoolSwap},
		{"AtomicPtr", runAtomicPointer},
		{"MutexAlloc", runRWMutexAlloc},
		{"MutexInPlace", runRWMutexInPlace},
	}

	ratios := []int{1, 10, 50}

	for _, r := range ratios {
		for _, sc := range scenarios {
			testName := fmt.Sprintf("impl=%s/writes=%02d", sc.name, r)
			b.Run(testName, func(b *testing.B) {
				b.ReportAllocs()
				sc.fn(b, r)
			})
		}
	}
}
