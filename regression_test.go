package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// Regression tests for the four protocol defects in the original scaffold:
//
//  1. info hash was computed by re-encoding a typed struct, dropping any info
//     dictionary key the struct did not model
//  2. request messages were serialized little-endian
//  3. has_piece read bits least-significant-first while set_piece wrote them
//     most-significant-first
//  4. the request backlog was incremented on send but never decremented on
//     receipt, stalling the pipeline once it hit max_backlog

// --- bug 3: bitfield evaluation ---------------------------------------------

func TestHasPieceUsesMostSignificantBitFirst(t *testing.T) {
	// 0xAA is 1010 1010: with MSB-first ordering pieces 0, 2, 4, 6 are present.
	bf := bitfield{0xAA}

	for index := 0; index < 8; index++ {
		want := index%2 == 0
		if got := bf.has_piece(index); got != want {
			t.Errorf("has_piece(%d) = %v, want %v", index, got, want)
		}
	}
}

func TestSetPieceThenHasPieceAgree(t *testing.T) {
	const num_pieces = 21
	bf := make(bitfield, (num_pieces+7)/8)

	for index := 0; index < num_pieces; index += 3 {
		bf.set_piece(index)
	}

	for index := 0; index < num_pieces; index++ {
		want := index%3 == 0
		if got := bf.has_piece(index); got != want {
			t.Errorf("index %d: has_piece = %v, want %v", index, got, want)
		}
	}
}

func TestBitfieldRejectsOutOfRangeIndex(t *testing.T) {
	bf := make(bitfield, 1)

	// a peer can announce any index in a have message; neither call may panic
	bf.set_piece(64)
	if bf.has_piece(64) {
		t.Error("has_piece(64) = true for a 1 byte bitfield")
	}
	if bf[0] != 0 {
		t.Errorf("out of range set_piece corrupted the bitfield: %08b", bf[0])
	}
}

// --- bug 2: wire protocol serialization -------------------------------------

func TestFormatRequestIsBigEndian(t *testing.T) {
	msg := format_request(1, 2, 16384)

	if msg.id != msg_request {
		t.Fatalf("id = %d, want %d", msg.id, msg_request)
	}

	want := []byte{
		0x00, 0x00, 0x00, 0x01, // index  1
		0x00, 0x00, 0x00, 0x02, // begin  2
		0x00, 0x00, 0x40, 0x00, // length 16384
	}
	if !bytes.Equal(msg.payload, want) {
		t.Errorf("payload = % x, want % x", msg.payload, want)
	}
}

func TestMessageSerializeRoundTrip(t *testing.T) {
	original := format_request(7, 32768, 16384)

	conn_a, conn_b := net.Pipe()
	defer conn_a.Close()
	defer conn_b.Close()

	go func() { conn_a.Write(original.serialize()) }()

	got, err := read_message(conn_b)
	if err != nil {
		t.Fatalf("read_message: %v", err)
	}
	if got.id != original.id || !bytes.Equal(got.payload, original.payload) {
		t.Errorf("round trip = %d/% x, want %d/% x",
			got.id, got.payload, original.id, original.payload)
	}
}

// --- bug 1: info hash generation --------------------------------------------

func TestInfoHashCoversUnmodelledKeys(t *testing.T) {
	// testdata/sample.torrent has "private" and "source" in its info dict,
	// neither of which bencode_info models. The expected digest was produced
	// by an independent bencode implementation over the same dictionary.
	const want = "f6281081ba60c874f00d7634fd22c6941ecb7735"

	tf, err := open("testdata/sample.torrent")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if got := hex.EncodeToString(tf.info_hash[:]); got != want {
		t.Errorf("info_hash = %s, want %s", got, want)
	}
}

func TestOpenParsesMetainfoFields(t *testing.T) {
	tf, err := open("testdata/sample.torrent")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if tf.name != "test.bin" {
		t.Errorf("name = %q, want %q", tf.name, "test.bin")
	}
	if tf.length != 40 {
		t.Errorf("length = %d, want 40", tf.length)
	}
	if tf.piece_length != 20 {
		t.Errorf("piece_length = %d, want 20", tf.piece_length)
	}
	if len(tf.piece_hashes) != 2 {
		t.Fatalf("piece_hashes = %d, want 2", len(tf.piece_hashes))
	}
	if tf.announce != "http://tracker.example.com/announce" {
		t.Errorf("announce = %q", tf.announce)
	}
}

