# Authenticated negotiation v1

Opt-in CLI extension to the existing encrypted packet stack, not a new cipher
suite or a production security certification. Both peers need `--negotiate`,
batched codec and the same encryption secret/context. Old clients require the
unchanged default mode on the exit. No automatic downgrade is implemented.

## Envelope

Send order: IPv4 -> negotiation envelope -> existing AES-GCM packet -> batch-v2
framing/compression -> carrier. Receive reverses that order. The envelope is
inside AEAD, including its type, role, identities, capabilities and sequence.
Integers are unsigned big-endian; unknown versions/types/reserved bits fail closed.

| Bytes | Meaning |
| --- | --- |
| 0..3 | `OFN` followed by version byte 1 |
| 4 | 1 = hello, 2 = IPv4 data |
| 5 | sender role: 0 client, 1 exit; peer must have opposite role |
| 6..37 | sender's random 256-bit process-session challenge |
| 38..69 | recipient's challenge; zero only for initial hello |
| 70..77 (hello) | capabilities uint32, max IPv4 packet uint16, ready byte (0/1), reserved zero byte |
| 70..77 (data) | sequence uint64, starting at 1 |
| 78.. (data) | one complete IPv4 packet (possibly an IP fragment) |

Hello length is exactly 78 bytes. Capability bits: 0 IPv4, 1 TCP, 2 UDP,
4 ICMP errors. Bit 3 belonged to the retired wire-v3 prototype and is rejected.
IPv4+TCP are mandatory. Allowed packet limits: 1280..65000, leaving space for
this envelope and existing AES overhead within a 65535-byte batch record.

## State and acceptance

Each instance generates a cryptographically random challenge. An authenticated
hello without an echo can elicit a response but does not establish a session.
Readiness requires a valid peer hello echoing the current local challenge.
The effective policy is the capability intersection and smaller packet limit.
Established peer identity and effective policy cannot be replaced by later
hellos. Retries with an unconfirmed ready bit receive a fresh confirmation,
including after the other side has completed Start. The startup deadline is
20 seconds after the underlying carrier's Start returns.

Data must name both current challenges, use a negotiated protocol, fit the
effective packet limit, and pass the 64-entry sequence replay window. Duplicates,
zero sequence numbers and packets older than the window are discarded; bounded
reordering is accepted. There is no delivery acknowledgment or retransmission.
This avoids adding a reliability layer underneath UDP/QUIC.

## Limits and compatibility

Carrier reconnects retain the process-session identity. Process restart requires
restarting both peers; secure live session replacement is intentionally absent.
Shared-secret holders are trusted peers. The existing static key derivation is
unchanged: no forward secrecy, automatic key rotation or protection after secret
compromise is claimed. AEAD authenticates packets; capability assertions still
describe configured software functionality, not a live Internet reachability test.

ICMP-error support refers to errors returned from a raw exit to the client; it
does not promise bidirectional arbitrary ICMP, IPv6, echo or redirects. There
is no active path-MTU probing. The separate raw-exit ICMP/NAT implementation
reports kernel route MTU errors and restores Internet ICMP quotes for live flows.
