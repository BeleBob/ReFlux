# Local review — 2026-09-17

Scope: local `codex/wire-v3-udp`, starting with `aeeac32`, `aa00cb3`, `09ab7d6`
on base `9148c63`. No GitHub PR was published or modified.

## Verdict

Keep the combined change **Draft**. The initial claim that all three priorities
were complete was too broad. CI exists and UDP has local coverage, but secure
negotiation and production Linux raw L3 UDP remain incomplete. Tests cannot
prove the absence of bugs.

## Corrected issues

- UDP payload bits interpreted as TCP FIN/RST; empty input could panic.
- Invalid UDP lengths/TCP data offsets accepted; UDP checksums included IP padding.
- Expired conntrack entries accepted until sweep; no table cap; endless stats loop.
- SOCKS5 failed/idle flows leaked; flows could be inserted after shutdown cleanup.
  Server Close left control connections open. Handshake deadlines, source-port
  hints, reserved bytes and IPv6 reply encoding were missing or incorrect.
- Mandatory DialUDP broke existing SOCKS dialers; UDP is now optional.
- Independent UDP read deadlines expired active one-way flows.
- gVisor packet/view reference leaks and unguarded detached-endpoint delivery.
- Batched Send accepted data outside Start/Stop; repeated Start spawned workers;
  received frames had no wire-byte/record limits. Queue memory is now bounded.
- Linux close relied on interrupting recvfrom; bounded I/O and descriptor locking
  prevent indefinite reads and fd-reuse I/O during shutdown.
- iOS blindly forwarded UDP to TCP-only exits; opt-in now preserves the fallback.
  Packet lengths and DNS response IPv4 headers were also corrected.

## Remaining blockers

1. Wire-v3 handshake is unauthenticated and not session-bound; replay/reconnect,
   downgrade resistance and MTU negotiation are unfinished. Disabled by default.
2. Raw L3 UDP needs source-port reservation/translation to avoid host collisions
   and kernel ICMP port-unreachable. No privileged Linux test environment was
   available here. Use L4 for UDP until this and a Linux canary are complete.
3. L3 fragmentation/ICMP/PMTU, process shutdown, legacy parser hardening, bounded
   legacy PacketTunnel associations and dial cancellation need follow-up.
4. No physical mobile-device or real document-carrier DNS/QUIC test was run.

## Reproducible checks

From the repository root (cache overrides are for this local sandbox):

```sh
export GOCACHE="$PWD/.cache/go-build"
export GOMODCACHE="$PWD/.cache/go-mod"
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./...
./build_ios.sh
git diff --check
```

L4 echo requires a non-loopback IPv4 interface and explicitly skips without one.
The test does not change routes/VPN. Four codec compositions and 0/12/1200-byte
datagrams must pass. This is not a public-network/QUIC validation.

Final post-review verification: all commands above completed successfully
(exit 0) on macOS arm64 with Go 1.26.5. This includes the full test suite,
race detector, vet, all five cross-builds, the iOS arm64 static library and
whitespace checks. GitHub CI and privileged Linux runtime tests were not run.
