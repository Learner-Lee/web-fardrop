package main

import (
	"container/list"
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

type blockKey struct {
	object  string
	version string
	index   int64
}

type cachedBlock struct {
	key      blockKey
	data     []byte
	size     int
	received int
	refs     int
	ready    bool
	err      error
	changed  chan struct{}
	cancel   context.CancelFunc
	lru      *list.Element
}

type blockCache struct {
	mu                           sync.Mutex
	blocks                       map[blockKey]*cachedBlock
	lru                          list.List
	resident, inflight           int
	residentLimit, inflightLimit int
	space                        chan struct{}
	fetch                        func(context.Context, blockKey, int, func([]byte)) error
}

func (c *blockCache) acquire(ctx context.Context, key blockKey, size int, prefetch bool) (*cachedBlock, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if block := c.blocks[key]; block != nil {
			block.refs++
			if block.lru != nil {
				c.lru.MoveToFront(block.lru)
			}
			c.mu.Unlock()
			return block, nil
		}
		budget := c.inflightLimit
		if prefetch {
			budget /= 2
		}
		if c.inflight+size <= budget {
			fetchCtx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			block := &cachedBlock{key: key, size: size, refs: 1, changed: make(chan struct{}), cancel: cancel, data: make([]byte, size)}
			c.blocks[key] = block
			c.inflight += size
			c.mu.Unlock()
			go c.load(fetchCtx, block)
			return block, nil
		}
		space := c.space
		c.mu.Unlock()
		if prefetch {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-space:
		}
	}
}

func (c *blockCache) signalSpace() {
	close(c.space)
	c.space = make(chan struct{})
}

func (c *blockCache) load(ctx context.Context, block *cachedBlock) {
	defer block.cancel()
	err := c.fetch(ctx, block.key, block.size, func(data []byte) {
		c.mu.Lock()
		block.received += copy(block.data[block.received:], data)
		close(block.changed)
		block.changed = make(chan struct{})
		c.mu.Unlock()
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && block.received != block.size {
		err = io.ErrUnexpectedEOF
	}
	block.err, block.ready = err, true
	if err == nil && c.blocks[block.key] == block {
		for item := c.lru.Back(); c.resident+block.size > c.residentLimit && item != nil; {
			candidate := item.Value.(*cachedBlock)
			item = item.Prev()
			if candidate.refs == 0 {
				c.lru.Remove(candidate.lru)
				delete(c.blocks, candidate.key)
				c.resident -= candidate.size
				candidate.data = nil
			}
		}
		if c.resident+block.size <= c.residentLimit {
			block.lru = c.lru.PushFront(block)
			c.resident += block.size
			c.inflight -= block.size
			c.signalSpace()
		}
	}
	if block.refs == 0 && block.lru == nil {
		c.drop(block)
	}
	close(block.changed)
}

func (c *blockCache) drop(block *cachedBlock) {
	if c.blocks[block.key] == block {
		delete(c.blocks, block.key)
	}
	c.inflight -= block.size
	block.data = nil
	c.signalSpace()
}

func (c *blockCache) release(block *cachedBlock) {
	if block == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	block.refs--
	if block.refs != 0 || block.lru != nil {
		return
	}
	if block.ready {
		c.drop(block)
		return
	}
	if block.received == block.size {
		return
	}
	if c.blocks[block.key] == block {
		delete(c.blocks, block.key)
	}
	block.cancel()
}

func (c *blockCache) read(ctx context.Context, block *cachedBlock, p []byte, offset int) (int, error) {
	for {
		c.mu.Lock()
		if block.ready && block.err != nil {
			c.mu.Unlock()
			return 0, block.err
		}
		if offset < block.received {
			n := copy(p, block.data[offset:block.received])
			c.mu.Unlock()
			return n, nil
		}
		changed := block.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-changed:
		}
	}
}

type download struct {
	ctx           context.Context
	file          File
	cache         *blockCache
	pos           int64
	current, next *cachedBlock
	readInBlock   int
}

func (d *download) Close() {
	d.cache.release(d.current)
	d.cache.release(d.next)
	d.current, d.next = nil, nil
	d.readInBlock = 0
}

func (d *download) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += d.pos
	case io.SeekEnd:
		offset += d.file.Size
	default:
		return 0, errors.New("invalid seek origin")
	}
	if offset < 0 {
		return 0, errors.New("negative seek offset")
	}
	if offset != d.pos && d.current != nil {
		d.Close()
	}
	d.pos = offset
	return offset, nil
}

func (d *download) Read(p []byte) (int, error) {
	if d.pos >= d.file.Size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	index := d.pos / cacheChunkSize
	if d.current == nil || d.current.key.index != index {
		d.cache.release(d.current)
		d.current, d.next = d.next, nil
		d.readInBlock = 0
		if d.current == nil {
			block, err := d.acquire(index, false)
			if err != nil {
				return 0, err
			}
			d.current = block
		}
	}
	n, err := d.cache.read(d.ctx, d.current, p, int(d.pos%cacheChunkSize))
	d.pos += int64(n)
	d.readInBlock += n
	if err == nil && d.next == nil && d.readInBlock >= 1<<20 && d.pos-int64(index)*cacheChunkSize >= cacheChunkSize/2 &&
		(index+1)*cacheChunkSize < d.file.Size {
		d.next, _ = d.acquire(index+1, true)
	}
	return n, err
}

func (d *download) acquire(index int64, prefetch bool) (*cachedBlock, error) {
	length := min(int64(cacheChunkSize), d.file.Size-index*cacheChunkSize)
	return d.cache.acquire(d.ctx, blockKey{d.file.ID, d.file.SHA256, index}, int(length), prefetch)
}
