package main

import (
	"fmt"
	"strings"
	"time"
)

func format_bytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func format_duration(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%02dh:%02dm:%02ds", h, m, s)
	}
	return fmt.Sprintf("%02dm:%02ds", m, s)
}

func make_progress_bar(pct float64, width int) string {
	completed := int(pct * float64(width))
	if completed > width {
		completed = width
	}
	if completed < 0 {
		completed = 0
	}
	remaining := width - completed
	return fmt.Sprintf("[%s%s]", strings.Repeat("█", completed), strings.Repeat("░", remaining))
}

func render_ui(state *piece_state, st *stats, totalLength int64, speedBytesPerSec int64) {
	fmt.Print("\033[H") // Move cursor to top-left corner

	currentDownloaded := st.downloaded.Load()
	numPieces := state.num_pieces
	completedPieces := state.count()

	pct := 0.0
	if totalLength > 0 {
		pct = float64(currentDownloaded) / float64(totalLength)
		if pct > 1.0 {
			pct = 1.0
		}
	}

	var etaStr string
	if speedBytesPerSec > 0 && currentDownloaded < totalLength {
		remBytes := totalLength - currentDownloaded
		etaSec := time.Duration(remBytes/speedBytesPerSec) * time.Second
		etaStr = format_duration(etaSec)
	} else if currentDownloaded >= totalLength {
		etaStr = "Complete"
	} else {
		etaStr = "Calculating..."
	}

	elapsed := format_duration(time.Since(st.started))
	bar := make_progress_bar(pct, 30)

	fmt.Println("==================================================")
	fmt.Println("             BITTORRENT CLIENT DASHBOARD          ")
	fmt.Println("==================================================")
	fmt.Printf(" Progress:      %s %.1f%%\n", bar, pct*100)
	fmt.Printf(" Pieces:        %d / %d completed\n", completedPieces, numPieces)
	fmt.Printf(" Downloaded:    %s / %s\n", format_bytes(currentDownloaded), format_bytes(totalLength))
	fmt.Printf(" Download Rate: %s/s\n", format_bytes(speedBytesPerSec))
	fmt.Printf(" Active Peers:  %d\n", st.active_peers.Load())
	fmt.Printf(" Elapsed Time:  %s\n", elapsed)
	fmt.Printf(" ETA:           %s\n", etaStr)
	fmt.Println("==================================================")
}

func run_dashboard(state *piece_state, st *stats, totalLength int64, stopChan <-chan struct{}) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastDownloaded int64
	lastTime := time.Now()

	// Clear screen and hide cursor
	fmt.Print("\033[?25l\033[2J")
	defer fmt.Print("\033[?25h\n") // Restore cursor on exit

	for {
		select {
		case <-stopChan:
			render_ui(state, st, totalLength, 0)
			return
		case now := <-ticker.C:
			downloaded := st.downloaded.Load()
			elapsedSec := now.Sub(lastTime).Seconds()

			var speed int64
			if elapsedSec > 0 {
				speed = int64(float64(downloaded-lastDownloaded) / elapsedSec)
			}

			lastDownloaded = downloaded
			lastTime = now

			render_ui(state, st, totalLength, speed)
		}
	}
}