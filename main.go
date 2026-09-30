package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <torrent file> <output file>\n", os.Args[0])
		os.Exit(1)
	}

	torrent_path := os.Args[1]
	output_path := os.Args[2]

	tf, err := open(torrent_path)
	if err != nil {
		log.Fatalf("could not open torrent file: %v\n", err)
	}

	// checked before open_storage creates the file: an output file left by an
	// earlier run is worth rechecking even if its state file is gone
	_, stat_err := os.Stat(output_path)
	existed := stat_err == nil

	store, err := open_storage(output_path, tf.length, tf.piece_length)
	if err != nil {
		log.Fatalf("could not open output file: %v\n", err)
	}
	defer store.close()

	state := new_piece_state(len(tf.piece_hashes))
	restore_progress(&tf, store, state, output_path, existed)

	saver := new_resume_saver(state_path(output_path), &tf, store, state)

	// save progress on Ctrl+C so the next run resumes from here
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interrupt
		log.Printf("interrupted, saving progress (%d/%d pieces)\n", state.count(), len(tf.piece_hashes))
		if err := saver.flush(); err != nil {
			log.Printf("could not save resume state: %v\n", err)
		}
		os.Exit(130)
	}()

	if err := tf.download(store, state, saver.piece_done); err != nil {
		saver.flush()
		log.Fatalf("download failed: %v\n", err)
	}

	if err := saver.flush(); err != nil {
		log.Fatalf("could not save resume state: %v\n", err)
	}

	fmt.Printf("downloaded %s to %s\n", tf.name, output_path)
}
