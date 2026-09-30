package main

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// make_test_torrent builds an in-memory torrent of num_pieces pieces whose
// last piece is short, and returns it with the piece data it describes.
func make_test_torrent(num_pieces, piece_length, last_length int) (*torrent_file, [][]byte) {
	rng := rand.New(rand.NewSource(1))

	tf := &torrent_file{
		name:         "test.bin",
		piece_length: piece_length,
		length:       piece_length*(num_pieces-1) + last_length,
	}
	copy(tf.info_hash[:], "resume-test-infohash")

	pieces := make([][]byte, num_pieces)
	for i := range pieces {
		size := piece_length
		if i == num_pieces-1 {
			size = last_length
		}
		pieces[i] = make([]byte, size)
		rng.Read(pieces[i])
		tf.piece_hashes = append(tf.piece_hashes, sha1.Sum(pieces[i]))
	}
	return tf, pieces
}

// write_test_file stores every piece of tf into a fresh file in dir.
func write_test_file(t *testing.T, dir string, tf *torrent_file, pieces [][]byte) (*storage, string) {
	t.Helper()

	path := filepath.Join(dir, "test.bin")
	store, err := open_storage(path, tf.length, tf.piece_length)
	if err != nil {
		t.Fatalf("open_storage: %v", err)
	}
	t.Cleanup(func() { store.close() })

	for i, data := range pieces {
		if err := store.write_piece(i, data); err != nil {
			t.Fatalf("write_piece %d: %v", i, err)
		}
	}
	return store, path
}

func TestResumeRoundTrip(t *testing.T) {
	tf, _ := make_test_torrent(20, 64, 10)
	path := filepath.Join(t.TempDir(), "out.state")

	have := make(bitfield, 3)
	for _, i := range []int{0, 3, 9, 19} {
		have.set_piece(i)
	}

	if err := save_resume(path, tf, have); err != nil {
		t.Fatalf("save_resume: %v", err)
	}
	got, err := load_resume(path, tf)
	if err != nil {
		t.Fatalf("load_resume: %v", err)
	}
	if !bytes.Equal(got, have) {
		t.Errorf("loaded bitfield % x, want % x", got, have)
	}

	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp file left behind after save")
	}
}

func TestLoadResumeMissingFile(t *testing.T) {
	tf, _ := make_test_torrent(4, 64, 64)

	got, err := load_resume(filepath.Join(t.TempDir(), "none.state"), tf)
	if err != nil || got != nil {
		t.Errorf("load_resume on missing file = %v, %v; want nil, nil", got, err)
	}
}

func TestLoadResumeRejectsOtherTorrent(t *testing.T) {
	tf, _ := make_test_torrent(4, 64, 64)
	path := filepath.Join(t.TempDir(), "out.state")

	if err := save_resume(path, tf, make(bitfield, 1)); err != nil {
		t.Fatalf("save_resume: %v", err)
	}

	other := *tf
	copy(other.info_hash[:], "a-different-torrent!")
	if _, err := load_resume(path, &other); !errors.Is(err, err_state_mismatch) {
		t.Errorf("load_resume with other info hash: err = %v, want err_state_mismatch", err)
	}
}

func TestLoadResumeRejectsWrongLayout(t *testing.T) {
	tf, _ := make_test_torrent(4, 64, 64)
	path := filepath.Join(t.TempDir(), "out.state")

	if err := save_resume(path, tf, make(bitfield, 1)); err != nil {
		t.Fatalf("save_resume: %v", err)
	}

	resized := *tf
	resized.piece_length = 128
	if _, err := load_resume(path, &resized); err == nil {
		t.Error("expected an error when piece length differs")
	}
}

func TestLoadResumeRejectsCorruptFile(t *testing.T) {
	tf, _ := make_test_torrent(4, 64, 64)
	path := filepath.Join(t.TempDir(), "out.state")

	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := load_resume(path, tf); err == nil {
		t.Error("expected an error for a corrupt state file")
	}
}

