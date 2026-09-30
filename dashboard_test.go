package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{500, "500 B"},
		{1024, "1.00 KB"},
		{1048576, "1.00 MB"},
		{792723456, "756.00 MB"},
	}

	for _, tt := range tests {
		got := format_bytes(tt.input)
		if got != tt.expected {
			t.Errorf("format_bytes(%d) = %s; want %s", tt.input, got, tt.expected)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		input    time.Duration
		expected string
	}{
		{45 * time.Second, "00m:45s"},
		{125 * time.Second, "02m:05s"},
		{3665 * time.Second, "01h:01m:05s"},
	}

	for _, tt := range tests {
		got := format_duration(tt.input)
		if got != tt.expected {
			t.Errorf("format_duration(%v) = %s; want %s", tt.input, got, tt.expected)
		}
	}
}

func TestMakePieceMap(t *testing.T) {
	// 8 pieces in 4 cells of 2: both done, one done, none done, both done
	have := bitfield{0}
	for _, i := range []int{0, 1, 2, 6, 7} {
		have.set_piece(i)
	}
	if got, want := make_piece_map(have, 8, 4), "█▒░█"; got != want {
		t.Errorf("make_piece_map = %s, want %s", got, want)
	}

	// never wider than the number of pieces
	if got := make_piece_map(have, 3, 60); len([]rune(got)) != 3 {
		t.Errorf("make_piece_map for 3 pieces has %d cells, want 3", len([]rune(got)))
	}
}

func TestDashboardSmoothsSpeed(t *testing.T) {
	tf, _ := make_test_torrent(10, 64, 30)
	st := &stats{started: time.Now()}
	d := new_dashboard(tf, new_piece_state(10), st, io.Discard)

	now := time.Now()
	d.sample(now)
	st.downloaded.Add(1000)
	d.sample(now.Add(time.Second))

	// first real sample moves the average 30% of the way to 1000 B/s
	if d.down_speed < 299 || d.down_speed > 301 {
		t.Errorf("down_speed = %.1f, want ~300", d.down_speed)
	}

	// a quiet second decays it rather than dropping straight to zero
	d.sample(now.Add(2 * time.Second))
	if d.down_speed < 209 || d.down_speed > 211 {
		t.Errorf("down_speed after idle second = %.1f, want ~210", d.down_speed)
	}
}

func TestDashboardCountsResumedPieces(t *testing.T) {
	tf, _ := make_test_torrent(10, 64, 30)
	state := new_piece_state(10)
	for i := 0; i < 5; i++ {
		state.mark_done(i)
	}
	st := &stats{started: time.Now()}

	// nothing downloaded this session, yet progress reflects pieces on disk
	frame := new_dashboard(tf, state, st, io.Discard).render(time.Now())
	if !strings.Contains(frame, "5/10 pieces") {
		t.Errorf("frame does not show 5/10 pieces:\n%s", frame)
	}
	if !strings.Contains(frame, "Waiting for peers") {
		t.Errorf("frame with no active peers should say waiting:\n%s", frame)
	}
}

func TestDashboardStopIsIdempotent(t *testing.T) {
	tf, _ := make_test_torrent(4, 64, 64)
	var out bytes.Buffer
	d := new_dashboard(tf, new_piece_state(4), &stats{started: time.Now()}, &out)

	d.start()
	d.stop()
	d.stop() // second call must not panic or block

	if !strings.HasSuffix(out.String(), "\033[?25h") {
		t.Error("cursor was not restored on stop")
	}
}

func TestMakeProgressBar(t *testing.T) {
	barHalf := make_progress_bar(0.5, 10)
	expectedHalf := "[█████░░░░░]"
	if barHalf != expectedHalf {
		t.Errorf("make_progress_bar(0.5, 10) = %s; want %s", barHalf, expectedHalf)
	}
}
