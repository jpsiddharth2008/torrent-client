package main

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	dashboard_refresh = 500 * time.Millisecond
	// weight of the newest sample in the smoothed speed; peers send data in
	// bursts, so the raw per-tick rate jumps around too much to read
	speed_smoothing = 0.3
	bar_width       = 40
	piece_map_width = 60
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

// format_cap renders the limiter's ceiling for display, or nothing at all when
// the direction is unlimited.
func format_cap(r *rate_limiter) string {
	if r.limit() == 0 {
		return ""
	}
	return fmt.Sprintf(" (capped %s/s)", format_bytes(r.limit()))
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

// make_piece_map squeezes the have-bitfield into width cells: full when every
// piece in the cell is done, shaded when some are, light when none are.
func make_piece_map(have bitfield, num_pieces, width int) string {
	if num_pieces <= 0 {
		return ""
	}
	if width > num_pieces {
		width = num_pieces
	}

	var b strings.Builder
	for cell := 0; cell < width; cell++ {
		first := cell * num_pieces / width
		last := (cell + 1) * num_pieces / width
		done := 0
		for i := first; i < last; i++ {
			if have.has_piece(i) {
				done++
			}
		}
		switch {
		case done == last-first:
			b.WriteString("█")
		case done > 0:
			b.WriteString("▒")
		default:
			b.WriteString("░")
		}
	}
	return b.String()
}

// dashboard redraws a live status screen in the terminal from the shared
// piece_state and stats, which the download goroutines update.
type dashboard struct {
	t     *torrent_file
	state *piece_state
	st    *stats
	out   io.Writer

	// optional; when set the configured cap is shown beside the live speed
	down_lim *rate_limiter
	up_lim   *rate_limiter

	pal palette // zero value renders without colour

	down_speed float64 // smoothed bytes/s
	up_speed   float64
	last_down  int64
	last_up    int64
	last_time  time.Time
	sampled    bool

	stop_once sync.Once
	stop_ch   chan struct{}
	done_ch   chan struct{}
}

func new_dashboard(t *torrent_file, state *piece_state, st *stats, out io.Writer) *dashboard {
	return &dashboard{
		t:       t,
		state:   state,
		st:      st,
		out:     out,
		stop_ch: make(chan struct{}),
		done_ch: make(chan struct{}),
	}
}

// sample folds the bytes moved since the last call into the smoothed speeds.
func (d *dashboard) sample(now time.Time) {
	down := d.st.downloaded.Load()
	up := d.st.uploaded.Load()

	if d.sampled {
		elapsed := now.Sub(d.last_time).Seconds()
		if elapsed > 0 {
			inst_down := float64(down-d.last_down) / elapsed
			inst_up := float64(up-d.last_up) / elapsed
			d.down_speed = speed_smoothing*inst_down + (1-speed_smoothing)*d.down_speed
			d.up_speed = speed_smoothing*inst_up + (1-speed_smoothing)*d.up_speed
		}
	}

	d.last_down, d.last_up, d.last_time, d.sampled = down, up, now, true
}

// render builds one full frame. Progress comes from verified pieces, not
// bytes received this session, so a resumed download starts where it left off.
func (d *dashboard) render(now time.Time) string {
	num_pieces := len(d.t.piece_hashes)
	done_pieces := d.state.count()
	left := d.t.bytes_left(d.state)
	done_bytes := int64(d.t.length - left)

	pct := 0.0
	if d.t.length > 0 {
		pct = float64(done_bytes) / float64(d.t.length)
	}

	status := "Downloading"
	eta := "--"
	switch {
	case done_pieces == num_pieces && d.st.seeding.Load():
		status = "Seeding (download complete, uploading to peers)"
		eta = "done"
	case done_pieces == num_pieces:
		status = "Complete"
		eta = "done"
	case d.st.active_peers.Load() == 0:
		status = "Waiting for peers"
	case d.down_speed >= 1:
		eta = format_duration(time.Duration(float64(left)/d.down_speed) * time.Second)
	}

	p := d.pal
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteString("\033[K\n") // clear what's left of the previous frame's line
	}
	// labels are recessive so the eye lands on the figures, not the chrome
	label := func(s string) string { return p.paint(p.muted, s) }

	// header: what is being transferred, and its state, on one line
	line(" %s   %s",
		p.paint(p.heading, d.t.name),
		p.paint(p.status_token(status), status))
	line(" %s", p.paint(p.muted, strings.Repeat("─", bar_width+24)))

	// the headline figure: bar, percentage and piece count together
	line(" %s  %s   %s",
		p.colour_bar(make_progress_bar(pct, bar_width), p.success),
		p.paint(p.bold, fmt.Sprintf("%5.1f%%", pct*100)),
		p.paint(p.accent, fmt.Sprintf("%d/%d pieces", done_pieces, num_pieces)))
	line("")

	// transfer rates, the numbers that move every frame, paired with the
	// two time figures so the whole "how fast, how long" story is one block
	line(" %s %s%s      %s %s",
		p.paint(p.success, "↓"),
		p.paint(p.bold, format_bytes(int64(d.down_speed))+"/s"),
		label(format_cap(d.down_lim)),
		label("ETA     "), p.paint(p.accent, eta))
	line(" %s %s%s      %s %s",
		p.paint(p.accent, "↑"),
		p.paint(p.bold, format_bytes(int64(d.up_speed))+"/s"),
		label(format_cap(d.up_lim)),
		label("Elapsed "), format_duration(now.Sub(d.st.started)))
	line("")

	// totals and connection counts, the slower-moving context
	line(" %s %s %s %s   %s",
		label("Downloaded"), format_bytes(done_bytes),
		label("/"), format_bytes(int64(d.t.length)),
		label(fmt.Sprintf("(%s this session)", format_bytes(d.st.downloaded.Load()))))
	line(" %s %s   %s",
		label("Uploaded  "), format_bytes(d.st.uploaded.Load()),
		label(fmt.Sprintf("(%d peers downloading from us)", d.st.upload_peers.Load())))
	line(" %s %s   %s",
		label("Workers   "), p.paint(p.accent, fmt.Sprintf("%d active", d.st.active_peers.Load())),
		label(fmt.Sprintf("(%d peers known)", d.st.known_peers.Load())))
	line("")

	line(" %s %s", label("Pieces    "), p.colour_piece_map(make_piece_map(d.state.snapshot(), num_pieces, piece_map_width)))
	line("            %s %s  %s %s  %s %s",
		p.paint(p.success, "█"), label("done"),
		p.paint(p.warning, "▒"), label("partial"),
		p.paint(p.muted, "░"), label("missing"))
	line("")
	line(" %s", label("Ctrl+C to stop — progress is saved"))
	return b.String()
}

func (d *dashboard) draw(now time.Time) {
	// cursor home, frame, then clear anything below it
	fmt.Fprint(d.out, "\033[H"+d.render(now)+"\033[J")
}

// start clears the screen and redraws until stop is called.
func (d *dashboard) start() {
	fmt.Fprint(d.out, "\033[?25l\033[2J") // hide cursor, clear screen

	go func() {
		defer close(d.done_ch)
		ticker := time.NewTicker(dashboard_refresh)
		defer ticker.Stop()

		d.sample(time.Now())
		d.draw(time.Now())
		for {
			select {
			case <-d.stop_ch:
				d.draw(time.Now())
				fmt.Fprint(d.out, "\033[?25h") // show cursor again
				return
			case now := <-ticker.C:
				d.sample(now)
				d.draw(now)
			}
		}
	}()
}

// stop draws a final frame, restores the cursor and waits for the redraw
// goroutine to exit. Safe to call more than once.
func (d *dashboard) stop() {
	d.stop_once.Do(func() { close(d.stop_ch) })
	<-d.done_ch
}
