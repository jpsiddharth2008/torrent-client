package main

import (
	"sync"
	"sync/atomic"
	"time"
)

type piece_state struct {
	mu         sync.Mutex
	have       bitfield
	num_pieces int
}

func new_piece_state(num_pieces int) *piece_state {
	return &piece_state{
		have:       make(bitfield, (num_pieces+7)/8),
		num_pieces: num_pieces,
	}
}

func (p *piece_state) mark_done(index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.have.set_piece(index)
}

func (p *piece_state) has(index int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.have.has_piece(index)
}

func (p *piece_state) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	c := 0
	for i := 0; i < p.num_pieces; i++ {
		if p.have.has_piece(i) {
			c++
		}
	}
	return c
}

func (p *piece_state) snapshot() bitfield {
	p.mu.Lock()
	defer p.mu.Unlock()

	cp := make(bitfield, len(p.have))
	copy(cp, p.have)
	return cp
}

type stats struct {
	downloaded   atomic.Int64
	uploaded     atomic.Int64
	active_peers atomic.Int32 // workers connected to a peer right now
	known_peers  atomic.Int32 // peers in the latest tracker response
	started      time.Time
}