func TestOpenRejectsNonTorrent(t *testing.T) {
	if _, err := open("testdata"); err == nil {
		t.Error("expected an error opening a directory")
	}
}

// --- bug 4: request pipeline flow control -----------------------------------

// scripted_peer answers request messages with the blocks they ask for, so a
// full piece can be downloaded without a real swarm. It records how many
// requests it saw and rejects any that do not decode as big-endian.
//
// This runs over a loopback TCP socket rather than net.Pipe: net.Pipe is
// unbuffered, so the peer's reply would block while the client is still
// pipelining requests and deadlock the harness instead of exercising it.
type scripted_peer struct {
	piece []byte

	mu       sync.Mutex
	requests int
	bad      error
}

func (sp *scripted_peer) note_bad(err error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.bad == nil {
		sp.bad = err
	}
}

func (sp *scripted_peer) stats() (int, error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.requests, sp.bad
}

func (sp *scripted_peer) serve(conn net.Conn) {
	defer conn.Close()

	for {
		msg, err := read_message(conn)
		if err != nil {
			return // client hung up, download finished
		}
		if msg == nil || msg.id != msg_request {
			continue
		}
		if len(msg.payload) != 12 {
			sp.note_bad(fmt.Errorf("request payload was %d bytes, want 12", len(msg.payload)))
			return
		}

		index := int(binary.BigEndian.Uint32(msg.payload[0:4]))
		begin := int(binary.BigEndian.Uint32(msg.payload[4:8]))
		length := int(binary.BigEndian.Uint32(msg.payload[8:12]))

		// a little-endian encoder turns 16384 into 2^22, so an implausible
		// length here is exactly how a byte-order regression shows up
		if length <= 0 || length > block_size || begin < 0 || begin+length > len(sp.piece) {
			sp.note_bad(fmt.Errorf("implausible request: begin=%d length=%d", begin, length))
			return
		}

		sp.mu.Lock()
		sp.requests++
		sp.mu.Unlock()

		payload := make([]byte, 8+length)
		binary.BigEndian.PutUint32(payload[0:4], uint32(index))
		binary.BigEndian.PutUint32(payload[4:8], uint32(begin))
		copy(payload[8:], sp.piece[begin:begin+length])

		reply := &message{id: msg_piece, payload: payload}
		if _, err := conn.Write(reply.serialize()); err != nil {
			return
		}
	}
}

// dial_scripted_peer starts sp on a loopback listener and returns the client
// side of the connection.
func dial_scripted_peer(t *testing.T, sp *scripted_peer) net.Conn {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		sp.serve(conn)
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return conn
}

// TestDownloadPieceDrainsBacklog downloads a piece that needs far more blocks
// than max_backlog. With the backlog counter stuck at its ceiling the transfer
// stalls after max_backlog blocks and this test times out.
func TestDownloadPieceDrainsBacklog(t *testing.T) {
	const num_blocks = 8
	const piece_length = num_blocks * block_size

	if num_blocks <= max_backlog {
		t.Fatalf("piece must need more than max_backlog (%d) blocks", max_backlog)
	}

	piece := make([]byte, piece_length)
	for i := range piece {
		piece[i] = byte(i * 7)
	}

	sp := &scripted_peer{piece: piece}
	conn := dial_scripted_peer(t, sp)

	c := &client{conn: conn, choked: false}
	pw := &piece_work{index: 3, length: piece_length}

	type outcome struct {
		data []byte
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		data, err := download_piece(c, pw)
		done <- outcome{data, err}
	}()

	select {
	case got := <-done:
		requests, bad := sp.stats()
		if bad != nil {
			t.Fatalf("peer rejected a request: %v", bad)
		}
		if got.err != nil {
			t.Fatalf("download_piece: %v", got.err)
		}
		if !bytes.Equal(got.data, piece) {
			t.Error("downloaded piece does not match the source data")
		}
		if requests < num_blocks {
			t.Errorf("peer saw %d requests, want at least %d", requests, num_blocks)
		}
	case <-time.After(10 * time.Second):
		if _, bad := sp.stats(); bad != nil {
			t.Fatalf("peer rejected a request: %v", bad)
		}
		t.Fatal("download_piece stalled: backlog never drained")
	}
}

