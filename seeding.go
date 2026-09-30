package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
)

type seeder struct {
	listener net.Listener
	tf       *torrent_file
	store    *storage
	state    *piece_state
	st       *stats
	peer_id  [20]byte
	quit     chan struct{}
	wg       sync.WaitGroup
}

func start_seeder(port int, tf *torrent_file, store *storage, state *piece_state, st *stats) (*seeder, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("failed to listen on port %d: %w", port, err)
	}

	var pid [20]byte
	copy(pid[:], []byte("-TC0001-"))
	rand.Read(pid[8:])

	s := &seeder{
		listener: listener,
		tf:       tf,
		store:    store,
		state:    state,
		st:       st,
		peer_id:  pid,
		quit:     make(chan struct{}),
	}

	s.wg.Add(1)
	go s.accept_loop()

	return s, nil
}

func (s *seeder) stop() {
	close(s.quit)
	s.listener.Close()
	s.wg.Wait()
}

func (s *seeder) accept_loop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return
			default:
				continue
			}
		}

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer c.Close()
			s.handle_peer(c)
		}(conn)
	}
}

func (s *seeder) handle_peer(conn net.Conn) {
	// 1. Handshake exchange
	hs, err := read_handshake(conn)
	if err != nil {
		return
	}

	if !bytes.Equal(hs.info_hash[:], s.tf.info_hash[:]) {
		return // Info-hash mismatch
	}

	replyHs := &handshake{
		pstr:      "BitTorrent protocol",
		info_hash: s.tf.info_hash,
		peer_id:   s.peer_id,
	}
	if _, err := conn.Write(replyHs.serialize()); err != nil {
		return
	}

	// 2. Send Bitfield message if we have verified pieces
	snap := s.state.snapshot()
	if len(snap) > 0 {
		bfMsg := message{
			id:      msg_bitfield,
			payload: snap,
		}
		if _, err := conn.Write(bfMsg.serialize()); err != nil {
			return
		}
	}

	// 3. Send Unchoke message to peer so they can request blocks
	unchokeMsg := message{id: msg_unchoke}
	if _, err := conn.Write(unchokeMsg.serialize()); err != nil {
		return
	}

	// 4. Message loop handling incoming request messages
	for {
		msg, err := read_message(conn)
		if err != nil {
			return
		}
		if msg == nil {
			continue // Keep-alive message
		}

		switch msg.id {
		case msg_request:
			if len(msg.payload) < 12 {
				return
			}
			index := int(binary.BigEndian.Uint32(msg.payload[0:4]))
			begin := int(binary.BigEndian.Uint32(msg.payload[4:8]))
			length := int(binary.BigEndian.Uint32(msg.payload[8:12]))

			block, err := s.store.read_block(index, begin, length)
			if err != nil {
				continue
			}

			payload := make([]byte, 8+len(block))
			binary.BigEndian.PutUint32(payload[0:4], uint32(index))
			binary.BigEndian.PutUint32(payload[4:8], uint32(begin))
			copy(payload[8:], block)

			pMsg := message{
				id:      msg_piece,
				payload: payload,
			}

			// Increment upload counter before pipe flush
			if s.st != nil {
				s.st.uploaded.Add(int64(len(block)))
			}

			if _, err := conn.Write(pMsg.serialize()); err != nil {
				return
			}
		}
	}
}