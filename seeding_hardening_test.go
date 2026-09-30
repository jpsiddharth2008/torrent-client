package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// seed_fixture is a one-piece torrent on disk; the piece is marked verified
// only when verified is true.
func seed_fixture(t *testing.T, verified bool) (*torrent_file, *storage, *piece_state) {
	t.Helper()

	const piece_length = 64 * 1024
	store, err := open_storage(filepath.Join(t.TempDir(), "seed.bin"), piece_length, piece_length)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.close() })

	if err := store.write_piece(0, bytes.Repeat([]byte{0xAB}, piece_length)); err != nil {
		t.Fatal(err)
	}

	state := new_piece_state(1)
	if verified {
		state.mark_done(0)
	}

	tf := &torrent_file{length: piece_length, piece_length: piece_length}
	copy(tf.info_hash[:], "seeding-test-hash-01")
	return tf, store, state
}

// seed_handshake performs the downloader's side of the handshake and reads
// the seeder's bitfield and unchoke.
func seed_handshake(t *testing.T, conn net.Conn, info_hash [20]byte) {
	t.Helper()

	var peer_id [20]byte
	copy(peer_id[:], "-TEST01-000000000000")
	if _, err := conn.Write(new_handshake(info_hash, peer_id).serialize()); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	if _, err := read_handshake(conn); err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	for _, want := range []message_id{msg_bitfield, msg_unchoke} {
		msg, err := read_message(conn)
		if err != nil || msg == nil || msg.id != want {
			t.Fatalf("expected message id %d, got %v (err %v)", want, msg, err)
		}
	}
}

func send_request(t *testing.T, conn net.Conn, index, begin, length int) {
	t.Helper()
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], uint32(index))
	binary.BigEndian.PutUint32(payload[4:8], uint32(begin))
	binary.BigEndian.PutUint32(payload[8:12], uint32(length))
	if _, err := conn.Write((&message{id: msg_request, payload: payload}).serialize()); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func TestSeederRefusesUnverifiedPiece(t *testing.T) {
	tf, store, state := seed_fixture(t, false)
	s := &seeder{tf: tf, store: store, state: state, st: &stats{}, quit: make(chan struct{})}

	server, client := net.Pipe()
	defer client.Close()
	go func() { s.handle_peer(server); server.Close() }()

	seed_handshake(t, client, tf.info_hash)
	send_request(t, client, 0, 0, block_size)

	// we never verified piece 0, so its bytes must not leave the machine
	client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	msg, err := read_message(client)
	if err == nil && msg != nil && msg.id == msg_piece {
		t.Fatal("seeder served a piece it has not verified")
	}
}

func TestSeederDropsOversizedRequest(t *testing.T) {
	tf, store, state := seed_fixture(t, true)
	s := &seeder{tf: tf, store: store, state: state, st: &stats{}, quit: make(chan struct{})}

	server, client := net.Pipe()
	defer client.Close()
	go func() { s.handle_peer(server); server.Close() }()

	seed_handshake(t, client, tf.info_hash)
	send_request(t, client, 0, 0, 200*1024)

	client.SetReadDeadline(time.Now().Add(time.Second))
	msg, err := read_message(client)
	if err == nil && msg != nil && msg.id == msg_piece {
		t.Fatal("seeder answered a 200 KB request")
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatal("seeder kept the connection open after an oversized request")
	}
}

func TestSeederRejectsWrongProtocol(t *testing.T) {
	tf, store, state := seed_fixture(t, true)
	s := &seeder{tf: tf, store: store, state: state, st: &stats{}, quit: make(chan struct{})}

	server, client := net.Pipe()
	defer client.Close()
	go func() { s.handle_peer(server); server.Close() }()

	h := new_handshake(tf.info_hash, [20]byte{})
	h.pstr = "NotTorrent protocol"
	go client.Write(h.serialize())

	client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := read_handshake(client); err == nil {
		t.Fatal("seeder answered a handshake for another protocol")
	}
}

func TestSeederStopClosesIdlePeers(t *testing.T) {
	tf, store, state := seed_fixture(t, true)
	s, err := start_seeder(0, tf, store, state, &stats{})
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("tcp", s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	seed_handshake(t, conn, tf.info_hash)

	// the peer is now connected and silent; stop must not wait on it forever
	stopped := make(chan struct{})
	go func() {
		s.stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("stop() hung waiting for an idle peer connection")
	}
}
