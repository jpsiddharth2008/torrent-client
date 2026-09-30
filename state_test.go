package main

import (
	"testing"
)

func TestPieceState(t *testing.T) {
	numPieces := 10
	ps := new_piece_state(numPieces)

	if ps.count() != 0 {
		t.Errorf("expected initial count 0, got %d", ps.count())
	}

	ps.mark_done(1)
	ps.mark_done(5)

	if !ps.has(1) || !ps.has(5) {
		t.Errorf("expected pieces 1 and 5 to be marked done")
	}
	if ps.has(0) || ps.has(2) {
		t.Errorf("piece marked done unexpectedly")
	}

	if ps.count() != 2 {
		t.Errorf("expected count 2, got %d", ps.count())
	}

	snap := ps.snapshot()
	snap[0] = 0xFF
	if ps.has(0) {
		t.Errorf("snapshot modification mutated internal state")
	}
}