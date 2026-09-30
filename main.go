package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// how often a seeding client re-announces itself so the tracker keeps
// listing it (trackers typically ask for 30 minutes)
const seed_announce_interval = 30 * time.Minute

func main() {
	no_ui := flag.Bool("no-ui", false, "print log lines instead of the live dashboard")
	port := flag.Int("port", 6881, "TCP port to accept peer connections on, also announced to the tracker")
	seed := flag.Bool("seed", false, "keep running after the download completes and upload to other peers until Ctrl+C")
	peer_list := flag.String("peer", "", "connect only to these peers (host:port, comma-separated) instead of asking the tracker")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <torrent file> <output file>\n\nflags:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nexample, seed a finished file and download it again from it locally:\n"+
			"  %[1]s --seed --port 6881 file.torrent file.iso\n"+
			"  %[1]s --port 6882 --peer 127.0.0.1:6881 file.torrent copy.iso\n", os.Args[0])
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(1)
	}
	if *port < 1 || *port > 65535 {
		fmt.Fprintf(os.Stderr, "--port must be between 1 and 65535\n")
		os.Exit(1)
	}

	var fixed_peers []peer
	if *peer_list != "" {
		var err error
		if fixed_peers, err = parse_peers(*peer_list); err != nil {
			fmt.Fprintf(os.Stderr, "--peer: %v\n", err)
			os.Exit(1)
		}
	}

	torrent_path := flag.Arg(0)
	output_path := flag.Arg(1)

	tf, err := open(torrent_path)
	if err != nil {
		log.Fatalf("could not open torrent file: %v\n", err)
	}

	// the dashboard owns the terminal, so log lines go to a file instead of
	// being drawn over it; fall back to plain logs if stdout isn't a console
	use_ui := !*no_ui && enable_ansi()
	if use_ui {
		log_path := output_path + ".log"
		log_file, err := os.OpenFile(log_path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("could not open log file: %v\n", err)
		}
		defer log_file.Close()
		log.SetOutput(log_file)
		fmt.Printf("logging to %s\nchecking existing data...\n", log_path)
	}

	// checked before open_storage creates the file: an output file left by an
	// earlier run is worth rechecking even if its state file is gone
	_, stat_err := os.Stat(output_path)
	existed := stat_err == nil

	store, err := open_storage(output_path, tf.length, tf.piece_length)
	if err != nil {
		log.Printf("could not open output file: %v\n", err)
		fmt.Fprintf(os.Stderr, "could not open output file: %v\n", err)
		os.Exit(1)
	}
	defer store.close()

	state := new_piece_state(len(tf.piece_hashes))
	restore_progress(&tf, store, state, output_path, existed)

	saver := new_resume_saver(state_path(output_path), &tf, store, state)
	st := &stats{started: time.Now()}

	// one identity for both directions, so peers (and the tracker) see the
	// downloader and the seeder as the same client
	peer_id, err := new_peer_id()
	if err != nil {
		log.Fatalf("could not generate peer id: %v\n", err)
	}

	// serve verified pieces to other peers while we download (issue #9)
	seeder, err := start_seeder(*port, &tf, store, state, st, peer_id)
	if err != nil {
		log.Printf("seeding disabled, could not start listener: %v\n", err)
		if *seed {
			fmt.Fprintf(os.Stderr, "--seed: could not listen on port %d: %v\n", *port, err)
			os.Exit(1)
		}
	} else {
		defer seeder.stop()
		log.Printf("seeding listener active on port %d\n", *port)
	}

	var ui *dashboard
	if use_ui {
		ui = new_dashboard(&tf, state, st, os.Stdout)
		ui.start()
	}
	stop_ui := func() {
		if ui != nil {
			ui.stop()
		}
	}

	// save progress on Ctrl+C so the next run resumes from here
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interrupt
		stop_ui()
		log.Printf("interrupted, saving progress (%d/%d pieces)\n", state.count(), len(tf.piece_hashes))
		if err := saver.flush(); err != nil {
			log.Printf("could not save resume state: %v\n", err)
		}
		if st.seeding.Load() {
			fmt.Printf("\nstopped seeding after uploading %s\n", format_bytes(st.uploaded.Load()))
		} else {
			fmt.Printf("\nstopped at %d/%d pieces, progress saved; run the same command again to resume\n", state.count(), len(tf.piece_hashes))
		}
		os.Exit(130)
	}()

	// each finished piece is saved for resume and announced to every peer
	// downloading from us, so they can request it straight away
	on_piece := func(index int) {
		saver.piece_done(index)
		if seeder != nil {
			seeder.broadcast_have(index)
		}
	}

	opts := download_options{peer_id: peer_id, port: uint16(*port), peers: fixed_peers}
	if err := tf.download(store, state, st, opts, on_piece); err != nil {
		stop_ui()
		saver.flush()
		log.Printf("download failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "download failed: %v\n", err)
		os.Exit(1)
	}

	if err := saver.flush(); err != nil {
		stop_ui()
		log.Printf("could not save resume state: %v\n", err)
		fmt.Fprintf(os.Stderr, "could not save resume state: %v\n", err)
		os.Exit(1)
	}

	if *seed {
		st.seeding.Store(true)
		log.Printf("download complete, seeding on port %d until Ctrl+C\n", *port)
		if !use_ui {
			fmt.Printf("download complete, seeding on port %d; press Ctrl+C to stop\n", *port)
		}
		// tell the tracker we're a seed (left=0) so other peers are sent to
		// us; with --peer there is no tracker to tell
		if len(fixed_peers) == 0 {
			go announce_as_seed(&tf, opts)
		}
		select {} // the Ctrl+C handler exits
	}
	stop_ui()

	elapsed := time.Since(st.started)
	avg := int64(float64(st.downloaded.Load()) / elapsed.Seconds())
	fmt.Printf("downloaded %s to %s in %s (avg %s/s)\n", tf.name, output_path, format_duration(elapsed), format_bytes(avg))
}

// announce_as_seed registers us with the tracker as a complete copy, and
// repeats it so the tracker doesn't drop us.
func announce_as_seed(tf *torrent_file, opts download_options) {
	for {
		if _, err := tf.request_peers(opts.peer_id, opts.port, 0); err != nil {
			log.Printf("seed announce failed: %v\n", err)
		} else {
			log.Println("announced to tracker as a seed")
		}
		time.Sleep(seed_announce_interval)
	}
}
