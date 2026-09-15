# braid

**One download, every uplink at once.**

Your Mac usually has more than one way onto the internet — Wi-Fi, an Ethernet
adapter, a phone tethered over USB — and uses exactly one of them. macOS routes
everything through a single default gateway, so the others sit idle no matter
how many connections you open.

braid splits a file into byte ranges and pulls them over all of them
simultaneously, so a single transfer runs at roughly the **sum** of your links.

```
$ braid get https://dl.google.com/go/go1.27.1.darwin-arm64.tar.gz

https://dl.google.com/go/go1.27.1.darwin-arm64.tar.gz
over 2 link(s): Wi-Fi, iPhone USB

  ████████████████████████ 100.0%    64.9 MB   187.2 Mbps  eta 0s

saved go1.27.1.darwin-arm64.tar.gz
64.9 MB in 2s (183.4 Mbps)

  en0  ███████████░░░░░░░░░  52.9%    34.4 MB   97.1 Mbps
  en5  █████████░░░░░░░░░░░  47.1%    30.6 MB   86.3 Mbps
```

## Does it actually work?

Measured on the machine it was built against — home Wi-Fi plus an iPhone 5G
tether over USB:

| | |
|---|---|
| Wi-Fi alone | 94.1 Mbps |
| iPhone 5G tether alone | 107.3 Mbps |
| **Both together** | **183.4 Mbps** |
| Gain over the best single link | **+71%** |
| Integrity | SHA256 matched the publisher's published hash exactly |

That last row matters more than the speed. Seventeen chunks were pulled out of
order across two different network interfaces and reassembled
**byte-identically** to Google's published `ee215d57…87d12`.

## Requirements

- **macOS.** Link discovery and socket pinning use Darwin-specific APIs.
  Nothing else is needed: no kernel extension, no VPN, no root, and no
  dependencies outside the standard library.
- **Go 1.22+** to build.
- **Two working uplinks**, which is the only fiddly part — see below.

## Install

```sh
git clone https://github.com/shaaz1000/braid.git
cd braid
go build -o braid ./cmd/braid
```

## Usage

```sh
braid links                          # what can it use right now?
braid get <url>                      # download over everything available
braid get -o ~/Downloads <url>       # choose where it lands
braid get -o out.iso <url>           # or the exact filename
braid serve                          # share the bonded speed with every device
```

`braid links` tells you whether bonding is even possible:

```
LINK                 IFACE   IDX   IPv4             IPv6                    METERED
Wi-Fi                en0     11    192.168.1.42     2001:db8:1::a1b2…   no
iPhone USB           en5     21    172.20.10.2      2001:db8:2::c3d4…   yes

2 uplinks — a download will be split across all of them.
```

**Interrupt and resume.** Ctrl-C leaves a small `.braid` sidecar next to the
output. Run the same command again and it picks up exactly where it stopped —
and it *refuses* to resume if the remote file has changed, because stitching
two different files together is worse than downloading again.

### Flags

| Flag | Default | |
|---|---|---|
| `-o <path>` | current directory | output directory or exact file path |
| `-chunk <bytes>` | 4 MiB | chunk size. Smaller is measurably worse: 1 MiB managed 82 Mbps where 4 MiB managed 183 |
| `-workers <n>` | 4 | concurrent fetches per link |
| `-tail-steal` | off | near the end, let an idle link re-request a chunk stuck on a slow one. Costs duplicate bytes, so it is opt-in |

## Every device on your network

`braid serve` turns the Mac into a bonded gateway. Clients install **nothing**:

```sh
braid serve
# braid is sharing 2 uplink(s) on port 8080
#   http://192.168.1.42:8080/?t=<token>
```

Open that on a phone, a laptop or a TV and you get a live dashboard. Paste a
link and press **Stream** to download through the bonded connection, or **Copy
link** to get a self-contained URL you can paste into VLC or Infuse — it
carries its own token, because those clients cannot set request headers.

The endpoint answers an ordinary sequential HTTP response while the bytes
behind it are being fetched out of order over every uplink, and it honours
`Range` so players can seek. A repeat request for the same URL is served from
disk, so it costs no mobile data twice.

On iOS, **Add to Home Screen** gives it an icon and a full-screen window.

A deliberate limit: the daemon binds to every interface, because a phone cannot
reach something listening only on localhost. That exposure is why a token is
mandatory and why there is no way to turn it off.

## Getting a second uplink

Your Mac has **one Wi-Fi radio**, so the phone has to arrive on a *separate*
interface. Either of these works:

| | Broadband via | Cellular via |
|---|---|---|
| **A** | Wi-Fi | iPhone tethered over **USB** |
| **B** | Ethernet adapter | Wi-Fi joined to the phone's hotspot |

Turn on Personal Hotspot (Settings → Personal Hotspot → Allow Others to Join),
keep the phone unlocked, and make sure cellular data is on.

Two things that will waste your afternoon if you don't know them:

