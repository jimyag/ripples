package impact

import (
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
)

func TestParallelForRunsEachItemOnce(t *testing.T) {
	previous := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previous)

	const count = 32
	calls := make([]atomic.Int32, count)
	var active atomic.Int32
	var maximum atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 2)

	done := make(chan error, 1)
	go func() {
		done <- parallelFor(count, func(index int) error {
			calls[index].Add(1)
			current := active.Add(1)
			for {
				observed := maximum.Load()
				if current <= observed || maximum.CompareAndSwap(observed, current) {
					break
				}
			}
			if index < 2 {
				started <- struct{}{}
				<-release
			}
			active.Add(-1)
			return nil
		})
	}()

	<-started
	<-started
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("parallelFor() error = %v", err)
	}
	if maximum.Load() < 2 {
		t.Fatalf("maximum concurrency = %d, want at least 2", maximum.Load())
	}
	for index := range count {
		if got := calls[index].Load(); got != 1 {
			t.Fatalf("calls[%d] = %d, want 1", index, got)
		}
	}
}

func TestParallelForReturnsErrorsInInputOrder(t *testing.T) {
	first := errors.New("first")
	second := errors.New("second")

	err := parallelFor(4, func(index int) error {
		switch index {
		case 1:
			return first
		case 3:
			return second
		default:
			return nil
		}
	})
	if !errors.Is(err, first) {
		t.Fatalf("parallelFor() error = %v, want %v", err, first)
	}
}
