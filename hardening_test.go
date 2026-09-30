package main

import (
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadMessageRejectsOversizedLength(t *testing.T) {
	conn_a, conn_b := net.Pipe()
	defer conn_a.Close()
	defer conn_b.Close()

	// a 4 GB length prefix must be refused before anything is allocated
	go func() {
		prefix := make([]byte, 4)
		binary.BigEndian.PutUint32(prefix, 0xFFFFFFFF)
		conn_a.Write(prefix)
	}()

	if _, err := read_message(conn_b); err == nil {
		t.Error("expected an error for a 4 GB message length")
	}
}

func TestRequestPeersReportsTrackerFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("d14:failure reason22:torrent not registerede"))
	}))
	defer server.Close()

	tf := &torrent_file{announce: server.URL}
	_, err := tf.request_peers([20]byte{}, 6881, 0)
	if err == nil || !strings.Contains(err.Error(), "torrent not registered") {
		t.Errorf("err = %v, want the tracker's failure reason", err)
	}
}

func TestRequestPeersRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	tf := &torrent_file{announce: server.URL}
	if _, err := tf.request_peers([20]byte{}, 6881, 0); err == nil {
		t.Error("expected an error for HTTP 503")
	}
}

func TestOpenStorageRefusesUnrelatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	original := []byte("important data that is not a torrent download")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := open_storage(path, 1000, 100); err == nil {
		t.Fatal("expected open_storage to refuse a file of a different size")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Error("open_storage modified the existing file")
	}
}

func TestOpenStorageExtendsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}

	store, err := open_storage(path, 1000, 100)
	if err != nil {
		t.Fatalf("open_storage on an empty file: %v", err)
	}
	store.close()

	if info, _ := os.Stat(path); info.Size() != 1000 {
		t.Errorf("size = %d, want 1000", info.Size())
	}
}
