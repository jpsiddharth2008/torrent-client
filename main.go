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

func main() {
	no_ui := flag.Bool("no-ui", false, "print log lines instead of the live dashboard")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [--no-ui] <torrent file> <output file>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(1)
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

	// serve verified pieces to other peers while we download (issue #9)
	seeder, err := start_seeder(6881, &tf, store, state, st)
	if err != nil {
		log.Printf("seeding disabled, could not start listener: %v\n", err)
	} else {
		defer seeder.stop()
		log.Println("seeding listener active on port 6881")
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
		fmt.Printf("\nstopped at %d/%d pieces, progress saved; run the same command again to resume\n", state.count(), len(tf.piece_hashes))
		os.Exit(130)
	}()

	if err := tf.download(store, state, st, saver.piece_done); err != nil {
		stop_ui()
		saver.flush()
		log.Printf("download failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "download failed: %v\n", err)
		os.Exit(1)
	}
	stop_ui()

	if err := saver.flush(); err != nil {
		log.Printf("could not save resume state: %v\n", err)
		fmt.Fprintf(os.Stderr, "could not save resume state: %v\n", err)
		os.Exit(1)
	}

	elapsed := time.Since(st.started)
	avg := int64(float64(st.downloaded.Load()) / elapsed.Seconds())
	fmt.Printf("downloaded %s to %s in %s (avg %s/s)\n", tf.name, output_path, format_duration(elapsed), format_bytes(avg))
}
