package main

import (
	"log"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		log.Fatalf("usage: torrent-client <torrent-file> <out-path>")
	}

	torrent_path := os.Args[1]
	out_path := os.Args[2]

	// 1. Open and parse torrent file
	tf, err := open(torrent_path)
	if err != nil {
		log.Fatalf("could not open torrent file: %v\n", err)
	}

	// 2. Initialize disk-backed storage, piece state tracker, and atomic stats
	store, err := open_storage(out_path, tf.length, tf.piece_length)
	if err != nil {
		log.Fatalf("could not open storage: %v\n", err)
	}
	defer store.close()

	pState := new_piece_state(len(tf.piece_hashes))
	st := &stats{started: time.Now()}

	// 3. Start live terminal progress dashboard in background (Issue #8)
	stopDash := make(chan struct{})
	go run_dashboard(pState, st, int64(tf.length), stopDash)

	// 4. Start TCP seeding upload listener on port 6881 (Issue #9)
	seedingServer, err := start_seeder(6881, &tf, store, pState, st)
	if err != nil {
		log.Printf("Warning: Could not start seeding listener: %v\n", err)
	} else {
		defer seedingServer.stop()
		log.Println("Seeding TCP listener active on port 6881")
	}

	// 5. Run main download pipeline
	_, err = tf.download()
	close(stopDash) // Stop dashboard renderer when download completes or exits

	if err != nil {
		log.Fatalf("download failed: %v\n", err)
	}

	log.Println("Download completed successfully!")
}