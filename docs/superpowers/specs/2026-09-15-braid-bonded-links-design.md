# braid — bonded links for downloads and viewing

**Date:** 2026-09-15
**Status:** approved design, not yet implemented

## Goal

Use Wi-Fi and a tethered phone's cellular connection *at the same time* so that a
single large transfer runs at roughly the sum of both links, and so that the
speed-up is available to every device in the house — not only the Mac running the
software.

## Non-goals

- Accelerating Netflix, YouTube, Prime Video or any other DRM'd adaptive-bitrate
  service. Those negotiate their own bitrate over connections we do not control.
  Deferred to project B (see Phase 4).
- Accelerating arbitrary HTTPS browsing. That needs a forged CA installed on
  every client, which this project explicitly refuses to do.
- Aggregating uploads. Out of scope for v1.
- Windows or Linux clients for the *host* side. The host is macOS-only because
  link discovery and socket pinning use Darwin-specific APIs. Any OS can be a
  *client*.

## Measured environment (2026-09-15, this Mac)

These are observations, not assumptions. Each one shaped a decision below.

| Observation | How it was measured | Design consequence |
|---|---|---|
| Wi-Fi `en0` = 192.168.1.42, 802.11ac, 5 GHz, 80 MHz, 650 Mbps link rate | `networksetup`, `system_profiler SPAirPortDataType` | Local radio is not the bottleneck |
| Real Wi-Fi throughput **≈100 Mbps sustained** (95.4 MB over 8 s, pinned, 4 connections) | `spike measure` — see the Phase 0 log. An earlier 1.47 s single-shot read 108–190 Mbps; that was burst, not sustained | Cellular has to be genuinely fast to help. Expect +30–80%, not +100% |
| `en0` carries **both** IPv4 and IPv6, and **IPv6 is preferred** — egress went out over `2001:db8:ff::1` | `ifconfig en0`, `curl -w %{remote_ip}` | Socket pinning **must be per address family**. A v4-only implementation would silently fall through to Wi-Fi and still appear to work |
| The ISP is a large mobile-and-fixed carrier with a regional egress | Cloudflare `cf-meta-*` headers | Indian cellular is frequently IPv6-first / 464XLAT, reinforcing the above |
| `iPhone USB` is a **configured service on `en5`**, but `en5` does not exist while the phone is unplugged | `networksetup -listnetworkserviceorder`, `ifconfig en5` | Links must be hot-plugged: discovered, weighted and dropped at runtime |
| Only one default route today (`default 192.168.1.504 en0`) | `netstat -rn` | With a second link up, macOS adds a scoped route; pinning is what reaches it |
| `IP_BOUND_IF = 25`, `IPV6_BOUND_IF = 125` | grepped from the Xcode MacOSX.sdk headers | Exact constants for the pinning syscall |
| Go was not installed; Homebrew 6.0.22 present | `go version`, `brew --version` | `brew install go` is a prerequisite |

Not measured: cellular throughput, and behaviour on files larger than ~20 MB —
the agent sandbox killed a 300 MB transfer. Both are the first spike's job.

## Why the phone must tether over USB

The Mac has one Wi-Fi radio and it is already carrying the broadband uplink. If
the phone shares over Wi-Fi hotspot, `en0` can only join one of the two networks,
so there is nothing to bond. USB tethering gives a *separate* interface (`en5`),
which is the entire premise. Ethernet-for-broadband + Wi-Fi-for-hotspot is an
equally valid topology and the code should not care which it gets.

## Architecture

One statically linked Go binary, `braid`, with subcommands:

- `braid links` — print discovered links and their measured rates. Diagnostics.
- `braid get <url>` — bonded download to disk. The Mac-local case.
- `braid serve` — long-running daemon: HTTP API, web UI, and stream-through
  endpoint on the LAN. The everything-else case.
- `braid tunnel` — Phase 4, project B. Same binary, run on a VPS.

Go was chosen over forking the (MIT, Node/Electron) prior art `plexo` so that the
same binary can later serve as the project-B tunnel endpoint on a Linux VPS, and
to get real goroutine parallelism and a dependency-free single-file install.

