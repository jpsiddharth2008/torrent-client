# BitTorrent Client

A concurrent BitTorrent client in Go, implementing the core peer wire protocol
(BEP 3). It downloads a file in pieces from many peers at once, verifies every
piece against its SHA-1 before keeping it, resumes interrupted transfers, and
uploads back to the swarm.

## Features

**Protocol**

- `.torrent` metainfo parsing, with the info hash taken from the real info
  dictionary so it matches what trackers and peers compute
- HTTP tracker announce, including `failure reason` handling
- Peer handshake, bitfield exchange, and `have` tracking
- Pipelined 16 KB block requests with a bounded in-flight window
- SHA-1 verification of every piece before it is written

**Beyond the baseline**

- **Live terminal dashboard** — progress bar, piece map, download and upload
  speed, ETA, active workers, all redrawn in place
- **Resume** — piece state is saved to disk, and an interrupted download picks
  up where it stopped. State is verified, not trusted: every piece is rehashed
  on startup, so a corrupted file repairs itself
- **Seeding** — a TCP listener that accepts incoming handshakes and serves
  piece requests, sharing verified pieces while the download is still running
- **Bandwidth limiting** — `--max-down` and `--max-up`, a token bucket shared
  across all peers so the cap is a total rather than a per-peer allowance
- **Hardening** against hostile peers: message length caps, request size
  limits, bounded peer counts, and range checks on peer-supplied indices

## Quick start

Requires **Go 1.20+**.

```bash
git clone https://github.com/jpsiddharth2008/torrent-client.git
cd torrent-client
go mod download
go build -o torrent-client .
```

Against a real torrent:

```bash
./torrent-client file.torrent output.bin
```

## Trying it locally

You do not need a tracker or a real swarm. `mkdemo` generates a payload and a
matching torrent, and two instances of the client talk to each other:

```bash
go run ./cmd/mkdemo -size 32
```

That writes `demo/payload.bin` and `demo/demo.torrent`, then prints the two
commands below.

```bash
# terminal 1 — seed the file
./torrent-client --seed --port 6881 demo/demo.torrent demo/payload.bin

# terminal 2 — download it back, throttled so the dashboard is readable
./torrent-client --port 6882 --peer 127.0.0.1:6881 --max-down 2048 demo/demo.torrent demo/copy.bin
```

On Windows the binary is `torrent-client.exe` and paths use backslashes;
everything else is identical.

**Watching resume work.** Interrupt the download with Ctrl+C and run the exact
same command again. It rechecks what is already on disk and fetches only what
is missing. To see that it genuinely verifies rather than trusting its own
bookkeeping, corrupt a few bytes in the middle of a finished file, delete the
`.state` file, and run it again: it reports fewer verified pieces and
re-downloads precisely the damaged ones.

## Flags

| Flag | Default | Purpose |
|------|---------|---------|
| `--port` | 6881 | TCP port for incoming peers, also announced to the tracker |
| `--seed` | off | Keep running after the download finishes and upload to peers |
| `--peer` | — | Connect only to these `host:port` peers instead of asking the tracker |
| `--max-down` | 0 | Download limit in KB/s (0 = unlimited) |
| `--max-up` | 0 | Upload limit in KB/s (0 = unlimited) |
| `--no-ui` | off | Print log lines instead of the live dashboard |

The dashboard is coloured when stdout is a terminal. It honours the
[`NO_COLOR`](https://no-color.org) convention and switches itself off for
`TERM=dumb` or redirected output, so piped output stays plain.

## How it works

```
 .torrent ──► tracker announce ──► peer list
                                      │
                                      ▼
             ┌──────────── work channel (pieces to fetch) ────────────┐
             │                        │                        │      │
          worker                   worker                   worker    │
        (1 per peer)             (1 per peer)             (1 per peer) │
             │                        │                        │      │
             └──────────── results channel (verified) ─────────┘      │
                                      │                               │
                                      ▼                  failed piece ┘
                          storage ──► disk
                                      │
                             piece state ──► .state file (resume)
                                      │
                                      ▼
                         seeder ──► serves verified pieces
```

One goroutine per peer, each pulling pieces off a shared work channel. A piece
that fails its hash check, or whose peer disconnects, goes back on the channel
for someone else to try. Finished pieces go to the main loop, which writes them
to disk, records them in the piece state, and tells the seeder to announce them.

The dashboard and the resume saver both read that same piece state, which is
why progress survives a restart: the bar reflects verified pieces on disk, not
bytes received this session.

## The bugs

The scaffold shipped with four deliberate protocol defects. Each is documented
in its own issue, and each has a regression test that was checked against the
unfixed code to confirm it actually catches the bug.

| # | Bug | Effect |
|---|-----|--------|
| [#1](https://github.com/jpsiddharth2008/torrent-client/issues/1) | Info hash computed from a lossy struct re-encode | Tracker and every peer rejected us |
| [#2](https://github.com/jpsiddharth2008/torrent-client/issues/2) | `format_request` wrote little-endian | A 16 KB request went out as 4 MB |
| [#3](https://github.com/jpsiddharth2008/torrent-client/issues/3) | `has_piece` read LSB-first, `set_piece` wrote MSB-first | Asked the wrong peers for the wrong pieces |
| [#4](https://github.com/jpsiddharth2008/torrent-client/issues/4) | Request backlog never decremented | Any piece over 80 KB stalled forever |

They form a chain — each blocks a different stage, so fixing any one alone
still leaves a client that downloads nothing.

## Tests

```bash
go vet ./...
go test ./...
```

Coverage includes the four protocol bugs, bitfield ordering, message framing,
resume and corruption repair, the seeder's request handling, rate limiter
behaviour under concurrency, and hardening against malformed peer input.

The end-to-end piece transfer test runs over a real loopback TCP socket rather
than `net.Pipe`: `net.Pipe` is unbuffered, so a scripted peer's reply blocks
while the client is still pipelining requests, which deadlocks the harness
instead of exercising the code.

## Troubleshooting

**`bind: address already in use`** (on Windows, *"Only one usage of each socket
address is normally permitted"*) — something is already listening on that port,
usually a seeder still running from an earlier attempt. Either stop it:

```bash
pkill -f torrent-client          # Windows: taskkill /IM torrent-client.exe /F
```

or just pick different ports. They are arbitrary; only `--peer` needs to match
whatever the seeder is actually listening on.

**Dashboard looks garbled on Windows** — use Windows Terminal rather than the
legacy `cmd.exe` console, which handles in-place redraw and colour poorly.

**A firewall prompt appears on first run** — the client opens a listening
socket for incoming peers. Allow it, or seeding will not accept connections.

## AI usage disclosure

This project was developed with AI assistance (Claude). AI tools were used for
debugging the protocol defects, writing the regression test suite, and drafting
documentation. All changes were reviewed by, and are understood by, the team
members who submitted them.
