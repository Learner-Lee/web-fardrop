package main

import (
	"context"
	"testing"
	"time"
)

func TestLastReaderCanLeaveAfterFullBlockWithoutCancelingCacheAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reachedEnd := make(chan struct{})
	allowFinish := make(chan struct{})
	cache := &blockCache{
		blocks:        make(map[blockKey]*cachedBlock),
		residentLimit: 16,
		inflightLimit: 128,
		space:         make(chan struct{}),
		fetch: func(fetchCtx context.Context, _ blockKey, _ int, consume func([]byte)) error {
			consume([]byte("test"))
			close(reachedEnd)
			select {
			case <-fetchCtx.Done():
				return fetchCtx.Err()
			case <-allowFinish:
				return nil
			}
		},
	}
	key := blockKey{object: "test", version: "one"}
	block, err := cache.acquire(ctx, key, 4, false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-reachedEnd:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cache.release(block)
	close(allowFinish)

	cache.mu.Lock()
	changed := block.changed
	cache.mu.Unlock()
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.blocks[key] != block || block.lru == nil || cache.resident != 4 || cache.inflight != 0 {
		t.Fatal("the completed block was not retained in the cache")
	}
}
