package provision

import "fmt"

// The node-install.sh scripts this app build runs, each a file at a fixed
// commit and its SHA-256. The script decides which repository's node-v*
// releases the server's core and its updater come from, so choosing a
// script chooses the node's core.
//
// Pinned* is this repository's own deploy/node-install.sh
// (meepo161/OpenFlux releases); TestPinnedScriptHash keeps its hash in step
// with the file: when the script changes, commit it, then point
// PinnedCommit at that commit. Official* is the same script following
// p1neappleXpress/OpenFlux releases.
const (
	PinnedRepo   = "meepo161/OpenFlux"
	PinnedCommit = "0e21cb71731de61c5961dcf55b8c8b2194958681"
	PinnedSHA256 = "764d9eb74540c46f7bdf8b7f391f86dc18b9364b886befd25b602687e827b193"

	OfficialRepo   = "p1neappleXpress/OpenFlux"
	OfficialCommit = "cdc6c0d10adc82eda3cb3ccaeee0b67ca3dd5597"
	OfficialSHA256 = "11b65510d0ded807f33e172357c70d787c67b02facc9eb8e37156c25f8fb2ba1"
)

// Script sources the node wizard offers.
const (
	SourceFork     = "fork"
	SourceOfficial = "official"
)

// Pinned returns this build's own script (SourceFork).
func Pinned() Script {
	return Script{
		URL:    "https://raw.githubusercontent.com/" + PinnedRepo + "/" + PinnedCommit + "/deploy/node-install.sh",
		SHA256: PinnedSHA256,
	}
}

// PinnedFor returns the script for a source: SourceFork (also "") or
// SourceOfficial.
func PinnedFor(source string) (Script, error) {
	switch source {
	case "", SourceFork:
		return Pinned(), nil
	case SourceOfficial:
		return Script{
			URL:    "https://raw.githubusercontent.com/" + OfficialRepo + "/" + OfficialCommit + "/deploy/node-install.sh",
			SHA256: OfficialSHA256,
		}, nil
	}
	return Script{}, fmt.Errorf("неизвестный источник ядра %q", source)
}