func TestVerifyPiecesSkipsCorruptPiece(t *testing.T) {
	tf, pieces := make_test_torrent(10, 64, 30)
	store, _ := write_test_file(t, t.TempDir(), tf, pieces)

	// damage piece 4 on disk
	if err := store.write_piece(4, bytes.Repeat([]byte{0xFF}, 64)); err != nil {
		t.Fatal(err)
	}

	candidates := make(bitfield, 2)
	for i := 0; i < 10; i++ {
		candidates.set_piece(i)
	}

	state := new_piece_state(10)
	if got := verify_pieces(tf, store, candidates, state); got != 9 {
		t.Errorf("verify_pieces = %d valid, want 9", got)
	}
	if state.has(4) {
		t.Error("corrupt piece 4 was marked done")
	}
	for _, i := range []int{0, 3, 5, 9} {
		if !state.has(i) {
			t.Errorf("good piece %d not marked done", i)
		}
	}
}

func TestVerifyPiecesOnlyChecksCandidates(t *testing.T) {
	tf, pieces := make_test_torrent(10, 64, 30)
	store, _ := write_test_file(t, t.TempDir(), tf, pieces)

	candidates := make(bitfield, 2)
	candidates.set_piece(2)
	candidates.set_piece(7)

	state := new_piece_state(10)
	if got := verify_pieces(tf, store, candidates, state); got != 2 {
		t.Errorf("verify_pieces = %d valid, want 2", got)
	}
	if state.count() != 2 {
		t.Errorf("state.count() = %d, want 2", state.count())
	}
}

func TestRestoreProgressUsesStateFile(t *testing.T) {
	tf, pieces := make_test_torrent(10, 64, 30)
	store, path := write_test_file(t, t.TempDir(), tf, pieces)

	// the state file only claims half the pieces, so only those count
	have := make(bitfield, 2)
	for _, i := range []int{0, 1, 2, 3, 9} {
		have.set_piece(i)
	}
	if err := save_resume(state_path(path), tf, have); err != nil {
		t.Fatal(err)
	}

	state := new_piece_state(10)
	if got := restore_progress(tf, store, state, path, true); got != 5 {
		t.Errorf("restore_progress = %d, want 5", got)
	}
}

func TestRestoreProgressRechecksWithoutStateFile(t *testing.T) {
	tf, pieces := make_test_torrent(10, 64, 30)
	store, path := write_test_file(t, t.TempDir(), tf, pieces)

	state := new_piece_state(10)
	if got := restore_progress(tf, store, state, path, true); got != 10 {
		t.Errorf("restore_progress = %d, want 10 (full recheck)", got)
	}
}

func TestRestoreProgressFreshDownload(t *testing.T) {
	tf, _ := make_test_torrent(10, 64, 30)
	path := filepath.Join(t.TempDir(), "test.bin")
	store, err := open_storage(path, tf.length, tf.piece_length)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()

	state := new_piece_state(10)
	if got := restore_progress(tf, store, state, path, false); got != 0 {
		t.Errorf("restore_progress = %d, want 0", got)
	}
}

func TestBytesLeft(t *testing.T) {
	tf, _ := make_test_torrent(4, 64, 10)
	state := new_piece_state(4)

	if got := tf.bytes_left(state); got != 64*3+10 {
		t.Errorf("bytes_left = %d, want %d", got, 64*3+10)
	}

	state.mark_done(0)
	state.mark_done(3) // the short last piece
	if got := tf.bytes_left(state); got != 128 {
		t.Errorf("bytes_left = %d, want 128", got)
	}
}

func TestResumeSaverThrottlesAndFlushes(t *testing.T) {
	tf, pieces := make_test_torrent(10, 64, 30)
	store, path := write_test_file(t, t.TempDir(), tf, pieces)

	state := new_piece_state(10)
	saver := new_resume_saver(state_path(path), tf, store, state)

	state.mark_done(1)
	saver.piece_done(1) // first save goes straight to disk

	state.mark_done(2)
	saver.piece_done(2) // within the interval, only marked dirty

	got, err := load_resume(state_path(path), tf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.has_piece(1) || got.has_piece(2) {
		t.Errorf("after throttled save: has(1)=%v has(2)=%v, want true false", got.has_piece(1), got.has_piece(2))
	}

	if err := saver.flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	got, err = load_resume(state_path(path), tf)
	if err != nil {
		t.Fatal(err)
	}
	if !got.has_piece(2) {
		t.Error("flush did not save piece 2")
	}
}
