package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/jackpal/bencode-go"
)

type bencode_info struct {
	Pieces      string `bencode:"pieces"`
	PieceLength int    `bencode:"piece length"`
	Length      int    `bencode:"length"`
	Name        string `bencode:"name"`
}

type bencode_torrent struct {
	Announce string       `bencode:"announce"`
	Info     bencode_info `bencode:"info"`
}

type torrent_file struct {
	announce     string
	info_hash    [20]byte
	piece_hashes [][20]byte
	piece_length int
	length       int
	name         string
}

func open(path string) (torrent_file, error) {
	metainfo, err := os.ReadFile(path)
	if err != nil {
		return torrent_file{}, err
	}

	raw := &bencode_torrent{}

	if err := bencode.Unmarshal(bytes.NewReader(metainfo), raw); err != nil {
		return torrent_file{}, err
	}

	info_hash, err := info_hash_of(metainfo)
	if err != nil {
		return torrent_file{}, err
	}

	return raw.to_torrent_file(info_hash)
}

func (b *bencode_torrent) to_torrent_file(info_hash [20]byte) (torrent_file, error) {
	piece_hashes, err := b.Info.split_piece_hashes()
	if err != nil {
		return torrent_file{}, err
	}

	return torrent_file{
		announce:     b.Announce,
		info_hash:    info_hash,
		piece_hashes: piece_hashes,
		piece_length: b.Info.PieceLength,
		length:       b.Info.Length,
		name:         b.Info.Name,
	}, nil
}

// info_hash_of computes the SHA-1 of the torrent's info dictionary.
//
// The hash has to cover the info dictionary exactly as trackers and peers see
// it, including keys this client does not model (private, files, source, ...).
// Re-encoding bencode_info would drop those keys and yield an info hash nobody
// else agrees with, so decode the metainfo generically and re-encode just the
// info dictionary. bencode-go emits dictionary keys sorted, which is the
// canonical ordering the spec requires.
func info_hash_of(metainfo []byte) ([20]byte, error) {
	decoded, err := bencode.Decode(bytes.NewReader(metainfo))
	if err != nil {
		return [20]byte{}, err
	}

	top, ok := decoded.(map[string]interface{})
	if !ok {
		return [20]byte{}, fmt.Errorf("malformed torrent: top level is not a dictionary")
	}

	info, ok := top["info"]
	if !ok {
		return [20]byte{}, fmt.Errorf("malformed torrent: missing info dictionary")
	}

	var buf bytes.Buffer
	if err := bencode.Marshal(&buf, info); err != nil {
		return [20]byte{}, err
	}

	return sha1.Sum(buf.Bytes()), nil
}

func (i *bencode_info) split_piece_hashes() ([][20]byte, error) {
	buf := []byte(i.Pieces)

	if len(buf)%20 != 0 {
		return nil, fmt.Errorf("malformed pieces: length %d not a multiple of 20", len(buf))
	}

	n := len(buf) / 20
	hashes := make([][20]byte, n)

	for j := 0; j < n; j++ {
		copy(hashes[j][:], buf[j*20:(j+1)*20])
	}
	return hashes, nil
}

type peer struct {
	ip   [4]byte
	port uint16
}

func unmarshal_peers(data []byte) ([]peer, error) {
	if len(data)%6 != 0 {
		return nil, fmt.Errorf("malformed peers: length %d not a multiple of 6", len(data))
	}

	n := len(data) / 6
	peers := make([]peer, n)

	for i := 0; i < n; i++ {
		copy(peers[i].ip[:], data[i*6:i*6+4])
		peers[i].port = binary.BigEndian.Uint16(data[i*6+4 : i*6+6])
	}
	return peers, nil
}

func (p peer) String() string {
	return fmt.Sprintf("%d.%d.%d.%d:%d", p.ip[0], p.ip[1], p.ip[2], p.ip[3], p.port)
}

func (t *torrent_file) request_peers(peer_id [20]byte, port uint16, left int) ([]peer, error) {
	base, err := url.Parse(t.announce)
	if err != nil {
		return nil, err
	}

	params := url.Values{
		"info_hash":  []string{string(t.info_hash[:])},
		"peer_id":    []string{string(peer_id[:])},
		"port":       []string{strconv.Itoa(int(port))},
		"uploaded":   []string{"0"},
		"downloaded": []string{"0"},
		"compact":    []string{"1"},
		"left":       []string{strconv.Itoa(left)},
		"numwant":    []string{"100"},
	}

	base.RawQuery = params.Encode()
	http_client := &http.Client{Timeout: 15 * time.Second}

	response, err := http_client.Get(base.String())
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tracker returned HTTP %d", response.StatusCode)
	}

	var tracker_resp struct {
		Failure string `bencode:"failure reason"`
		Peers   string `bencode:"peers"`
	}

	if err := bencode.Unmarshal(bytes.NewReader(body), &tracker_resp); err != nil {
		return nil, err
	}
	// a tracker reports errors (unregistered torrent, rate limit, ...) as a
	// normal response with this key; without the check it looks like 0 peers
	if tracker_resp.Failure != "" {
		return nil, fmt.Errorf("tracker error: %s", tracker_resp.Failure)
	}
	return unmarshal_peers([]byte(tracker_resp.Peers))
}

func new_peer_id() ([20]byte, error) {
	var id [20]byte
	_, err := rand.Read(id[:])
	return id, err
}
