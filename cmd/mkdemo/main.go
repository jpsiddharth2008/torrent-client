// Command mkdemo builds a random data file and a matching .torrent for it, so
// the client can be demonstrated locally without a tracker or a real swarm.
//
//	go run ./cmd/mkdemo -size 32 -out demo
//
// writes demo/payload.bin and demo/demo.torrent. Seed the first with --seed
// and download it back with --peer to exercise the whole protocol.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackpal/bencode-go"
)

func main() {
	size_mb := flag.Int("size", 32, "payload size in MB")
	piece_kb := flag.Int("piece", 256, "piece length in KB")
	out_dir := flag.String("out", "demo", "directory to write the payload and torrent into")
	name := flag.String("name", "payload.bin", "file name recorded inside the torrent")
	flag.Parse()

	if *size_mb <= 0 || *piece_kb <= 0 {
		fmt.Fprintln(os.Stderr, "-size and -piece must be positive")
		os.Exit(1)
	}

	if err := run(*size_mb, *piece_kb, *out_dir, *name); err != nil {
		fmt.Fprintf(os.Stderr, "mkdemo: %v\n", err)
		os.Exit(1)
	}
}

func run(size_mb, piece_kb int, out_dir, name string) error {
	if err := os.MkdirAll(out_dir, 0755); err != nil {
		return err
	}

	length := size_mb * 1024 * 1024
	piece_length := piece_kb * 1024

	data := make([]byte, length)
	if _, err := rand.Read(data); err != nil {
		return fmt.Errorf("generating payload: %w", err)
	}

	// concatenated SHA-1 of each piece, which is what the pieces key holds
	var pieces bytes.Buffer
	for off := 0; off < length; off += piece_length {
		end := off + piece_length
		if end > length {
			end = length
		}
		sum := sha1.Sum(data[off:end])
		pieces.Write(sum[:])
	}

	payload_path := filepath.Join(out_dir, name)
	if err := os.WriteFile(payload_path, data, 0644); err != nil {
		return err
	}

	// the announce URL is never contacted in the local demo, which uses
	// --peer, but a torrent without one is malformed
	meta := map[string]interface{}{
		"announce": "http://localhost:6969/announce",
		"info": map[string]interface{}{
			"name":         name,
			"length":       int64(length),
			"piece length": int64(piece_length),
			"pieces":       pieces.String(),
		},
	}

	var buf bytes.Buffer
	if err := bencode.Marshal(&buf, meta); err != nil {
		return fmt.Errorf("encoding torrent: %w", err)
	}

	torrent_path := filepath.Join(out_dir, "demo.torrent")
	if err := os.WriteFile(torrent_path, buf.Bytes(), 0644); err != nil {
		return err
	}

	fmt.Printf("payload  %s  (%d MB, %d pieces of %d KB)\n",
		payload_path, size_mb, pieces.Len()/20, piece_kb)
	fmt.Printf("torrent  %s\n\n", torrent_path)
	fmt.Printf("seed it:      torrent-client --seed --port 6881 %s %s\n", torrent_path, payload_path)
	fmt.Printf("download it:  torrent-client --port 6882 --peer 127.0.0.1:6881 --max-down 2048 %s %s\n",
		torrent_path, filepath.Join(out_dir, "copy.bin"))
	return nil
}