// TestFillRequestsRespectsCeiling pins the invariant directly.
func TestFillRequestsRespectsCeiling(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	go io.Copy(io.Discard, remote)

	c := &client{conn: local, choked: false}
	pp := &piece_progress{index: 0, buf: make([]byte, 100*block_size)}

	if err := pp.fill_requests(c, 100*block_size); err != nil {
		t.Fatalf("fill_requests: %v", err)
	}
	if pp.backlog != max_backlog {
		t.Errorf("backlog = %d, want %d", pp.backlog, max_backlog)
	}
	if pp.requested != max_backlog*block_size {
		t.Errorf("requested = %d, want %d", pp.requested, max_backlog*block_size)
	}
}

// --- supporting protocol invariants -----------------------------------------

func TestHandshakeRoundTrip(t *testing.T) {
	var info_hash, peer_id [20]byte
	copy(info_hash[:], "12345678901234567890")
	copy(peer_id[:], "ABCDEFGHIJKLMNOPQRST")

	h := new_handshake(info_hash, peer_id)
	serialized := h.serialize()

	if len(serialized) != len(h.pstr)+49 {
		t.Fatalf("serialized length = %d, want %d", len(serialized), len(h.pstr)+49)
	}

	conn_a, conn_b := net.Pipe()
	defer conn_a.Close()
	defer conn_b.Close()

	go func() { conn_a.Write(serialized) }()

	got, err := read_handshake(conn_b)
	if err != nil {
		t.Fatalf("read_handshake: %v", err)
	}
	if got.pstr != "BitTorrent protocol" {
		t.Errorf("pstr = %q", got.pstr)
	}
	if got.info_hash != info_hash {
		t.Errorf("info_hash = % x, want % x", got.info_hash, info_hash)
	}
	if got.peer_id != peer_id {
		t.Errorf("peer_id = % x, want % x", got.peer_id, peer_id)
	}
}

func TestPieceLengthAtHandlesFinalPiece(t *testing.T) {
	tf := &torrent_file{piece_length: 20, length: 50}

	for index, want := range []int{20, 20, 10} {
		if got := tf.piece_length_at(index); got != want {
			t.Errorf("piece_length_at(%d) = %d, want %d", index, got, want)
		}
	}
}

func TestUnmarshalPeers(t *testing.T) {
	// two peers: 127.0.0.1:6881 and 192.168.0.5:1
	data := []byte{127, 0, 0, 1, 0x1A, 0xE1, 192, 168, 0, 5, 0x00, 0x01}

	peers, err := unmarshal_peers(data)
	if err != nil {
		t.Fatalf("unmarshal_peers: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(peers))
	}
	if peers[0].String() != "127.0.0.1:6881" {
		t.Errorf("peers[0] = %s", peers[0])
	}
	if peers[1].String() != "192.168.0.5:1" {
		t.Errorf("peers[1] = %s", peers[1])
	}
}

func TestUnmarshalPeersRejectsRagged(t *testing.T) {
	if _, err := unmarshal_peers([]byte{1, 2, 3}); err == nil {
		t.Error("expected an error for a length that is not a multiple of 6")
	}
}

func TestParseHaveRejectsWrongLength(t *testing.T) {
	if _, err := parse_have(&message{id: msg_have, payload: []byte{1, 2}}); err == nil {
		t.Error("expected an error for a 2 byte have payload")
	}
	got, err := parse_have(&message{id: msg_have, payload: []byte{0, 0, 0, 9}})
	if err != nil {
		t.Fatalf("parse_have: %v", err)
	}
	if got != 9 {
		t.Errorf("parse_have = %d, want 9", got)
	}
}

func TestParsePieceRejectsMismatchedIndex(t *testing.T) {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[0:4], 5) // claims piece 5
	buf := make([]byte, 64)

	if _, err := parse_piece(4, buf, &message{id: msg_piece, payload: payload}); err == nil {
		t.Error("expected an error when the piece index does not match")
	}
}
