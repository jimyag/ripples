package impact

import (
	"runtime"
	"sync"
)

func parallelFor(count int, run func(int) error) error {
	if count == 0 {
		return nil
	}

	workers := min(runtime.GOMAXPROCS(0), count)
	jobs := make(chan int)
	errors := make([]error, count)

	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			for index := range jobs {
				errors[index] = run(index)
			}
		}()
	}
	for index := range count {
		jobs <- index
	}
	close(jobs)
	wait.Wait()

	// Return errors in input order so concurrency does not change diagnostics.
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}