### How each device gets the benefit

```
                         ┌──────────── braid serve (Mac) ─────────────┐
 iPhone  ──USB──→        │  GET /stream?url=…                         │      ┌─ en0  Wi-Fi  (v4+v6)
 Laptop  ──LAN──→  ──────┤  in-order, Range-capable, resumable output ├──────┤
 Apple TV ─LAN──→        │  ← fed by out-of-order bonded chunk fetch  │      └─ en5  iPhone USB (v6-first)
 Mac apps ─local─→       └────────────────────────────────────────────┘
```

The daemon fetches chunks **out of order across all links** for throughput, but
serves each client **in order**, blocking only when the next needed byte has not
landed. Clients therefore need no software: point VLC, Infuse, Safari, or `curl`
at `http://<mac>.local:8080/stream?url=…`.

This deliberately replaces the transparent-proxy idea. A proxy cannot split an
HTTPS body without terminating TLS, which would mean installing a forged
certificate authority on the phone and the TV. Stream-through gets the same
benefit for the cases that matter, with no client trust changes. A plain
`http://` proxy mode may be added later as a convenience; it is not the
primary path.

### The donor phone benefits too

While USB-tethered, the iPhone is on the same subnet as the Mac (typically
`172.20.10.1` gateway, Mac at `172.20.10.x`) and can therefore reach the Mac's
port. Traffic path: phone app → USB → Mac daemon → (Wi-Fi ∥ back out over the
phone's cellular) → internet.

Two unverified assumptions, both on the first spike's checklist:
1. iOS Personal Hotspot does not isolate clients from the host device.
2. USB carrying the cellular half twice is not the new bottleneck. iPhone USB
   tethering is USB 2.0 class, ~250 Mbps realistic, so this only bites if
   cellular is very fast.

If (1) turns out false, the phone falls back to reaching the daemon over the
house Wi-Fi instead, and only the *other* devices benefit from bonding. The
design does not collapse; one entry in the topology table changes.

## Packages

Each package is independently testable and depends only on interfaces below it.

### `linkset` — what can we send on?

Discovers usable uplinks and keeps them current.

```go
type Link struct {
    Iface    string        // "en0"
    Friendly string        // "Wi-Fi", "iPhone USB"
    Index    int           // ifindex, for the pinning syscall
    V4, V6   netip.Addr    // zero value = family unavailable on this link
    Metered  bool
    Rate     float64       // EWMA bytes/sec, observed
    Alive    bool
}

type Source interface {
    Links() []Link
    Watch(context.Context) <-chan []Link   // fires on hot-plug
}
```

Implementation reads `net.Interfaces()` for addresses and ifindex, and
`networksetup -listnetworkserviceorder` once for friendly names and service
order. Excludes loopback, `utun*`, `bridge*`, link-local-only, and any interface
with no default route. Polls every 2 s — there is no cheap Darwin notification
for this and 2 s is imperceptible against a multi-minute transfer.

`Metered` defaults true for any interface whose friendly name matches a phone
tether, and is overridable in config. It gates the data budget below.

### `dial` — pin a socket to one link

The single most important package, and the place plexo is weakest: it sets only a
source address. On macOS the reliable mechanism is the `*_BOUND_IF` socket
option, which forces the kernel to use that interface's scoped route regardless
of the default route.

```go
type Family int   // Fam4 | Fam6

func Transport(l Link, fam Family) (*http.Transport, error)   // error if l has no address in fam
```

Sets both belt and braces — `net.Dialer.LocalAddr` to the link's address in the
requested family, **and** in `Dialer.Control`:

- `tcp4` → `setsockopt(fd, IPPROTO_IP,   25,  ifindex)`   // IP_BOUND_IF
- `tcp6` → `setsockopt(fd, IPPROTO_IPV6, 125, ifindex)`   // IPV6_BOUND_IF

Per-family is not optional: a link may have v6 only. The chunk scheduler asks for
a transport in a family the link actually has, and the probe resolves both A and
AAAA once so either can be used.

Each link gets its own `*http.Transport` so connection pools never leak across
links — a pooled connection belongs to the interface it was dialed on.

### `probe` — is this file splittable, and how big?

Sends a one-byte ranged `GET` (`Range: bytes=0-0`) rather than a `HEAD`, because
CDNs misreport `HEAD` and a `206` response is proof rather than a promise.
Follows up to 5 redirects. Returns size (from `Content-Range`), `ETag`,
`Last-Modified`, filename (from `Content-Disposition` then the URL path), and
whether ranges are honoured. A `200` answer means the server ignored the header:
fall back to a single-stream download on the fastest link, and say so in the UI
rather than pretending to bond.

### `plan` — chunking and resume state

Fixed-size chunks (default 4 MB, configurable) over the byte range. Chunks are
the unit of leasing, retry and accounting.

Output goes to **one sparse file** written with `WriteAt` at each chunk's offset —
not to `part-N` files needing a merge pass. This halves disk use and removes an
entire failure mode. Resume state is a sidecar `<name>.braid` file holding the
URL, size, validators and a completion bitmap; it is fsynced on chunk completion.
On resume the validators are re-probed and a mismatch **refuses** to resume
rather than stitching two different files together.

### `sched` — work stealing, tail stealing, streaming bias

A shared queue of pending chunks. Each link runs *w* workers, where *w* adapts
from the link's observed EWMA rate (bounded 1–8). A worker leases a chunk,
fetches it on its pinned transport, writes it, marks the bitmap, updates the rate,
and leases again. Fast links therefore consume more chunks without any static
share being declared — the failure mode of splitting a file 50/50 and waiting for
cellular is structurally impossible.

Two additions beyond the prior art:

**Tail stealing.** When the queue empties but chunks are still in flight, an idle
fast worker re-requests the *unfetched remainder* of the slowest in-flight chunk
as a narrower range. Both writers target the same offsets in the sparse file, so
correctness comes from the bitmap, not from the write order: a worker takes the
bitmap lock, and if its sub-range is already marked complete it discards its
buffer without writing. Byte-identical content makes an interleaved write
harmless anyway; the lock exists to avoid wasted I/O. Without tail stealing, one
unlucky cellular chunk adds its full duration to the end of every transfer.

**Streaming bias.** When a client is reading `/stream`, the queue is ordered by
distance ahead of the client's read cursor instead of by file offset, with a
configurable readahead window. Bytes the viewer needs next get fetched next,
while bytes far ahead still soak up spare capacity.

### `budget` — cellular is metered and that is not a footnote

Per-link, per-period byte ceilings, persisted across restarts. Defaults: any
link marked `Metered` gets **5 GB per calendar month and 1 GB per day**,
whichever binds first; unmetered links are uncapped. Both numbers are editable in
the UI and in config; they are starting values chosen to be useful-but-safe, not
a claim about your plan. A link at its ceiling is weighted to zero rather than
erroring — transfers keep working, just on Wi-Fi alone. The UI shows consumption
per link. This exists because the entire premise of the project is spending
mobile data, and software that spends money silently is not acceptable.

### `server` — HTTP surface

`net/http`, no framework.

| Route | Purpose |
|---|---|
| `GET /` | Web UI: paste a URL, watch per-link throughput, manage budgets |
| `GET /links` | JSON link state, for `braid links` and the UI |
| `POST /jobs` | Start a download to the Mac's disk |
| `GET /jobs/:id/events` | SSE progress: per-link bytes, rate, ETA, chunk map |
| `GET /stream?url=…` | The device-facing path. In-order bonded body, honours the client's own `Range` header so players can seek, resumable |

Binds to the LAN by default, since a localhost-only daemon cannot serve the
phone. That is a deliberate exposure decision and so it ships with a shared
token required on every route, generated on first run and shown in the UI and by
`braid links`. Open to the LAN with no auth is not acceptable even at home.

The token is accepted either as an `Authorization: Bearer` header or as a `?t=`
query parameter, because the clients that matter most — VLC, Infuse, an Apple TV
— cannot set request headers. The `/stream` URL is therefore self-contained and
pasteable, which is the whole point of that endpoint.

### UI requirements

The web UI is a **first-class requirement, not a debug view**. It must be
modern, clean, intuitively obvious without explanation, and animated with
purpose. Specifically:

- **Legible at a glance.** The one question the UI answers is "is bonding
  working right now, and how much is each link contributing?" That must be
  readable in under a second, from across a room, without interpreting a number.
- **Animation that carries information**, never decoration: live per-link
  throughput, chunks landing on the progress map coloured by the link that
  fetched them, a link going dead or hitting its budget. Motion should show the
  two streams merging, because that *is* the product. Every animation must
  respect `prefers-reduced-motion`.
- **Intuitive on a phone.** The phone is a primary client, not an afterthought,
  so the layout is responsive and touch-first, and `/stream` URLs are one tap to
  copy or share.
- **Light and dark**, following the system, both deliberately designed.
- **No framework build step.** Server-rendered HTML plus a single vanilla
  JS/CSS bundle, driven by the SSE stream that already exists. This preserves
  the single-binary promise (assets embedded with `go:embed`) and keeps the UI
  from becoming a second project with its own toolchain.

Design directions are to be presented and approved **before** the UI is built,
using the `frontend-design` skill. Aesthetic direction is not to be improvised
during implementation.

## Data flow

**`braid get <url>`** — probe → plan → open sparse file → start `sched` over all
live links → chunk workers fetch/write/mark → tail steal at the end → fsync,
verify bitmap complete, drop the sidecar, rename into place.

**`GET /stream?url=…`** — probe → plan → `sched` with streaming bias → response
writer walks the bitmap from the client's requested offset, writing each
contiguous run as it becomes available and parking on a condition variable when
it reaches a hole. Client disconnect cancels the job's context, which cancels
in-flight chunk requests.

## Failure handling

| Failure | Behaviour |
|---|---|
| Chunk request fails | Lease returns to the queue; exponential backoff 1 s → 15 s, 5 attempts, then the job fails loudly |
| Connection open but silent > 20 s | Treated as failed, connection dropped, chunk re-queued, link rate penalised |
| Interface disappears (phone unplugged) | `linkset` marks it dead; its leases are re-queued on survivors; the job continues |
| All links die | Job pauses rather than fails; resumes when any link returns |
| Server ignores `Range` (`200`) | Single-stream fallback on the fastest link, surfaced in the UI |
| `ETag`/`Last-Modified` changed since the sidecar was written | Refuse to resume; offer a clean restart |
| Insufficient disk space for the full size | Refuse before writing any bytes |
| Redirect mid-chunk | Followed, up to 5 hops |
| Metered link at its budget ceiling | Weighted to zero, not an error |
| Client disconnects from `/stream` | Context cancelled, in-flight chunks aborted, partial output discarded |

## Testing

The central testing decision: **`linkset.Source` and `dial.Transport` are
interfaces, so the scheduler is tested with fake links and no hardware.** Without
this, nothing is testable unless a phone is plugged in, which would make the work
undebuggable.

- **Unit** — chunk planning maths and boundary conditions; bitmap encode/decode
  and resume; budget accounting across period rollover; `Content-Range` and
  `Content-Disposition` parsing, including malformed headers.
- **Scheduler, with fakes** — a test HTTP server with per-connection throttling
  plus synthetic links. Assertions, each written to fail before the feature
  exists: aggregate throughput approximates the sum of link rates; a link that
  is 10× slower does not extend total time proportionally (proves work stealing);
  a link that dies mid-transfer loses no bytes; tail stealing bounds the endgame;
  streaming bias delivers byte 0 before a far-offset chunk.
- **Integration** — real loopback HTTP server, real sparse-file writes, real
  resume across a process restart.
- **On-device spike** — the only part needing the phone, and it runs first.

## Phases

**Phase 0 — spike (needs the iPhone on USB, hotspot on).** Prove, with numbers,
that two links pull independently on this machine: `braid links` sees `en5`;
`IP_BOUND_IF` actually pins (verified by per-interface byte counters moving, not
by trusting the socket option); simultaneous per-link `curl` totals more than
either alone; whether cellular is v6-only; whether the phone can reach the Mac's
port. Output is a measurement, not code we keep. **If pinning does not hold, the
project stops here and we reconsider** — everything else rests on it.

**Phase 1 — `braid get` + `braid links`.** Bonded download on the Mac. The whole
engine except the HTTP surface.

**Phase 2 — `braid serve`.** API, web UI, and `/stream`. This is the phase that
delivers the phone and the other devices.

**Phase 3 — polish.** Budgets in the UI, per-link naming and colours,
menu-bar status, launchd service.

**Phase 4 — project B, separate spec.** Bonded tunnel on a VPS for DRM streaming
and general browsing. Its real costs — a monthly server bill, an Apple Developer
account for an iOS packet-tunnel extension, and a total throughput ceiling set by
the VPS — deserve their own decision rather than being smuggled in here.

## Risks

1. **`IP_BOUND_IF` might not deliver independent throughput** in practice. Retired
   by Phase 0, before anything is built on it.
2. **USB 2.0 tethering (~250 Mbps) caps the cellular half**, and doubly so for
   traffic to and from the phone itself. Measured in Phase 0.
3. **Cellular may be IPv6-only** while some origins are IPv4-only, making those
   origins unreachable on that link. Mitigation: per-link family awareness is
   already in `dial`, and a link that cannot reach an origin is weighted to zero
   for that job rather than failing it.
4. **Origins that do not honour ranges** get no benefit at all. Detected by
   `probe` and reported honestly instead of faked.
5. **Cellular data cost.** Mitigated by `budget`, defaulting to capped.
6. **LAN exposure of the daemon.** Mitigated by a required token.

## Prior art

[`anmolkapil/plexo`](https://github.com/anmolkapil/plexo) (MIT) — a macOS
Electron download manager built on the same core insight, per-interface socket
binding plus HTTP range requests. Its README's framing of the three primitives,
and its handling of probing, work stealing, stall detection and validator-checked
resume, directly informed this design. braid differs in being a single Go binary
rather than an Electron app, in serving other devices over the LAN, in pinning
with `*_BOUND_IF` per address family rather than a source address alone, in
writing one sparse file rather than merging part files, and in adding tail
stealing, streaming bias and metered-link budgets.

## Phase 0 log

**2026-09-15, Wi-Fi only (phone not yet attached).** Spike harness built and
validated; `spike links` resolves `en0` as "Wi-Fi", index 11, with both an IPv4
and an IPv6 global address.

| Test | Result |
|---|---|
| Wi-Fi sustained, IPv4, pinned | **100.0 Mbps** (95.4 MB in 8 s) |
| Wi-Fi sustained, IPv6, pinned | **95.6 Mbps** (91.2 MB in 8 s) |
| Kernel counter cross-check | 101.0 MB seen on `en0` vs 95.4 MB at the app — overhead-sized gap, counters agree |
| Sandbox limit | Worked around: repeated 20 MB requests sustain ~95 MB total where one 300 MB request failed |
| `speed.cloudflare.com/__down` | Returns **200, no ranges** — usable as a throughput payload only |
| `dl.google.com/go/go1.27.1.darwin-arm64.tar.gz` | **206, 64.9 MB** — the real range-capable test file for Phase 1 |
| `cdn.jsdelivr.net` | 206, ranges honoured |

Baseline revised down: the earlier 108–190 Mbps figure was a single-shot burst.
Sustained Wi-Fi is ~100 Mbps, which is the number cellular has to be measured
against.

**Still open, and blocked on the iPhone being attached over USB:** whether
`IP_BOUND_IF` genuinely pins across *two* links. With a single uplink the check
passes trivially, since there is nowhere else for traffic to go, so it proves
the harness works and nothing about the premise.

**2026-09-15, cellular measured (Wi-Fi radio joined to the iPhone's 5G hotspot,
`en0` = 172.20.10.3).**

| Link | IPv4 | IPv6 |
|---|---|---|
| 5G cellular | **112.8 Mbps** (107.6 MB / 8 s) | 70.3 Mbps |
| Home Wi-Fi | 100.0 Mbps | 95.6 Mbps |

Consequences:

- **The two links are near-equal, with cellular slightly ahead.** Projected
  aggregate is ~210 Mbps, roughly double — the ideal bonding case, and much
  better than the +30–80% predicted when cellular was assumed to be the weak
  link. This is a projection from two independent measurements, not yet an
  observed aggregate.
- **Cellular is dual-stack**, holding a global `2001:db8:2:…` address.
  Risk #3 (cellular being IPv6-only and unable to reach v4-only origins) is
  retired.
- **Family choice matters per link.** IPv6 costs 37% on cellular (70.3 vs 112.8)
  but is near-parity on Wi-Fi (95.6 vs 100.0). `linkset` must therefore track
  an EWMA rate *per link per family*, and `sched` must pick each link's faster
  family independently rather than choosing one family globally.
- Range requests behave identically over cellular: 206 from both
  `dl.google.com` and `cdn.jsdelivr.net`.

**USB tethering is not required after all.** At 113 Mbps, cellular sits well
under the ~250 Mbps USB 2.0 ceiling, so the earlier worry that USB would
throttle 5G does not apply. Either topology works:

| Topology | Broadband | Cellular | Blocker |
|---|---|---|---|
| A | Wi-Fi `en0` ~100 | iPhone USB ~113 | USB link is intermittent — `en5`/`en14` appeared and vanished repeatedly, DHCP never answered |
| B | USB 10/100 LAN `en12` → router, ~94 | Wi-Fi `en0` → hotspot, 113 | Needs the adapter plugged in and an Ethernet run to the router |

Topology B is currently the shorter path, since the Wi-Fi half is already
proven working and the USB tether half is not.

**Still open:** the premise itself. Every measurement so far has been taken with
exactly one uplink, where pinning passes trivially because there is nowhere else
for traffic to go. `IP_BOUND_IF` has still never been tested against two
simultaneous links, which is the only thing Phase 0 exists to prove.

### Phase 0 verdict: PASSED

**2026-09-15, two live uplinks at last** — Wi-Fi `en0` on the home line
(192.168.1.42) and the iPhone tethered over USB on `en5` (172.20.10.2), after a
third cable finally held. The first two produced interfaces that appeared and
vanished with DHCP never answering; a worn data cable, not a configuration
fault.

| Measurement | Result |
|---|---|
| Wi-Fi alone, IPv4 / IPv6 | 84.6 / **94.1** Mbps |
| iPhone USB alone, IPv4 | **107.3** Mbps (no IPv6 offered at all) |
| **Both concurrently** | Wi-Fi 92.5 + cellular 82.1 = **174.6 Mbps** |
| Best single link | 107.3 Mbps → **gain +63%** |
| Theoretical ceiling (sum of each link's best family) | 201.4 Mbps → **87% captured** |
| Kernel counters during the concurrent run | **142.2 MB on `en0`, 125.8 MB on `en5`** |

**`IP_BOUND_IF` works.** That last row is the proof the entire project rests on:
substantial traffic moved on both interfaces at once while the routing table's
default route pointed at only one of them. Verified at the interface, not
inferred from a `setsockopt` return value. Risk #1 is retired and the premise is
sound.

Further findings, each with a design consequence:

- **The links are not perfectly independent.** Cellular fell from 107.3 to
  82.1 Mbps under concurrency (−24%) while Wi-Fi barely moved (94.1 → 92.5).
  Cause unconfirmed — USB 2.0 contention or ordinary cellular variance. The
  scheduler must therefore weight links on *continuously observed* throughput
  during the transfer, never on a rate measured in isolation beforehand.
- **A link's faster family changes between runs.** Wi-Fi favoured IPv4 in the
  first run (100.0 vs 95.6) and IPv6 in this one (94.1 vs 84.6). Per-link
  per-family EWMA is load-bearing, not a refinement.
- **A link's available address families change over time.** The USB tether had
  no IPv6 at all during the spike, and a global `2001:db8:2:…` appeared on
  the same interface minutes later, once SLAAC completed. (An earlier revision of
  this document claimed USB tethering never offers IPv6. That was wrong, and the
  corrected fact is the more demanding one.) So `dial` must error on a missing
  family rather than assume dual-stack, and `linkset` must re-read addresses
  continuously rather than resolving families once at startup.
- **USB 2.0 is not the bottleneck at these speeds.** USB tether measured
  107.3 Mbps against 112.8 for the same phone over Wi-Fi hotspot — close enough
  that the earlier worry about USB throttling 5G is unfounded below ~250 Mbps.
- **macOS link names are not presentable.** The OS named this service
  `iPhone USB USB`, having created a duplicate when the phone re-enumerated.
  This is why user-facing rename/recolour is a real feature, not polish.
- **Interface churn is real.** Across this session `en5`, `en6` and `en14` each
  appeared and vanished, and macOS spawned duplicate services. The hot-plug
  requirement in `linkset` is fully justified.

### Phase 1 verdict: DONE

`braid links` and `braid get` work. 51 tests across 7 packages, green under
`-race` and repeated runs.

**Verified against real hardware**, two live uplinks (Wi-Fi `en0` + iPhone USB
`en5`):

| | Result |
|---|---|
| 64.9 MB (`dl.google.com` Go tarball) | **2 s at 183.4 Mbps** |
| Split | 52.9% `en0` / 47.1% `en5` — near-even, as work stealing should give for near-equal links |
| Versus best single link (~107 Mbps) | **+71%** |
| Integrity | **SHA256 matches Google's published hash exactly** (`ee215d57…87d12`, 68,100,347 bytes) |
| Interrupt and resume | SIGINT left a 233-byte sidecar at 1/65 chunks; re-run fetched only the remaining 63.9 MB; checksum still matched; sidecar cleaned up |

That checksum is the claim worth keeping: 17 chunks pulled out of order across
two different network interfaces reassemble byte-identically.

Two things found only by running it for real:

**A CDN that lies about ranges.** `cdn.jsdelivr.net` answers `206` with
`Content-Range: bytes 0-1494133/1494134` and then sends 8,928,146 bytes — the
whole uncompressed file. Its weak ETag gives the game away: `W/"883b92…"`, and
`0x883b92` is exactly 8,928,146. The `Content-Range` total is the *compressed*
length while the body is the *decoded* representation, which RFC 9110 forbids.
The chunk-length guard caught it and refused to write a corrupt 1.4 MB
truncation. Handling added: an over-long body raises `ErrRangeIgnored` wrapping
`sched.ErrFatal`, the scheduler stops instead of retrying, and the transfer
falls back to a single stream sized to the bytes actually received. Retrying
would have cost a full 8.9 MB per attempt, much of it metered.

**Chunk size matters more than expected.** The same file at `-chunk 1048576`
managed 82 Mbps against 183 Mbps at the 4 MiB default — 65 requests instead of
17. One uncontrolled run against a variable cellular link, so suggestive rather
than measured, but it supports the default.

Deferred from this phase, and deliberately so:

- **Per-family rate selection.** Each link currently uses its first available
  family, IPv4 preferred. The spike showed the faster family flips between
  runs, so choosing by observed rate is worth doing — but it needs live
  measurement, which belongs with the same machinery that will drive the UI's
  live throughput display.
- **Adaptive worker counts.** Fixed at 4 per link. The shared queue already
  handles unequal link speeds, so this is a refinement.
- **The `budget` package.** Metered links are detected (via Darwin's
  `constrained` flag) and reported, but nothing enforces a ceiling yet. Phase 1
  testing spent roughly 200 MB of real mobile data, which is precisely the
  argument for building it.
