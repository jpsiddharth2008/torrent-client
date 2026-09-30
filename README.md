# BitTorrent Client 

A concurrent BitTorrent client written in Go implementing the core BitTorrent Peer Protocol (BEP 0003).

> **Note to Participants:**
> This codebase is intentionally incomplete and contains implementation defects. Please consult the **Problem Statement** document for your submission guidelines and evaluation criteria.

## Features (Baseline Framework)

* `.torrent` metainfo file parsing via `bencode-go`
* HTTP Tracker Announce client
* Concurrent piece downloading across multiple peers using worker goroutines and channels
* Block pipelining 16 KB chunk requests and SHA-1 piece verification

## BitTorrent Protocol Overview

1. **Metainfo (.torrent):** Parses piece length, file size, and the SHA-1 piece hash string.
2. **Tracker Announce:** Contacts the tracker over HTTP to request active peer `IP:Port` combinations.
3. **Peer Handshake:** Connects to peers over TCP and exchanges protocol handshakes.
4. **Piece Transfer:** Sends `interested`, receives `unchoke`, and pipelines 16 KB block requests using `request` messages (`index`, `begin`, `length`).

## Getting Started

### Prerequisites

* **Go 1.20+** installed on your system.

### Building & Running

1. **Clone the repository:**
```bash
git clone https://github.com/TatHack-Tathva/torrent-client.git
cd torrent-client

```

2. **Download dependencies:**
```bash
go mod download

```

3. **Build the binary:**
```bash
go build -o torrent-client .

```

4. **Run against a sample torrent file:**
```bash
./torrent-client sample.torrent output.bin

```

### Flags

| Flag | Default | Purpose |
|------|---------|---------|
| `--port` | 6881 | TCP port for incoming peers, also announced to the tracker |
| `--seed` | off | Keep running after the download finishes and upload to peers |
| `--peer` | — | Connect only to these `host:port` peers instead of asking the tracker |
| `--max-down` | 0 | Download limit in KB/s (0 = unlimited) |
| `--max-up` | 0 | Upload limit in KB/s (0 = unlimited) |
| `--no-ui` | off | Print log lines instead of the live dashboard |

The dashboard is coloured when stdout is a terminal. It respects the
[`NO_COLOR`](https://no-color.org) convention and switches itself off for
`TERM=dumb` or redirected output.

### Trying it locally

Two instances on one machine, one seeding and one downloading from it, is
enough to exercise the whole protocol without a tracker:

```bash
# terminal 1 — seed a file you already have
./torrent-client --seed --port 6881 demo.torrent original.bin

# terminal 2 — download it back, throttled so the dashboard is readable
./torrent-client --port 6882 --peer 127.0.0.1:6881 --max-down 2048 demo.torrent copy.bin
```

Interrupt the second one with Ctrl+C and run it again to watch it resume:
it rechecks the pieces already on disk and fetches only what is missing.