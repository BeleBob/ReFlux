# OpenFlux

**English** | [Русский](README.ru.md)

Network stack research tool. IPv4 TCP/UDP tunnel with pluggable transports,
batched+zstd codec, and two exit-node backends (L3 raw forward / L4 gVisor proxy).

# Disclaimer

The author of OpenFlux **does not encourage** the use of this project to bypass
restrictions or violate the rules of any platform, and **is not responsible**
for the final scenarios of how users apply this tool in real life or on the
Internet. Any specific technical features of the application are nothing more
than an **architectural coincidence**, created **without any intent**.

The project is **entirely non-commercial**, contains **no paid features, hidden
subscriptions, or commercial benefit**.

The author **is not responsible** for forks, modifications, or derivative
versions of OpenFlux created by third parties. Any changes added to a fork are
the responsibility of its author.

The author **is not responsible** for:

- Any use of OpenFlux by third parties
- Consequences caused by the use of forks and modifications
- Damage resulting from derivative versions
- Violations committed using forks

The original code is provided **as is**, **without any warranties**.

## Clients

| Platform | Download | Notes |
|----------|----------|-------|
| **macOS**   | build from source | CLI + utun L3 client (`--inbound=tun`, default on macOS) |
| **Linux**   | build from source | CLI client (SOCKS5) / exit node (L3 or L4) |
| **Windows** | build from source | CLI client (SOCKS5) / exit node (`l4`, or `l3` via QEMU - see TODO) |
| **Android** | [OpenFluxAndroid releases](https://github.com/p1neappleXpress/OpenFluxAndroid) | Standalone APK |
| **iOS**     | [TestFlight beta](https://testflight.apple.com/join/BwnAcdus) | System-wide VPN via Network Extension |

> **iOS app** built by [@saharev1](https://github.com/saharev1) - full iOS client,
> TestFlight pipeline, system VPN support, DNS-over-TLS, and many stability fixes.
> HUGE thanks!
>
> **Android app** - [p1neappleXpress/OpenFluxAndroid](https://github.com/p1neappleXpress/OpenFluxAndroid).

## Architecture

Any client works with either exit backend. `--mode` is chosen on the **exit
node**, not on the client.

```
Client (any):  macOS (utun) / Linux / Windows / iOS (packet tunnel) / Android
                    |
                    v
               Transport (Yandex.Docs / Volga / MAX / Cups / Mail.ru)
                    |
                    v
               Exit node  -->  Internet
                 --mode l3   (raw SNAT/DNAT, Linux + root)
                 --mode l4   (gVisor proxy, any platform)
```

| Client (any)                            | Exit backend | Requires              |
|-----------------------------------------|--------------|-----------------------|
| macOS / Linux / Windows / iOS / Android | `--mode l3`  | exit on Linux + root  |
| macOS / Linux / Windows / iOS / Android | `--mode l4`  | nothing               |

In `l3`, the exit node terminates nothing: it forwards raw TCP and UDP packets
with SNAT/DNAT (conntrack + egress-IP filter). TCP remains end-to-end between
the client and the real server.

In `l4`, the exit node terminates TCP/UDP in a userspace gVisor stack, then
re-dials the real server. Works on any OS, no root.

The client terminates TCP locally (gVisor, utun, or NEPacketTunnelProvider),
then sends raw IP packets into the transport.

## Exit-node backends

The exit node has exactly **two** backends, selected with `--mode` on the
**exit node**. The client does not choose a backend - the same client works
against either.

| `--mode` | Backend | Forwarding | Requires | Platforms |
|----------|---------|-----------|----------|-----------|
| `l3` | Raw L3 | SNAT/DNAT on raw IPv4 via SOCK_RAW + conntrack. No userspace TCP stack. | root / CAP_NET_RAW | Linux only |
| `l4` (alias `proxy`) | gVisor proxy | Terminates TCP/UDP in a userspace gVisor stack, then dials the real server. | nothing | Linux, macOS, Windows |

- `proxy` is a deprecated alias for `l4`; both select the same backend.
  `l4` is the canonical name going forward.
- **l3 is faster** (single end-to-end TCP connection, no double termination)
  but Linux-only and needs root.
- **l4 works everywhere** without root, at the cost of terminating TCP twice
  (client -> gVisor on exit -> real server).
- On Linux with root, prefer `l3`. On Windows, the intended path is `l3`
  inside a lightweight QEMU VM (see TODO) - the WinDivert backend is not wired
  yet, and `l4` is the working fallback until QEMU is shipped. On non-root
  hosts, use `l4`.

### l3 and kernel RSTs

In `l3` mode the kernel sees return packets for connections it never opened
and emits RSTs, tearing the tunnel connections down. Drop them:

```
# Scoped (recommended): assign a dedicated egress IP, run with --local-ip, then:
sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -s <egress-ip> -j DROP

# Host-wide fallback (drops ALL outbound RST; makes closed ports look filtered):
sudo iptables -A OUTPUT -p tcp --tcp-flags RST RST -j DROP
```

Client-originated RSTs are forwarded normally. The rule above is only for
RSTs generated locally by the exit-node kernel.

## Highlights

- **Pluggable transports** - Yandex.Docs (WS), Yandex Volga (HTTP relay + WS),
  MAX/OneMe (WebRTC DataChannel), Cups.online (Centrifugo rooms),
  Mail.ru Docs (WS).
- **Batched + zstd codec** - coalesces many tunnel packets into a single
  transport message. Fewer channel messages, higher throughput. See
  `transport/batched.go` and `transport/framing.go`.
- **IPv4 UDP** - L4 forwarding and SOCKS5 `UDP ASSOCIATE` have local echo
  coverage. Linux raw L3 UDP remains experimental; see the limitations below.
- **Authenticated capability negotiation** - opt-in `--negotiate` inside
  encryption, with fresh session challenges, packet limits and replay checks.
  The old unauthenticated wire-v3 startup option is retired. Legacy mode is unchanged.
- **Two exit backends** - `l3` (raw SNAT/DNAT) and `l4` (gVisor proxy).
  See [Exit-node backends](#exit-node-backends).
- **macOS utun client** - `--inbound=tun` (default on macOS). Creates a utun
  interface, watches its own sockets to install bypass routes, then takes
  the default route. No SOCKS5, no gVisor on the client.
- **iOS packet tunnel** - NEPacketTunnelProvider, pure L3 forwarding.
- **Legacy codec** - `--codec=legacy` reverts to the old per-packet LZ4 codec
  (compatible with older clients).
- **Optional encryption** - `--encryption-key-file` wraps the transport in
  AES-256-GCM. Both peers must share the secret.
- **Benchmark modes** - `--role=bench-send --bench-bytes=N` / `--role=bench-sink`
  measure raw goodput through the transport without touching the host network.

## Requirements

1. **Go** - to build the desktop client / exit-node binary. See `go.mod` for
   the exact version.
2. **Android NDK r27+** - to build the Android client binary.
3. **Xcode 26.6+** - to build the iOS client binary.
4. **A Linux VPS / VDS** for the exit node. The `l3` backend requires root;
   `l4` works without.

## Structure

```
OpenFlux/
  main.go                          # CLI entry (client / exit / benches)
  bench.go                         # Benchmark helpers
  tun_darwin.go                    # macOS utun L3 client
  tun_watch.go                     # Socket watcher for bypass routes
  tun_other.go                     # Stubs for non-darwin platforms
  export_ios.go                    # cgo bridge for the iOS static library
  transport/
    transport.go                   # Transport interface
    batched.go                     # BatchedTransport (coalescing + zstd)
    framing.go                     # Wire framing for batched frames
    compressor.go                  # Legacy per-packet LZ4 codec
    encrypted.go                   # Optional AES-256-GCM wrapper
    yandex/                        # Yandex.Docs + Volga backends
    oneme/                         # MAX Messenger backend
    cupsonline/                    # Cups.online backend
    mailru/                        # Mail.ru Docs backend
  tunnel/
    tunnel.go                      # Client tunnel (gVisor + TunnelLinkEndpoint)
    endpoint.go                    # Virtual NIC (client)
    exit.go                        # NewExitNode dispatcher (l3 / l4)
    proxy_exit.go                  # L4 exit (gVisor + net.Dial)
    l3/
      l3.go                        # L3Exit: SNAT/DNAT, conntrack, egress filter
      backend.go                   # L3Backend interface
      backend_linux.go             # SOCK_RAW backend (Linux)
      backend_windows.go           # Stub (WinDivert not wired yet)
      backend_other.go             # Unsupported-platform stub
      conntrack.go                 # Conntrack table
      flow.go                      # Flow keys, SNAT/DNAT, checksums
    rawsocket_linux.go             # Legacy raw exit (kept for reference)
    rawsocket_{darwin,windows}.go  # Stubs
    windivert/                     # WinDivert backend (present, not wired to L3 yet)
  socks5/                          # SOCKS5 server (client fallback)
  network/                         # Checksums, packet parsing
  utils/                           # Logging
  ios-app/                         # SwiftUI iOS client (XcodeGen)
  build_ios.sh                     # Build iOS static library (liboflux.a)
  build_ios_app.sh                 # Build + archive + export iOS app IPA
  build_android.sh                 # Build Android client binary
  scripts/
    cleanup-utun.sh                # Remove leftover utun routes (macOS)
    build-flx-linux-img.sh         # Build minimal Alpine rootfs for QEMU
```

## Build

```
go mod tidy
go build -o openflux .
```

Cross-build for the exit node (Linux amd64), stripped:

```
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath -o openflux-linux .
```

## Usage

### Exit node - L3 (Linux, root)

```
sudo ./openflux --role=exit --mode=l3 \
    --transport=yandex \
    --url="YOUR_YANDEX_DOC_URL"
```

Requires root / CAP_NET_RAW. Install the iptables rule (see
[l3 and kernel RSTs](#l3-and-kernel-rsts)).

### Exit node - L4 (any OS, no root)

```
./openflux --role=exit --mode=l4 \
    --transport=yandex \
    --url="YOUR_YANDEX_DOC_URL"
```

Fallback for platforms where `l3` is unavailable (Windows without WinDivert,
macOS, non-root Linux). Slower than `l3` (double TCP termination).

### Client - macOS utun (default on macOS)

```
sudo ./openflux --role=client --inbound=tun \
    --transport=yandex \
    --url="YOUR_YANDEX_DOC_URL"
```

Creates a utun interface, installs bypass routes for the transport, waits for
the transport to connect, then takes the default route. No SOCKS5.
Requires sudo. All traffic except the transport goes through the tunnel.

### Client - SOCKS5 (all platforms, fallback)

```
./openflux --role=client --inbound=socks5 \
    --transport=yandex \
    --url="YOUR_YANDEX_DOC_URL" \
    --socks5=:1080
```

Point your browser / app at `127.0.0.1:1080` as a SOCKS5 proxy. This is the
default inbound on non-macOS platforms. UDP-capable applications may use the
SOCKS5 `UDP ASSOCIATE` command.

### UDP limitations

- UDP is IPv4-only for now.
- L3 reassembles IPv4 fragments with a 30-second fixed lifetime, 64 incomplete
  datagrams, 128 fragments per datagram and a 4 MiB byte budget per direction.
  Overlaps and malformed fragments are discarded; expiry is swept on input.
- L3 relays checksum-validated ICMP errors only for live TCP/UDP NAT flows,
  restoring the quoted client address/port and checksums. Redirects and echo
  traffic are not relayed. Egress EMSGSIZE produces ICMP fragmentation-needed
  with the kernel route MTU; non-DF packets can instead be fragmented. Outgoing
  fragmentation of IPv4 headers containing options is not supported.
- This is ICMP-based PMTU feedback, not active DPLPMTUD probing. Networks that
  filter ICMP can still black-hole large DF packets; real-network tests remain
  necessary. The negotiated packet ceiling is distinct from the Internet MTU.
- Linux raw L3 UDP reserves a kernel-selected source port per remote endpoint
  using a real UDP socket and restores the client's port on return. This avoids
  taking ports owned by host applications and is intended to prevent kernel
  ICMP port-unreachable without firewall changes. There are at most 256 mappings;
  idle expiry is 2 minutes (15 seconds for DNS). Source-port preservation and
  endpoint-independent NAT/hole-punching are not provided.
- The isolated Linux raw-socket/ICMP test passes in GitHub Actions. It covers
  loopback inside a disposable network namespace, including host-port conflicts
  and false ICMP port-unreachable responses; it is not an Internet/PMTU canary.
  TCP's existing raw-port ownership and RST-suppression requirements are unchanged.
- iOS keeps the old TCP fallback for non-DNS UDP unless the app explicitly
  calls `OpenFluxTunSetUDPEnabled(1)` for a known UDP-capable exit. Reset it to
  `0` when switching to an older exit. Physical-device QUIC is not validated.
- Most document/WebSocket transports are reliable and ordered. UDP works over
  them, but packet loss in the carrier can still cause head-of-line blocking;
  this is not equivalent to a native datagram transport.

### Codec selection

By default the transport uses the batched + zstd codec
(`transport/batched.go` + `transport/framing.go`). For the old per-packet
LZ4 codec, pass `--codec=legacy`:

```
./openflux --role=client --codec=legacy ...
```

**Important:** batched and legacy LZ4 codecs remain incompatible. Default
batched mode remains v2. The old `OPENFLUX_EXPERIMENTAL_WIRE_V3=1` prototype
now fails startup rather than accepting unauthenticated capability messages.

### Authenticated capability negotiation (opt-in CLI)

Add these options on **both** updated peers, using the same secret and codec:

```
--codec=batched --encryption-key-file=/path/to/secret.txt --negotiate
```

The handshake runs inside AES-GCM and confirms fresh random challenges, peer
roles, IPv4/TCP/UDP support, ICMP-error support and maximum IPv4 packet size.
L4 does not advertise raw ICMP forwarding. Only the intersection of capabilities
is enabled. Data carries both session IDs and a sequence number; a 64-packet
sliding replay window tolerates bounded reordering. The old batch-v2 envelope
and encryption key derivation are unchanged; this is not forward secrecy or a
replacement for a future key-exchange/rekey design.

Negotiated mode never falls back to unencrypted or legacy peers. Startup fails
after 20 seconds if negotiation cannot complete (wrong key, incompatible codec,
missing option, or unavailable peer). `--max-packet-size=1280..65000` caps the
complete IPv4 packet; the default is 65000, leaving room for authenticated
envelopes. The agreed limit is used by the gVisor link; the macOS TUN remains
1280. Raw-exit replies exceeding the agreed limit are fragmented without DF,
or produce ICMP feedback to the Internet sender with DF.

Session identity and capabilities stay fixed for the process lifetime. Carrier
reconnects retain them; after either process restarts, restart the other peer
as well. Automatic secure session replacement is not implemented. Existing
iOS builds have no negotiation setting and must use an exit without `--negotiate`.
Their UDP switch remains manual. No claim of device-level QUIC validation is made.

### Encryption (optional)

```
./openflux ... --encryption-key-file=/path/to/secret.txt
```

Both peers must use the same secret file. AES-256-GCM, directional keys.
Unset means unencrypted, unchanged behavior.

### Benchmarks

Measure raw goodput through the transport, without touching the host network:

```
# Sender: push 100 MB
./openflux --role=bench-send --bench-bytes=100 --transport=yandex --url="..."

# Receiver: measure goodput
./openflux --role=bench-sink --transport=yandex --url="..."
```

### Other transports

```
# Yandex Volga (HTTP relay + WS)
./openflux --role=exit --mode=l3 --transport=vyandex --url="..." --debug

# MAX / OneMe (WebRTC DataChannel)
./openflux --role=exit --mode=l3 --transport=oneme \
    --maxToken="..." --maxUid="..." --debug

# Cups.online (Centrifugo rooms)
./openflux --role=exit --mode=l3 --transport=cupsonline --debug
# prints a base64 room list; pass it to the client via --url

# Mail.ru Docs (WS)
./openflux --role=exit --mode=l3 --transport=mailru \
    --url="YOUR_MAILRU_PUBLIC_LINK" --debug
# accepts either a bare weblink (AbCdEfGh1/IjKlMnOp2) or a full URL
# (https://cloud.mail.ru/public/AbCdEfGh1/IjKlMnOp2)
```

## Flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--role` | `-r` | `client` | `client` \| `exit` \| `bench-send` \| `bench-sink` |
| `--inbound` | `-i` | (platform) | `tun` (macOS) \| `socks5` |
| `--transport` | `-t` | `yandex` | `yandex` \| `vyandex` \| `oneme` \| `cupsonline` \| `mailru` |
| `--mode` | `-m` | `l3` | Exit-node mode: `l3` \| `l4` |
| `--codec` | `-c` | `batched` | `batched` \| `legacy` |
| `--url` | `-u` | `http://#` | Document URL |
| `--socks5` | `-s` | `:1080` | SOCKS5 listen address |
| `--local-ip` | `-l` | (auto) | Egress IP for l3 SNAT / RST filter |
| `--debug` | `-d` | `false` | Verbose per-packet logging |
| `--encryption-key-file` | | | AES-256-GCM shared secret file |
| `--maxToken` | | | MAX auth token (`--transport=oneme`) |
| `--maxUid` | | | MAX user id (`--transport=oneme`) |
| `--bench-bytes` | | `0` | MB to push (`--role=bench-send`) |
| `--bench-compressible` | | `false` | Use compressible payload (bench) |

Deprecated (kept for one release, mapped automatically to the new flags):
`--client`, `--exit-node`, `--tun`, `--socks5-mode`, `--legacy`,
`--bench-send`, `--bench-sink`.

## Implementing custom transports

Implement the `Transport` interface from `transport/transport.go` and register
your transport in the `main.go` switch block (see `transport/mailru/` for a
complete example). The batched codec (`BatchedTransport`) wraps any transport,
so a new backend gets batching for free.

## TODO

- **L3 exit on Windows and macOS.** The L3 exit currently works on Linux
  (SOCK_RAW) only; Windows and macOS use `--mode=l4`. The `tunnel/windivert/`
  package (Windows) exists but is not wired to the L3 forwarder yet. A native
  macOS L3 exit is not implemented.
- **Run the exit node (QEMU).**

## License

GNU General Public License v3.0 or later. See LICENSE for the full text.

Third-party licenses are listed in [NOTICE](NOTICE).
