package main

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"log"
	"sync/atomic"
	"time"
)

const block_size = 16384
const max_backlog = 5

// how often the download loop checks whether any workers are still alive,
// and the minimum gap between tracker announces when they are all gone
const peer_check_interval = 5 * time.Second
const reannounce_delay = 30 * time.Second

type piece_work struct {
	index  int
	hash   [20]byte
	length int
}

type piece_result struct {
	index int
	data  []byte
}

type piece_progress struct {
	index      int
	buf        []byte
	downloaded int
	requested  int
	backlog    int
}

func (pp *piece_progress) fill_requests(c *client, piece_length int) error {
	for pp.backlog < max_backlog && pp.requested < piece_length {
		block_len := block_size
		if piece_length-pp.requested < block_size {
			block_len = piece_length - pp.requested
		}
		if err := c.send_request(pp.index, pp.requested, block_len); err != nil {
			return err
		}
		pp.backlog++
		pp.requested += block_len
	}
	return nil
}

func (pp *piece_progress) handle_message(c *client) error {
	msg, err := c.read()
	if err != nil {
		return err
	}
	if msg == nil {
		return nil
	}

	switch msg.id {

	case msg_unchoke:
		c.choked = false
		log.Printf("unchoked by peer\n")

	case msg_choke:
		c.choked = true

	case msg_have:
		index, err := parse_have(msg)
		if err != nil {
			return err
		}
		c.bitfield.set_piece(index)

	case msg_piece:
		n, err := parse_piece(pp.index, pp.buf, msg)
		if err != nil {
			return err
		}
		pp.downloaded += n
		// the block landed, so free its slot for the next request
		pp.backlog--
		// counted per 16 KB block, not per piece, so the speed readout is smooth
		if c.stats != nil {
			c.stats.downloaded.Add(int64(n))
		}
		// throttle after the block is banked, not before: the bytes are
		// already on the wire, and sleeping here delays the next request,
		// which is what actually paces the peer
		c.down_lim.wait(n)
	}

	return nil
}

func download_piece(c *client, pw *piece_work) ([]byte, error) {
	pp := &piece_progress{
		index: pw.index,
		buf:   make([]byte, pw.length),
	}

	c.conn.SetDeadline(time.Now().Add(30 * time.Second))
	defer c.conn.SetDeadline(time.Time{})

	for pp.downloaded < pw.length {
		if !c.choked {
			if err := pp.fill_requests(c, pw.length); err != nil {
				return nil, err
			}
		}
		if err := pp.handle_message(c); err != nil {
			return nil, err
		}
	}

	return pp.buf, nil
}

func check_integrity(pw *piece_work, data []byte) error {
	hash := sha1.Sum(data)
	if !bytes.Equal(hash[:], pw.hash[:]) {
		return fmt.Errorf("piece %d failed integrity check", pw.index)
	}
	return nil
}

func start_download_worker(p peer, info_hash [20]byte, peer_id [20]byte, num_pieces int, st *stats, down_lim *rate_limiter, work_ch chan *piece_work, results_ch chan *piece_result) {
	// num_pieces is passed explicitly: len(work_ch) shrinks as work is claimed,
	// which would undersize the bitfield of any late-connecting peer
	c, err := new_client(p, info_hash, peer_id, num_pieces)
	if err != nil {
		log.Printf("could not connect to peer %s: %v\n", p, err)
		return
	}
	defer c.conn.Close()

	c.stats = st
	// one limiter shared by every worker, so the cap is a total across peers
	// rather than a per-peer allowance
	c.down_lim = down_lim
	st.active_peers.Add(1)
	defer st.active_peers.Add(-1)

	log.Printf("connected to peer %s\n", p)

	if err := c.send_unchoke(); err != nil {
		return
	}
	if err := c.send_interested(); err != nil {
		return
	}
	log.Printf("sent interested to %s, choked=%v\n", p, c.choked)

	misses := 0
	for pw := range work_ch {
		if !c.bitfield.has_piece(pw.index) {
			work_ch <- pw
			// once we have cycled through the whole queue without finding a
			// piece this peer has, it has nothing we need: give up rather
			// than spin on the channel at full CPU
			misses++
			if misses > len(work_ch) {
				log.Printf("peer %s has none of the pieces we still need\n", p)
				return
			}
			continue
		}
		misses = 0

		data, err := download_piece(c, pw)
		if err != nil {
			work_ch <- pw
			log.Printf("failed to download piece %d from %s: %v\n", pw.index, p, err)
			return
		}

		if err := check_integrity(pw, data); err != nil {
			work_ch <- pw
			log.Printf("piece %d from %s failed integrity check\n", pw.index, p)
			continue
		}

		results_ch <- &piece_result{pw.index, data}
	}
}

