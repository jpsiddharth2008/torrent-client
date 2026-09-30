package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

const (
	// a peer must finish its handshake within this long
	seed_handshake_timeout = 10 * time.Second
	// peers send a keep-alive at least every 2 minutes; silence beyond that
	// means the connection is dead
	seed_idle_timeout = 3 * time.Minute
	// 16 KB is the standard block size; a few clients ask for up to 128 KB,
	// anything larger is abuse
	max_request_length = 128 * 1024
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

	mu    sync.Mutex
	conns map[net.Conn]struct{} // open peer connections, closed by stop
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
		conns:    make(map[net.Conn]struct{}),
	}

	s.wg.Add(1)
	go s.accept_loop()

	return s, nil
}

// stop closes the listener and every open peer connection, then waits for
// all handlers to return. Handlers block in read_message, so without closing
// their connections a single idle peer would make stop hang forever.
func (s *seeder) stop() {
	close(s.quit)
	s.listener.Close()

	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()

	s.wg.Wait()
}

// track registers a connection so stop can close it. It reports false if the
// seeder is already stopping, in which case the caller must drop the conn.
func (s *seeder) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.quit:
		return false
	default:
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *seeder) untrack(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
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
			}
			// e.g. out of file descriptors: back off instead of spinning
			log.Printf("seeder: accept failed: %v\n", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if !s.track(conn) {
			conn.Close()
			return
		}

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer s.untrack(c)
			defer c.Close()
			s.handle_peer(c)
		}(conn)
	}
}

func (s *seeder) handle_peer(conn net.Conn) {
	// 1. Handshake exchange, bounded so a silent peer can't hold us forever
	conn.SetDeadline(time.Now().Add(seed_handshake_timeout))

	hs, err := read_handshake(conn)
	if err != nil {
		return
	}

	if hs.pstr != "BitTorrent protocol" {
		return // not a BitTorrent peer
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
		conn.SetDeadline(time.Now().Add(seed_idle_timeout))

		msg, err := read_message(conn)
		if err != nil {
			return
		}
		if msg == nil {
			continue // Keep-alive message
		}

		switch msg.id {
		case msg_request:
			if len(msg.payload) != 12 {
				return
			}
			index := int(binary.BigEndian.Uint32(msg.payload[0:4]))
			begin := int(binary.BigEndian.Uint32(msg.payload[4:8]))
			length := int(binary.BigEndian.Uint32(msg.payload[8:12]))

			if length <= 0 || length > max_request_length {
				return // misbehaving peer
			}
			// never upload a piece we haven't verified: while downloading, the
			// file holds zeros and half-written pieces we must not spread.
			// Peers learn what we have from our bitfield, so a request for
			// anything else is ignored rather than answered with bad data.
			if !s.state.has(index) {
				continue
			}

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
