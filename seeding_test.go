package main

import (
	"bytes"
	"encoding/binary"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestSeederHandshakeAndUpload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seed_test.iso")

	totalLen := 256 * 1024
	pieceLen := 256 * 1024

	st, err := open_storage(path, totalLen, pieceLen)
	if err != nil {
		t.Fatalf("open_storage failed: %v", err)
	}
	defer st.close()

	// Write dummy piece data
	dummyData := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, pieceLen/4)
	if err := st.write_piece(0, dummyData); err != nil {
		t.Fatalf("write_piece failed: %v", err)
	}

	pState := new_piece_state(1)
	pState.mark_done(0)
	statsCounter := &stats{started: time.Now()}

	var dummyHash [20]byte
	copy(dummyHash[:], []byte("01234567890123456789"))
	var dummyPeerID [20]byte
	copy(dummyPeerID[:], []byte("-GO0001-123456789012"))

	tf := &torrent_file{
		info_hash:    dummyHash,
		length:       totalLen,
		piece_length: pieceLen,
	}

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	s := &seeder{
		tf:      tf,
		store:   st,
		state:   pState,
		st:      statsCounter,
		peer_id: dummyPeerID,
		quit:    make(chan struct{}),
	}

	go s.handle_peer(serverConn)

	// Send handshake from client
	hs := &handshake{
		pstr:      "BitTorrent protocol",
		info_hash: dummyHash,
		peer_id:   dummyPeerID,
	}
	if _, err := clientConn.Write(hs.serialize()); err != nil {
		t.Fatalf("failed to write client handshake: %v", err)
	}

	// Read reply handshake
	replyHs, err := read_handshake(clientConn)
	if err != nil || !bytes.Equal(replyHs.info_hash[:], dummyHash[:]) {
		t.Fatalf("handshake exchange failed: %v", err)
	}

	// Read Bitfield message
	bfMsg, err := read_message(clientConn)
	if err != nil || bfMsg.id != msg_bitfield {
		t.Fatalf("expected bitfield message, got: %v", err)
	}

	// Read Unchoke message
	unchokeMsg, err := read_message(clientConn)
	if err != nil || unchokeMsg.id != msg_unchoke {
		t.Fatalf("expected unchoke message, got: %v", err)
	}

	// Request 16 KB block from piece 0
	reqPayload := make([]byte, 12)
	binary.BigEndian.PutUint32(reqPayload[0:4], 0)
	binary.BigEndian.PutUint32(reqPayload[4:8], 0)
	binary.BigEndian.PutUint32(reqPayload[8:12], 16384)

	reqMsg := message{
		id:      msg_request,
		payload: reqPayload,
	}
	if _, err := clientConn.Write(reqMsg.serialize()); err != nil {
		t.Fatalf("failed to send request message: %v", err)
	}

	// Read Piece response message
	pieceMsg, err := read_message(clientConn)
	if err != nil || pieceMsg.id != msg_piece {
		t.Fatalf("expected piece message response, got: %v", err)
	}

	// Verify payload matches
	blockData := pieceMsg.payload[8:]
	if !bytes.Equal(blockData, dummyData[:16384]) {
		t.Fatalf("piece data mismatch")
	}

	if statsCounter.uploaded.Load() != 16384 {
		t.Fatalf("expected uploaded stat 16384, got %d", statsCounter.uploaded.Load())
	}
}