- **A worn cable is the most likely failure.** The symptom is interfaces
  appearing and vanishing (`en5`, `en14`, …) with DHCP never answering, leaving
  a self-assigned `169.254.x.x` address. It looks like a software problem and
  is not. Try another cable, straight into the Mac rather than through a hub.
- **Bluetooth tethering is not an option.** macOS no longer has a Bluetooth PAN
  port at all; Instant Hotspot replaced it.

## How it works

Three primitives, and one of them is the whole trick.

**1. HTTP range requests.** Most servers will hand over an arbitrary slice of a
file and say so with `206 Partial Content`. braid probes with a *one-byte
ranged GET* rather than a `HEAD`, because CDNs frequently misreport `HEAD`
while a real 206 is proof.

**2. Pinning a socket to an interface.** This is what makes bonding possible at
all. Binding a source address is *not* enough on macOS — the kernel still
consults the single default route. You need Darwin's `IP_BOUND_IF` /
`IPV6_BOUND_IF` socket options, which force a socket onto one interface's
scoped route:

```go
syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, 25 /* IP_BOUND_IF */, ifIndex)
```

braid applies both, per address family, because a link may have IPv4 only.

**3. A shared work-stealing queue.** Nobody is handed a fixed share. Every
worker leases the next chunk from one queue, so a link that turns out slow
simply completes fewer chunks. Splitting a file 50/50 up front would make every
transfer wait for the slower link — and measurement showed link speeds are
neither equal nor stable: the cellular link lost 24% of its throughput the
moment both ran at once.

```
                  ┌── Wi-Fi     en0 ──┐
file ──→ chunks ──┤                   ├──→ one sparse file, written at true offsets
                  └── iPhone    en5 ──┘
```

Chunks are written straight into a single sparse file with `WriteAt`, so there
is no merge pass and no doubled disk use.

## Honest limits

- **Only helps where the server honours byte ranges.** Some don't;
  `speed.cloudflare.com` answers `200` and sends everything.
- **Some servers lie about it.** `cdn.jsdelivr.net` answers `206` with a
  `Content-Range` total that is the *compressed* length, then sends the whole
  *uncompressed* body. Its weak ETag gives it away — `W/"883b92…"`, and
  `0x883b92` is exactly the true byte count. braid detects the over-long body,
  stops immediately rather than retrying (each retry would re-download the
  entire file), and falls back to a single stream instead of writing a corrupt
  truncation.
- **Nothing for Netflix, YouTube or other DRM'd adaptive streaming.** Those
  negotiate their own bitrate over connections we don't control.
- **No HTTPS proxying.** Splitting arbitrary browser traffic would mean
  installing a forged certificate authority on every device. braid won't.
- **Cellular costs money.** Every byte pulled over a metered link is billed to
  you. braid detects metered links via Darwin's own `constrained` interface
  flag and labels them, but does not yet enforce a ceiling.

## Layout

| Package | Responsibility |
|---|---|
| `internal/linkset` | which interfaces can carry an uplink; hot-plug, metered detection |
| `internal/dial` | transports pinned per interface *and* address family |
| `internal/probe` | size, splittability, cache validators |
| `internal/plan` | chunk arithmetic, completion bitmap, resume sidecar |
| `internal/sched` | shared-queue work stealing, retries, tail stealing |
| `internal/stream` | serving a file in order while its chunks are still arriving |
| `internal/server` | the LAN daemon: token auth, live dashboard, `/stream` |
| `internal/xfer` | one transfer, end to end |
| `internal/human` | formatting for people |
| `cmd/braid` | the CLI |
| `spike/` | throwaway measurement harness that proved the premise |

## Tests

```sh
go test ./... -race
```

172 assertions across 9 packages. The design keeps this honest: `linkset` and
the dialer sit behind interfaces, so the scheduler, planner, resume logic and
probe are all exercised with synthetic links and loopback servers — **no phone
required**. Tests cover a link dying mid-transfer, a server that ignores
ranges, a server that lies about them, resume across a process restart, a
refused resume after the file changed, and a chunk wedged forever on a slow
link.

## Roadmap

- [x] **Phase 0** — prove `IP_BOUND_IF` really pins traffic, with numbers
- [x] **Phase 1** — `braid links`, `braid get`
- [x] **Phase 2** — `braid serve`: a LAN endpoint so a phone, laptop or Apple TV
      gets the bonded speed with nothing installed, by streaming bytes in order
      as out-of-order chunks land
- [ ] **Phase 3** — enforce metered budgets, pick each link's faster address
      family from observed rate, menu-bar status
- [ ] **Phase 4** — a bonding tunnel on a VPS, the only way to accelerate
      browsing and adaptive streaming

## Prior art

[`anmolkapil/plexo`](https://github.com/anmolkapil/plexo) (MIT) — a macOS
Electron download manager built on the same core insight. Its framing of the
problem shaped this design. braid differs in being a single dependency-free Go
binary, in pinning with `*_BOUND_IF` per address family rather than a source
address alone, in writing one sparse file rather than merging part files, and in
adding tail stealing and metered-link awareness.

## License

MIT — see [LICENSE](LICENSE).
