# braid

One download, every uplink at once. Splits a file into byte ranges and pulls
them simultaneously over Wi-Fi *and* a tethered phone, so a transfer runs at
roughly the sum of both links.

macOS only (it uses Darwin's `IP_BOUND_IF` to pin sockets to an interface).
No dependencies, no kernel extension, no VPN, no root.

## Measured

On the machine this was built against — home Wi-Fi plus an iPhone 5G tether:

```
64.9 MB in 2s (183.4 Mbps)

  en0  ███████████░░░░░░░░░  52.9%    34.4 MB   97.1 Mbps
  en5  █████████░░░░░░░░░░░  47.1%    30.6 MB   86.3 Mbps
```

Against ~107 Mbps on the best single link, that is **+71%**. The downloaded
file's SHA256 matches the publisher's published hash.

## Build and use

```sh
go build -o braid ./cmd/braid

./braid links                      # what can it use?
./braid get <url>                  # download over all of them
./braid get -o ~/Downloads <url>
```

Interrupt with Ctrl-C and run the same command again: it resumes from a
`.braid` sidecar, and refuses to resume if the remote file has changed.

Flags: `-o` output path, `-chunk` chunk size (default 4 MiB), `-workers` per
link (default 4), `-tail-steal` to re-request a stalled chunk near the end.

## Getting a second uplink

The Mac has one Wi-Fi radio, so the phone must arrive on a *separate*
interface. Either works:

- **Wi-Fi for broadband + iPhone tethered over USB.** Needs a cable with
  working data pins; a worn one shows up as interfaces appearing and vanishing
  with DHCP never answering.
- **Ethernet for broadband + Wi-Fi joined to the phone's hotspot.**

Bluetooth tethering is not an option — macOS no longer has a Bluetooth PAN port.

## Honest limits

- Only helps where the server honours byte ranges. Some do not, and some
  *lie* about it: `cdn.jsdelivr.net` answers `206` with a `Content-Range`
  total that is the compressed length, then sends the whole uncompressed body.
  braid detects that and falls back to a single stream rather than writing a
  corrupt file.
- Does nothing for Netflix, YouTube or other DRM'd adaptive streaming.
- Cellular data costs money. Every byte braid pulls over a metered link is
  billed to you.

## Layout

| Package | Responsibility |
|---|---|
| `internal/linkset` | which interfaces can carry an uplink; hot-plug and metered detection |
| `internal/dial` | transports pinned per interface *and* address family |
| `internal/probe` | size, splittability, cache validators |
| `internal/plan` | chunk arithmetic, completion bitmap, resume sidecar |
| `internal/sched` | shared-queue work stealing, retries, tail stealing |
| `internal/xfer` | one transfer end to end |
| `internal/human` | formatting for people |
| `spike/` | throwaway Phase 0 measurement harness |

Prior art: [`anmolkapil/plexo`](https://github.com/anmolkapil/plexo) (MIT), an
Electron download manager built on the same core insight.
