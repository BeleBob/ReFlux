package transport

import (
	"encoding/binary"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Wire format for a batched frame (one Yandex/transport message can now carry
// many tunnel packets):
//
//	[0]   version byte (batchFormatVersion)
//	[1]   flags (bit0 = payload is zstd-compressed)
//	[2:]  payload: a sequence of [2-byte big-endian length][packet] records,
//	      optionally zstd-compressed as a whole.
const (
	batchFormatVersion = 0x02
	wireFormatVersion  = 0x03
	batchFlagZstd      = 0x01
	wireTypeData       = 0x01
	wireV3HeaderLen    = 20
	maxFrameBytes      = 1 << 20
	maxFrameRecords    = 1024
)

type wireMetadata struct {
	version   byte
	sessionID uint32
	sequence  uint64
}

// Capabilities belong to the experimental negotiation protocol. They are not
// authenticated or session-bound; do not enable it on untrusted channels.
type Capabilities uint32

const (
	CapabilityIPv4 Capabilities = 1 << iota
	CapabilityTCP
	CapabilityUDP
	CapabilityWireV3
)

const DefaultCapabilities = CapabilityIPv4 | CapabilityTCP | CapabilityUDP | CapabilityWireV3

var capabilityMagic = [5]byte{0x00, 'O', 'F', 'X', wireFormatVersion}

var (
	zstdEnc *zstd.Encoder
	zstdDec *zstd.Decoder
)

func init() {
	var err error
	// SpeedDefault (~level 3): far better ratio than LZ4 at a CPU cost that is
	// irrelevant next to the Yandex channel's latency. Single-shot EncodeAll /
	// DecodeAll are safe for concurrent use on a shared instance.
	zstdEnc, err = zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		panic(fmt.Sprintf("zstd encoder init: %v", err))
	}
	zstdDec, err = zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		// Bound the damage from a malformed/hostile frame injected into the
		// shared document: cap decompressed memory.
		zstd.WithDecoderMaxMemory(8<<20),
	)
	if err != nil {
		panic(fmt.Sprintf("zstd decoder init: %v", err))
	}
}

// frameBatch concatenates packets into length-prefixed records.
func frameBatch(pkts [][]byte) []byte {
	total := 0
	for _, p := range pkts {
		total += 2 + len(p)
	}
	out := make([]byte, 0, total)
	var lenbuf [2]byte
	for _, p := range pkts {
		binary.BigEndian.PutUint16(lenbuf[:], uint16(len(p)))
		out = append(out, lenbuf[:]...)
		out = append(out, p...)
	}
	return out
}

// encodeBatch serializes packets into a single wire frame, compressing the
// whole batch with zstd only when that actually shrinks it.
func encodeBatch(pkts [][]byte) []byte {
	return encodeBatchVersion(pkts, batchFormatVersion)
}

func encodeBatchV3(pkts [][]byte, sessionID uint32, sequence uint64) []byte {
	return encodeBatchVersion(pkts, wireFormatVersion, uint64(sessionID), sequence)
}

func encodeBatchVersion(pkts [][]byte, version byte, metadata ...uint64) []byte {
	framed := frameBatch(pkts)
	compressed := zstdEnc.EncodeAll(framed, nil)
	headerLen := 2
	if version == wireFormatVersion {
		headerLen = wireV3HeaderLen
	}
	makeHeader := func(flags byte, payloadLen int) []byte {
		out := make([]byte, headerLen, headerLen+payloadLen)
		out[0] = version
		out[1] = flags
		if version == wireFormatVersion {
			out[2] = wireTypeData
			if len(metadata) >= 2 {
				binary.BigEndian.PutUint32(out[4:8], uint32(metadata[0]))
				binary.BigEndian.PutUint64(out[8:16], metadata[1])
			}
			binary.BigEndian.PutUint32(out[16:20], uint32(payloadLen))
		}
		return out
	}

	if len(compressed) < len(framed) {
		out := makeHeader(batchFlagZstd, len(compressed))
		return append(out, compressed...)
	}
	out := makeHeader(0, len(framed))
	return append(out, framed...)
}

// decodeBatch reverses encodeBatch, returning the original packets.
func decodeBatch(data []byte) ([][]byte, error) {
	pkts, _, err := decodeBatchFrame(data)
	return pkts, err
}

func decodeBatchFrame(data []byte) ([][]byte, wireMetadata, error) {
	var metadata wireMetadata
	if len(data) > maxFrameBytes+wireV3HeaderLen {
		return nil, metadata, fmt.Errorf("batch frame exceeds size limit")
	}
	if len(data) < 2 {
		return nil, metadata, fmt.Errorf("batch frame too short: %d bytes", len(data))
	}
	version := data[0]
	metadata.version = version
	headerLen := 2
	if version == wireFormatVersion {
		if len(data) < wireV3HeaderLen {
			return nil, metadata, fmt.Errorf("v3 frame too short: %d bytes", len(data))
		}
		if data[2] != wireTypeData || data[3] != 0 {
			return nil, metadata, fmt.Errorf("unknown v3 frame type 0x%02x", data[2])
		}
		headerLen = wireV3HeaderLen
		metadata.sessionID = binary.BigEndian.Uint32(data[4:8])
		metadata.sequence = binary.BigEndian.Uint64(data[8:16])
		payloadLen := int(binary.BigEndian.Uint32(data[16:20]))
		if payloadLen != len(data)-headerLen {
			return nil, metadata, fmt.Errorf("v3 payload length %d, have %d", payloadLen, len(data)-headerLen)
		}
	} else if version != batchFormatVersion {
		return nil, metadata, fmt.Errorf("unknown batch version 0x%02x", data[0])
	}
	flags := data[1]
	if flags & ^byte(batchFlagZstd) != 0 {
		return nil, metadata, fmt.Errorf("unknown batch flags 0x%02x", flags)
	}
	payload := data[headerLen:]

	framed := payload
	if flags&batchFlagZstd != 0 {
		var err error
		framed, err = zstdDec.DecodeAll(payload, nil)
		if err != nil {
			return nil, metadata, fmt.Errorf("zstd decode: %w", err)
		}
	}

	if len(framed) > maxFrameBytes {
		return nil, metadata, fmt.Errorf("decoded batch exceeds size limit")
	}
	var pkts [][]byte
	for len(framed) > 0 {
		if len(pkts) >= maxFrameRecords {
			return nil, metadata, fmt.Errorf("batch exceeds record limit")
		}
		if len(framed) < 2 {
			return nil, metadata, fmt.Errorf("truncated length prefix")
		}
		n := int(binary.BigEndian.Uint16(framed[:2]))
		framed = framed[2:]
		if len(framed) < n {
			return nil, metadata, fmt.Errorf("truncated packet: need %d, have %d", n, len(framed))
		}
		pkt := make([]byte, n)
		copy(pkt, framed[:n])
		pkts = append(pkts, pkt)
		framed = framed[n:]
	}
	return pkts, metadata, nil
}

func encodeCapabilityRecord(caps Capabilities, ack bool) []byte {
	p := make([]byte, 10)
	copy(p, capabilityMagic[:])
	if ack {
		p[5] = 1
	}
	binary.BigEndian.PutUint32(p[6:], uint32(caps))
	return p
}

func decodeCapabilityRecord(p []byte) (Capabilities, bool, bool) {
	if len(p) != 10 || string(p[:5]) != string(capabilityMagic[:]) || p[5] > 1 {
		return 0, false, false
	}
	return Capabilities(binary.BigEndian.Uint32(p[6:])), p[5]&1 != 0, true
}
