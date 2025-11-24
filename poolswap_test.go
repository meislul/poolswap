package poolswap_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keilerkonzept/poolswap"

	"pgregory.net/rapid"
)

type MockPayload struct {
	poolswap.Ref

	ID       int64
	Recycled atomic.Bool // used to detect use-after-free
	Content  []byte
}

func newMockPool() *poolswap.Pool[MockPayload, *MockPayload] {
	var idCounter atomic.Int64
	return poolswap.NewPool(
		func() *MockPayload {
			id := idCounter.Add(1)
			return &MockPayload{
				ID:      id,
				Content: make([]byte, 0, 10),
			}
		},
		func(obj *MockPayload) bool {
			obj.Recycled.Store(true)
			obj.Content = obj.Content[:0]
			return true
		},
	)
}

func TestLifecycle(t *testing.T) {
	pool := newMockPool()

	obj := pool.Get()
	obj.Recycled.Store(false)
	if obj.DebugPeekRef() != 1 {
		t.Errorf("New object should have Ref=1, got %d", obj.DebugPeekRef())
	}

	resetCalled := false
	pool.Reset = func(_ *MockPayload) bool {
		resetCalled = true
		return true
	}

	pool.Release(obj)

	if !resetCalled {
		t.Error("Reset was not called after Releasing new object")
	}
}

func TestStress_NoUseAfterFree(t *testing.T) {
	pool := newMockPool()
	container := poolswap.NewEmptyContainer(pool)

	initial := container.GetNew()
	initial.Recycled.Store(false)
	container.Update(initial)

	var wg sync.WaitGroup
	start := make(chan struct{})
	done := make(chan struct{})

	// writer
	wg.Go(func() {
		<-start
		for {
			select {
			case <-done:
				return
			default:
				newObj := container.GetNew()
				newObj.Recycled.Store(false)
				container.Update(newObj)
				time.Sleep(time.Microsecond)
			}
		}
	})

	readers := 10
	for range readers {
		wg.Go(func() {
			<-start
			for {
				select {
				case <-done:
					return
				default:
					obj := container.Acquire()
					if obj == nil {
						continue
					}

					if obj.Recycled.Load() {
						t.Fatal("Race condition detected: Object recycled while held by reader!")
					}

					container.Release(obj)
				}
			}
		})
	}

	close(start)
	time.Sleep(2 * time.Second)
	close(done)
	wg.Wait()
}

func TestProp_SequentialLogic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		type modelState struct {
			activeID  int64
			heldIDs   []int64
			refCounts map[int64]int // id -> refCount
		}

		state := modelState{
			activeID:  0,
			heldIDs:   make([]int64, 0),
			refCounts: make(map[int64]int),
		}

		pool := poolswap.NewPool(
			func() *MockPayload { return &MockPayload{} },
			func(_ *MockPayload) bool { return true },
		)
		container := poolswap.NewEmptyContainer(pool)

		realObjects := make(map[int64]*MockPayload)
		var nextID atomic.Int64

		incRef := func(id int64, delta int) {
			state.refCounts[id] += delta
		}

		t.Repeat(map[string]func(*rapid.T){
			"Update": func(_ *rapid.T) {
				newObj := container.GetNew()
				newObj.Recycled.Store(false)
				newObj.ID = nextID.Add(1)
				realObjects[newObj.ID] = newObj

				// model update
				state.refCounts[newObj.ID] = 1
				if state.activeID != 0 {
					incRef(state.activeID, -1)
				}

				container.Update(newObj)

				// model update
				state.activeID = newObj.ID
			},
			"Acquire": func(t *rapid.T) {
				obj := container.Acquire()

				if state.activeID == 0 {
					if obj != nil {
						t.Fatalf("Model says empty, but got object ID %d", obj.ID)
					}
					return
				}

				if obj == nil {
					t.Fatalf("Model says active %d, but got nil", state.activeID)
					return
				}

				if obj.ID != state.activeID {
					t.Fatalf("Acquired wrong object. Want %d, Got %d", state.activeID, obj.ID)
				}

				// model update
				state.heldIDs = append(state.heldIDs, obj.ID)
				incRef(obj.ID, 1)
			},
			"Release": func(t *rapid.T) {
				if len(state.heldIDs) == 0 {
					t.Skip("Nothing to release")
					return
				}

				idx := rapid.IntRange(0, len(state.heldIDs)-1).Draw(t, "releaseIdx")
				idToRelease := state.heldIDs[idx]
				state.heldIDs = append(state.heldIDs[:idx], state.heldIDs[idx+1:]...)
				realObj := realObjects[idToRelease]

				container.Release(realObj)

				// model update
				incRef(idToRelease, -1)
			},
			"CheckRefCounts": func(t *rapid.T) {
				// verify every known alive object matches the model's ref count
				for id, expectedRef := range state.refCounts {
					if expectedRef > 0 {
						realObj, exists := realObjects[id]
						if !exists {
							t.Fatalf("ID %d missing from real objects map", id)
						}

						gotRef := realObj.DebugPeekRef()
						if int(gotRef) != expectedRef {
							t.Fatalf("Ref mismatch for ID %d. Want %d, Got %d", id, expectedRef, gotRef)
						}
					}
				}
			},
		})
	})
}
