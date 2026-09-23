package beam

import (
	"context"
	"fmt"
	"runtime"
	"sync"
)

// ScanCompiledEvidence reads BEAM files concurrently and delivers results to
// one bounded consumer. The callback is never called concurrently.
func ScanCompiledEvidence(ctx context.Context, paths []string, workers int, consume func(string, CompiledEvidence, error) error) error {
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	if workers > len(paths) {
		workers = len(paths)
	}
	if workers == 0 {
		return nil
	}
	type result struct {
		path     string
		evidence CompiledEvidence
		err      error
	}
	pathCh := make(chan string, workers)
	resultCh := make(chan result, workers*2)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case path, ok := <-pathCh:
					if !ok {
						return
					}
					evidence, err := ReadCompiledEvidence(path)
					select {
					case resultCh <- result{path: path, evidence: evidence, err: err}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	go func() {
		defer close(pathCh)
		for _, path := range paths {
			select {
			case pathCh <- path:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		group.Wait()
		close(resultCh)
	}()
	var callbackErr error
	for result := range resultCh {
		if callbackErr != nil {
			continue
		}
		if err := consume(result.path, result.evidence, result.err); err != nil {
			callbackErr = fmt.Errorf("consume %s: %w", result.path, err)
			cancel()
		}
	}
	if callbackErr == nil {
		callbackErr = ctx.Err()
	}
	return callbackErr
}
