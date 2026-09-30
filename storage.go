package main

import (
	"fmt"
	"os"
	"sync"
)

type storage struct {
	mu           sync.Mutex
	f            *os.File
	length       int // total file size
	piece_length int
}

func open_storage(path string, length, piece_length int) (*storage, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}

	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	// a non-empty file of another size is not a download of this torrent;
	// resizing it would silently destroy whatever it is
	if stat.Size() != 0 && stat.Size() != int64(length) {
		f.Close()
		return nil, fmt.Errorf("%s already exists with size %d, expected %d; refusing to overwrite it", path, stat.Size(), length)
	}

	if stat.Size() != int64(length) {
		if err := f.Truncate(int64(length)); err != nil {
			f.Close()
			return nil, fmt.Errorf("failed to truncate file: %w", err)
		}
	}

	return &storage{
		f:            f,
		length:       length,
		piece_length: piece_length,
	}, nil
}

func (s *storage) piece_len_at(index int) int {
	numPieces := (s.length + s.piece_length - 1) / s.piece_length
	if index < 0 || index >= numPieces {
		return 0
	}
	if index == numPieces-1 {
		rem := s.length % s.piece_length
		if rem != 0 {
			return rem
		}
	}
	return s.piece_length
}

func (s *storage) write_piece(index int, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	offset := int64(index) * int64(s.piece_length)
	_, err := s.f.WriteAt(data, offset)
	if err != nil {
		return fmt.Errorf("failed to write piece %d at offset %d: %w", index, offset, err)
	}
	return nil
}

func (s *storage) read_piece(index int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	pLen := s.piece_len_at(index)
	if pLen == 0 {
		return nil, fmt.Errorf("invalid piece index: %d", index)
	}

	buf := make([]byte, pLen)
	offset := int64(index) * int64(s.piece_length)
	_, err := s.f.ReadAt(buf, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to read piece %d: %w", index, err)
	}
	return buf, nil
}

func (s *storage) read_block(index, begin, length int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	numPieces := (s.length + s.piece_length - 1) / s.piece_length
	if index < 0 || index >= numPieces {
		return nil, fmt.Errorf("index out of range: %d", index)
	}
	if begin < 0 || length <= 0 {
		return nil, fmt.Errorf("invalid begin (%d) or length (%d)", begin, length)
	}

	pLen := s.piece_len_at(index)
	if begin+length > pLen {
		return nil, fmt.Errorf("block [%d:%d] exceeds piece length %d", begin, begin+length, pLen)
	}

	buf := make([]byte, length)
	offset := int64(index)*int64(s.piece_length) + int64(begin)
	_, err := s.f.ReadAt(buf, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to read block at offset %d: %w", offset, err)
	}
	return buf, nil
}

// sync flushes written pieces from the OS cache to the disk.
func (s *storage) sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Sync()
}

func (s *storage) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}