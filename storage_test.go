package main

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestStorageOutOrderAndShortPiece(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.iso")

	totalLen := 700 * 1024
	pieceLen := 256 * 1024

	st, err := open_storage(path, totalLen, pieceLen)
	if err != nil {
		t.Fatalf("open_storage failed: %v", err)
	}
	defer st.close()

	piece0 := bytes.Repeat([]byte{0xAA}, pieceLen)
	piece1 := bytes.Repeat([]byte{0xBB}, pieceLen)
	shortLen := totalLen - 2*pieceLen
	piece2 := bytes.Repeat([]byte{0xCC}, shortLen)

	if err := st.write_piece(2, piece2); err != nil {
		t.Fatalf("write_piece 2 failed: %v", err)
	}
	if err := st.write_piece(0, piece0); err != nil {
		t.Fatalf("write_piece 0 failed: %v", err)
	}
	if err := st.write_piece(1, piece1); err != nil {
		t.Fatalf("write_piece 1 failed: %v", err)
	}

	r0, err := st.read_piece(0)
	if err != nil || !bytes.Equal(r0, piece0) {
		t.Fatalf("read_piece 0 failed or mismatched: %v", err)
	}

	r2, err := st.read_piece(2)
	if err != nil || len(r2) != shortLen || !bytes.Equal(r2, piece2) {
		t.Fatalf("read_piece 2 short piece error or length mismatch")
	}

	blk, err := st.read_block(1, 10, 100)
	if err != nil || !bytes.Equal(blk, piece1[10:110]) {
		t.Fatalf("read_block mismatch: %v", err)
	}
}

func TestStorageReadBlockValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.iso")

	totalLen := 500 * 1024
	pieceLen := 256 * 1024

	st, err := open_storage(path, totalLen, pieceLen)
	if err != nil {
		t.Fatalf("open_storage failed: %v", err)
	}
	defer st.close()

	tests := []struct {
		name   string
		index  int
		begin  int
		length int
	}{
		{"negative index", -1, 0, 16384},
		{"out of range index", 5, 0, 16384},
		{"negative begin", 0, -10, 16384},
		{"zero length", 0, 0, 0},
		{"negative length", 0, 0, -5},
		{"exceeds short piece bounds", 1, 240000, 16384},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := st.read_block(tt.index, tt.begin, tt.length); err == nil {
				t.Errorf("expected error for %s, got nil", tt.name)
			}
		})
	}
}