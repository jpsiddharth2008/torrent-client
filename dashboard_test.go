package main

import (
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

func TestMakeProgressBar(t *testing.T) {
	barHalf := make_progress_bar(0.5, 10)
	expectedHalf := "[█████░░░░░]"
	if barHalf != expectedHalf {
		t.Errorf("make_progress_bar(0.5, 10) = %s; want %s", barHalf, expectedHalf)
	}
}