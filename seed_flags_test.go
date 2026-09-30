package main

import (
	"bytes"
	"crypto/sha1"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestParsePeers(t *testing.T) {
	peers, err := parse_peers("127.0.0.1:6881, 10.0.0.2:51413")
	if err != nil {
		t.Fatalf("parse_peers: %v", err)
	}
	if len(peers) != 2 || peers[0].String() != "127.0.0.1:6881" || peers[1].String() != "10.0.0.2:51413" {
		t.Errorf("parse_peers = %v", peers)
	}

	if peers, err := parse_peers("localhost:6881"); err != nil || peers[0].String() != "127.0.0.1:6881" {
		t.Errorf("parse_peers(localhost) = %v, %v", peers, err)
	}

	for _, bad := range []string{"", "127.0.0.1", "127.0.0.1:0", "not a host:x"} {
		if _, err := parse_peers(bad); err == nil {
			t.Errorf("parse_peers(%q) should fail", bad)
		}
	}
}

func TestFindPeersUsesFixedList(t *testing.T) {
	// announce points nowhere: with fixed peers the tracker must not be used
	tf := &torrent_file{announce: "http://127.0.0.1:1/announce"}
	fixed, _ := parse_peers("127.0.0.1:6881")

	got, err := tf.find_peers(download_options{peers: fixed}, new_piece_state(1))
	if err != nil || len(got) != 1 || got[0] != fixed[0] {
		t.Errorf("find_peers = %v, %v; want the fixed peer", got, err)
	}
}

func TestSeederBroadcastsHave(t *testing.T) {
	// two pieces on disk; only piece 0 is verified when the peer connects
	const piece_length = 32 * 1024
	store, err := open_storage(filepath.Join(t.TempDir(), "seed.bin"), 2*piece_length, piece_length)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()

	pieces := [][]byte{bytes.Repeat([]byte{1}, piece_length), bytes.Repeat([]byte{2}, piece_length)}
	tf := &torrent_file{length: 2 * piece_length, piece_length: piece_length}
	copy(tf.info_hash[:], "broadcast-have-hash!")
	for i, data := range pieces {
		store.write_piece(i, data)
		tf.piece_hashes = append(tf.piece_hashes, sha1.Sum(data))
	}

	state := new_piece_state(2)
	state.mark_done(0)
	st := &stats{}

	s, err := start_seeder(0, tf, store, state, st, [20]byte{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.stop()

	conn, err := net.Dial("tcp", s.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	var id [20]byte
	conn.Write(new_handshake(tf.info_hash, id).serialize())
	if _, err := read_handshake(conn); err != nil {
		t.Fatal(err)
	}
	bf, err := read_message(conn)
	if err != nil || bf.id != msg_bitfield {
		t.Fatalf("expected bitfield, got %v %v", bf, err)
	}
	if !bitfield(bf.payload).has_piece(0) || bitfield(bf.payload).has_piece(1) {
		t.Fatalf("bitfield = %08b, want only piece 0", bf.payload)
	}
	if m, err := read_message(conn); err != nil || m.id != msg_unchoke {
		t.Fatalf("expected unchoke, got %v %v", m, err)
	}

	// wait until the handler has registered the peer as ready
	for i := 0; st.upload_peers.Load() == 0 && i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if got := s.peer_count(); got != 1 {
		t.Fatalf("peer_count = %d, want 1", got)
	}

	// we finish piece 1 while the peer is connected
	state.mark_done(1)
	s.broadcast_have(1)

	m, err := read_message(conn)
	if err != nil || m.id != msg_have {
		t.Fatalf("expected have, got %v %v", m, err)
	}
	if index, _ := parse_have(m); index != 1 {
		t.Fatalf("have index = %d, want 1", index)
	}

	// and the peer can now actually download it
	conn.Write(format_request(1, 0, block_size).serialize())
	m, err = read_message(conn)
	if err != nil || m.id != msg_piece {
		t.Fatalf("expected piece, got %v %v", m, err)
	}
	if !bytes.Equal(m.payload[8:], pieces[1][:block_size]) {
		t.Error("served block does not match piece 1")
	}
}
