package ipc

// LinkStatus is one carrier of a Session in the status, in the Session's
// order: highest priority first.
type LinkStatus struct {
	Name string `json:"name"`
	// Attached: connected to its document on this side, a peer there or
	// not. Live: the peer was heard on it lately, so data goes over it.
	Attached bool `json:"attached,omitempty"`
	Live     bool `json:"live,omitempty"`
	// HeardMs is when the peer was last heard on it, in Unix milliseconds;
	// 0 when it never was.
	HeardMs int64 `json:"heard_ms,omitempty"`
	// BytesIn and BytesOut count what the carrier received and sent since
	// the core started, as the carrier's service sees it.
	BytesIn  uint64 `json:"bytes_in"`
	BytesOut uint64 `json:"bytes_out"`
	// Queued is how many bytes wait to be sent over it, Flows how many
	// flows sent over it in the last few seconds.
	Queued int64 `json:"queued,omitempty"`
	Flows  int   `json:"flows,omitempty"`
}
