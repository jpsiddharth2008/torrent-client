package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// how often, at most, progress is written to the state file while downloading
const resume_save_interval = time.Second

var err_state_mismatch = errors.New("state file belongs to a different torrent")

// resume_file is the on-disk format of <output>.state. It records which
// pieces were written and verified so an interrupted download can continue
// where it stopped instead of starting over.
type resume_file struct {
	InfoHash    string    `json:"info_hash"`
	PieceLength int       `json:"piece_length"`
	NumPieces   int       `json:"num_pieces"`
	Have        []byte    `json:"have"` // bitfield, saved as base64 by encoding/json
	UpdatedAt   time.Time `json:"updated_at"`
}

func state_path(output_path string) string {
	return output_path + ".state"
}

func save_resume(path string, t *torrent_file, have bitfield) error {
	rf := resume_file{
		InfoHash:    hex.EncodeToString(t.info_hash[:]),
		PieceLength: t.piece_length,
		NumPieces:   len(t.piece_hashes),
		Have:        have,
		UpdatedAt:   time.Now().UTC(),
	}

	data, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return err
	}

	// write a temp file and rename it over the old one, so a crash mid-write
	// leaves either the previous state or the new one, never a torn file
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// load_resume returns the saved bitfield, or nil with no error if there is no
// state file yet. A state file for another torrent returns err_state_mismatch.
func load_resume(path string, t *torrent_file) (bitfield, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var rf resume_file
	if err := json.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("corrupt state file: %w", err)
	}

	if rf.InfoHash != hex.EncodeToString(t.info_hash[:]) {
		return nil, err_state_mismatch
	}

	num_pieces := len(t.piece_hashes)
	if rf.PieceLength != t.piece_length || rf.NumPieces != num_pieces || len(rf.Have) != (num_pieces+7)/8 {
		return nil, fmt.Errorf("state file layout does not match torrent")
	}

	return bitfield(rf.Have), nil
}

// verify_pieces re-hashes every candidate piece on disk and marks the ones
// that match the torrent's SHA-1 as done. The state file is only a hint: a
// crash between writing data and saving state, or an edited output file, must
// not let a bad piece through. Unreadable or mismatched pieces are left
// missing so they get downloaded again.
func verify_pieces(t *torrent_file, store *storage, candidates bitfield, state *piece_state) int {
	indices := make(chan int)
	var valid atomic.Int32
	var wg sync.WaitGroup

	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range indices {
				data, err := store.read_piece(index)
				if err != nil {
					log.Printf("recheck: could not read piece %d: %v\n", index, err)
					continue
				}
				if sha1.Sum(data) != t.piece_hashes[index] {
					continue
				}
				state.mark_done(index)
				valid.Add(1)
			}
		}()
	}

	for index := range t.piece_hashes {
		if candidates.has_piece(index) {
			indices <- index
		}
	}
	close(indices)
	wg.Wait()

	return int(valid.Load())
}

// restore_progress marks every piece already on disk and verified as done.
// existed says whether the output file was present before this run opened it:
// without a usable state file an existing file is fully rechecked, the same
// as a "force recheck" in other clients.
func restore_progress(t *torrent_file, store *storage, state *piece_state, output_path string, existed bool) int {
	num_pieces := len(t.piece_hashes)

	candidates, err := load_resume(state_path(output_path), t)
	if err != nil {
		log.Printf("ignoring state file: %v\n", err)
		candidates = nil
	}

	if candidates == nil {
		if !existed {
			return 0
		}
		log.Printf("no usable state file, rechecking existing %s\n", output_path)
		candidates = make(bitfield, (num_pieces+7)/8)
		for index := 0; index < num_pieces; index++ {
			candidates.set_piece(index)
		}
	}

	start := time.Now()
	valid := verify_pieces(t, store, candidates, state)
	log.Printf("resumed: %d/%d pieces verified from disk in %v\n", valid, num_pieces, time.Since(start).Round(time.Millisecond))
	return valid
}

// bytes_left is what the tracker's "left" parameter should report.
func (t *torrent_file) bytes_left(state *piece_state) int {
	left := 0
	for index := range t.piece_hashes {
		if !state.has(index) {
			left += t.piece_length_at(index)
		}
	}
	return left
}

// resume_saver writes the state file as pieces complete, at most once per
// resume_save_interval, and on demand via flush (on exit or Ctrl+C).
type resume_saver struct {
	mu    sync.Mutex
	path  string
	t     *torrent_file
	store *storage
	state *piece_state
	last  time.Time
	dirty bool
}

func new_resume_saver(path string, t *torrent_file, store *storage, state *piece_state) *resume_saver {
	return &resume_saver{path: path, t: t, store: store, state: state}
}

// piece_done is called after a piece is written and marked done.
func (rs *resume_saver) piece_done(index int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	rs.dirty = true
	if time.Since(rs.last) < resume_save_interval {
		return
	}
	if err := rs.save_locked(); err != nil {
		log.Printf("could not save resume state: %v\n", err)
	}
}

// flush saves any progress not yet written to the state file.
func (rs *resume_saver) flush() error {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if !rs.dirty {
		return nil
	}
	return rs.save_locked()
}

func (rs *resume_saver) save_locked() error {
	// snapshot first, then flush the data file: every piece in the snapshot
	// was written before it was marked done, so after the sync the state
	// file never claims a piece that is not durably on disk
	have := rs.state.snapshot()
	if err := rs.store.sync(); err != nil {
		return err
	}
	if err := save_resume(rs.path, rs.t, have); err != nil {
		return err
	}
	rs.last = time.Now()
	rs.dirty = false
	return nil
}
