package control

import (
	"encoding/binary"
	"encoding/json"
	"errors"
)

// A client may send its log to the exit, for the exit's owner (ReFlux):
// the user taps "send to the server" and the exit keeps the log in a file.
// The log goes zstd-compressed, cut into parts of SubtypeClientLog (client
// -> exit); the exit answers the whole log with one SubtypeClientLogAck
// (exit -> client). Only a Session client can send one, so only who holds
// the channel's key; the exit decides what it keeps and how often. Peers
// that do not know these subtypes ignore them, like any unknown subtype:
// the client then hears no answer.
const (
	SubtypeClientLog    Subtype = 0x31
	SubtypeClientLogAck Subtype = 0x32
)

// ClientLogPartBytes is the most of the compressed log one part carries.
const ClientLogPartBytes = 1024

// ClientLogMaxParts bounds a log: 256 KiB compressed.
const ClientLogMaxParts = 256

const clientLogHeader = 8

// ClientLogPart is the body of SubtypeClientLog: part Part (from 0) of the
// Parts parts of log ID. On the wire: ID (4 bytes), Part (2), Parts (2),
// then the data, all big-endian.
type ClientLogPart struct {
	ID    uint32
	Part  uint16
	Parts uint16
	Data  []byte
}

// Encode serializes the part.
func (p ClientLogPart) Encode() []byte {
	b := make([]byte, clientLogHeader, clientLogHeader+len(p.Data))
	binary.BigEndian.PutUint32(b[0:4], p.ID)
	binary.BigEndian.PutUint16(b[4:6], p.Part)
	binary.BigEndian.PutUint16(b[6:8], p.Parts)
	return append(b, p.Data...)
}

// DecodeClientLogPart parses a part and checks its numbers.
func DecodeClientLogPart(b []byte) (ClientLogPart, error) {
	if len(b) <= clientLogHeader || len(b) > clientLogHeader+ClientLogPartBytes {
		return ClientLogPart{}, errors.New("control: a client log part needs 1 to 1024 bytes of data")
	}
	p := ClientLogPart{
		ID:    binary.BigEndian.Uint32(b[0:4]),
		Part:  binary.BigEndian.Uint16(b[4:6]),
		Parts: binary.BigEndian.Uint16(b[6:8]),
		Data:  b[clientLogHeader:],
	}
	if p.Parts == 0 || p.Parts > ClientLogMaxParts || p.Part >= p.Parts {
		return ClientLogPart{}, errors.New("control: a client log part out of range")
	}
	return p, nil
}

// SplitClientLog cuts a compressed log into parts; it fails when the log
// is over ClientLogMaxParts parts or empty.
func SplitClientLog(id uint32, data []byte) ([]ClientLogPart, error) {
	n := (len(data) + ClientLogPartBytes - 1) / ClientLogPartBytes
	if n == 0 || n > ClientLogMaxParts {
		return nil, errors.New("control: a client log must be 1 byte to 256 KiB compressed")
	}
	parts := make([]ClientLogPart, n)
	for i := range parts {
		end := min((i+1)*ClientLogPartBytes, len(data))
		parts[i] = ClientLogPart{ID: id, Part: uint16(i), Parts: uint16(n), Data: data[i*ClientLogPartBytes : end]}
	}
	return parts, nil
}

// What an exit answers a client log with.
const (
	ClientLogOK      = "ok"       // kept
	ClientLogTooSoon = "too_soon" // the exit takes no more now: RetryAfter
	ClientLogTooBig  = "too_big"
	ClientLogBad     = "bad" // the parts do not make a log
	ClientLogOff     = "off" // this exit keeps no client logs
	ClientLogError   = "error"
)

// ClientLogAck is the body of SubtypeClientLogAck.
type ClientLogAck struct {
	ID     uint32 `json:"id"`
	Status string `json:"status"`
	// RetryAfter, with too_soon: seconds until the exit takes a log again.
	RetryAfter int `json:"retry_after,omitempty"`
	// Bytes, with ok: the size of the log as kept.
	Bytes int `json:"bytes,omitempty"`
}

// Encode serializes the answer to JSON.
func (a ClientLogAck) Encode() ([]byte, error) { return json.Marshal(a) }

// DecodeClientLogAck parses an answer.
func DecodeClientLogAck(b []byte) (ClientLogAck, error) {
	var a ClientLogAck
	if err := json.Unmarshal(b, &a); err != nil {
		return a, err
	}
	if a.Status == "" {
		return a, errors.New("control: a client log answer needs a status")
	}
	return a, nil
}
