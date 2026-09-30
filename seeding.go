package main

import (
	"bytes"
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
	// a single write to a peer that takes longer than this means it has
	// stopped reading; drop it rather than stall everyone else
	seed_write_timeout = 30 * time.Second
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
	peers map[*seed_peer]struct{} // connected peers, for have broadcasts and stop
}

// seed_peer is one incoming connection. Both its handler (sending piece
// messages) and broadcast_have (sending have messages) write to the conn, so
// every write goes through write_mu; otherwise the two could interleave bytes
// and corrupt the stream.
type seed_peer struct {
	conn     net.Conn
	write_mu sync.Mutex
	ready    bool // bitfield sent; have messages may follow
}

func (p *seed_peer) send(m *message) error {
	p.write_mu.Lock()
	defer p.write_mu.Unlock()
	return p.send_locked(m)
}

func (p *seed_peer) send_locked(m *message) error {
	p.conn.SetWriteDeadline(time.Now().Add(seed_write_timeout))
	_, err := p.conn.Write(m.serialize())
	return err
}

func start_seeder(port int, tf *torrent_file, store *storage, state *piece_state, st *stats, peer_id [20]byte) (*seeder, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("failed to listen on port %d: %w", port, err)
	}

	s := &seeder{
		listener: listener,
		tf:       tf,
		store:    store,
		state:    state,
		st:       st,
		peer_id:  peer_id,
		quit:     make(chan struct{}),
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
	for p := range s.peers {
		p.conn.Close()
	}
	s.mu.Unlock()

	s.wg.Wait()
}

// register adds a peer so stop can close it and broadcast_have can reach it.
// It reports false if the seeder is already stopping.
func (s *seeder) register(p *seed_peer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.quit:
		return false
	default:
	}
	if s.peers == nil {
		s.peers = make(map[*seed_peer]struct{})
	}
	s.peers[p] = struct{}{}
	return true
}

func (s *seeder) unregister(p *seed_peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.peers, p)
}

// peer_count reports how many peers are connected to us.
func (s *seeder) peer_count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.peers)
}

// broadcast_have tells every connected peer we now have a piece, so peers
// that connected mid-download can request pieces we finished after their
// bitfield was sent. Writes happen off the caller's goroutine so a slow peer
// never stalls the download loop.
func (s *seeder) broadcast_have(index int) {
	s.mu.Lock()
	peers := make([]*seed_peer, 0, len(s.peers))
	for p := range s.peers {
		peers = append(peers, p)
	}
	s.mu.Unlock()

	for _, p := range peers {
		go func(p *seed_peer) {
			p.write_mu.Lock()
			defer p.write_mu.Unlock()
			// not ready means its bitfield hasn't gone out yet; that bitfield
			// is snapshotted after this piece was marked done, so it already
			// includes it (a have before the bitfield would break the protocol)
			if !p.ready {
				return
			}
			if err := p.send_locked(format_have(index)); err != nil {
				p.conn.Close() // the handler notices and cleans up
			}
		}(p)
	}
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

		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			defer c.Close()
			s.handle_peer(c)
		}(conn)
	}
}

func (s *seeder) handle_peer(conn net.Conn) {
	p := &seed_peer{conn: conn}
	if !s.register(p) {
		return
	}
	defer s.unregister(p)

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
	log.Printf("seeder: peer %s connected\n", conn.RemoteAddr())
	if s.st != nil {
		s.st.upload_peers.Add(1)
		defer s.st.upload_peers.Add(-1)
	}

	replyHs := &handshake{
		pstr:      "BitTorrent protocol",
		info_hash: s.tf.info_hash,
		peer_id:   s.peer_id,
	}
	if err := p.send_raw(replyHs.serialize()); err != nil {
		return
	}

	// 2. Send our bitfield, then unchoke. The snapshot is taken under the
	// write lock and ready is set before releasing it, so every piece is
	// covered by either this bitfield or a later have message.
	p.write_mu.Lock()
	bfMsg := &message{id: msg_bitfield, payload: s.state.snapshot()}
	err = p.send_locked(bfMsg)
	if err == nil {
		p.ready = true
		// 3. Unchoke so the peer can request blocks right away
		err = p.send_locked(&message{id: msg_unchoke})
	}
	p.write_mu.Unlock()
	if err != nil {
		return
	}

	// 4. Message loop handling incoming request messages
	for {
		conn.SetReadDeadline(time.Now().Add(seed_idle_timeout))

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
			// Peers learn what we have from our bitfield and have messages,
			// so a request for anything else is ignored.
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

			// counted before the write: a synchronous conn only returns once
			// the peer has read the block
			if s.st != nil {
				s.st.uploaded.Add(int64(len(block)))
			}
			if err := p.send(&message{id: msg_piece, payload: payload}); err != nil {
				return
			}
		}
	}
}

// send_raw writes bytes that are not a length-prefixed message (the handshake).
func (p *seed_peer) send_raw(b []byte) error {
	p.write_mu.Lock()
	defer p.write_mu.Unlock()
	p.conn.SetWriteDeadline(time.Now().Add(seed_write_timeout))
	_, err := p.conn.Write(b)
	return err
}
