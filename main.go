package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
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

	// 2. Check existing file & restore state for resume support
	_, stat_err := os.Stat(out_path)
	existed := stat_err == nil

	store, err := open_storage(out_path, tf.length, tf.piece_length)
	if err != nil {
		log.Fatalf("could not open output file: %v\n", err)
	}
	defer store.close()

	pState := new_piece_state(len(tf.piece_hashes))
	restore_progress(&tf, store, pState, out_path, existed)

	st := &stats{started: time.Now()}
	saver := new_resume_saver(state_path(out_path), &tf, store, pState)

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

	// 5. Save progress on Ctrl+C signal
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interrupt
		log.Printf("interrupted, saving progress (%d/%d pieces)\n", pState.count(), len(tf.piece_hashes))
		if err := saver.flush(); err != nil {
			log.Printf("could not save resume state: %v\n", err)
		}
		os.Exit(130)
	}()

	// 6. Run main download pipeline
	if err := tf.download(store, pState, saver.piece_done); err != nil {
		saver.flush()
		close(stopDash)
		log.Fatalf("download failed: %v\n", err)
	}

	close(stopDash)

	if err := saver.flush(); err != nil {
		log.Fatalf("could not save resume state: %v\n", err)
	}

	log.Println("Download completed successfully!")
}