// download_options configures where download finds peers and how it
// identifies itself to them.
type download_options struct {
	peer_id  [20]byte
	port     uint16        // our listening port, announced to the tracker
	peers    []peer        // if set, connect only to these and never ask the tracker
	down_lim *rate_limiter // shared across workers; nil means unlimited
}

// find_peers returns the fixed peer list if one was given, otherwise asks the
// tracker.
func (t *torrent_file) find_peers(opts download_options, state *piece_state) ([]peer, error) {
	if len(opts.peers) > 0 {
		return opts.peers, nil
	}
	return t.request_peers(opts.peer_id, opts.port, t.bytes_left(state))
}

// download fetches every piece not already marked done in state, writing each
// verified piece to store. on_piece, if set, runs after a piece is on disk
// and marked done.
func (t *torrent_file) download(store *storage, state *piece_state, st *stats, opts download_options, on_piece func(index int)) error {
	num_pieces := len(t.piece_hashes)
	done_pieces := state.count()
	if done_pieces == num_pieces {
		log.Printf("all %d pieces already verified on disk, nothing to download\n", num_pieces)
		return nil
	}

	log.Println("starting download for", t.name)
	peer_id := opts.peer_id

	peers, err := t.find_peers(opts, state)
	if err != nil {
		return err
	}
	log.Printf("got %d peers\n", len(peers))
	st.known_peers.Store(int32(len(peers)))

	work_ch := make(chan *piece_work, num_pieces)
	results_ch := make(chan *piece_result)

	for index, hash := range t.piece_hashes {
		// already verified on disk by an earlier run
		if state.has(index) {
			continue
		}
		length := t.piece_length_at(index)
		work_ch <- &piece_work{index, hash, length}
	}

	// running counts worker goroutines still alive, including ones still
	// connecting; st.active_peers only counts those past the handshake
	var running atomic.Int32
	spawn := func(peers []peer) {
		for _, p := range peers {
			running.Add(1)
			go func(p peer) {
				defer running.Add(-1)
				start_download_worker(p, t.info_hash, peer_id, num_pieces, st, opts.down_lim, work_ch, results_ch)
			}(p)
		}
	}
	spawn(peers)

	check := time.NewTicker(peer_check_interval)
	defer check.Stop()
	last_announce := time.Now()

	for done_pieces < num_pieces {
		select {
		case result := <-results_ch:
			if err := store.write_piece(result.index, result.data); err != nil {
				return err
			}
			// mark done only once the data is written, so the saved state
			// never claims a piece that is not on disk
			state.mark_done(result.index)
			if on_piece != nil {
				on_piece(result.index)
			}
			done_pieces++

			percent := float64(done_pieces) / float64(num_pieces) * 100
			log.Printf("%.2f%% done — piece %d downloaded, %d peers active\n", percent, result.index, st.active_peers.Load())

		case <-check.C:
			// every worker has exited (peers disconnected, timed out or had
			// nothing we need), so nothing will ever arrive on results_ch:
			// get a fresh peer list (or redial --peer) instead of hanging
			if running.Load() > 0 || time.Since(last_announce) < reannounce_delay {
				continue
			}
			last_announce = time.Now()

			peers, err := t.find_peers(opts, state)
			if err != nil {
				log.Printf("re-announce failed: %v\n", err)
				continue
			}
			log.Printf("all peers gone, reconnecting to %d peers\n", len(peers))
			st.known_peers.Store(int32(len(peers)))
			spawn(peers)
		}
	}
	// work_ch is deliberately left open: a worker may still hand back a piece
	// it was holding, and a send on a closed channel would panic

	return nil
}

func (t *torrent_file) piece_length_at(index int) int {
	begin := index * t.piece_length
	end := begin + t.piece_length
	if end > t.length {
		end = t.length
	}
	return end - begin
}
