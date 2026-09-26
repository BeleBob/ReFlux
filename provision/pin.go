package provision

// Pinned is the node-install.sh this app build runs: the file at a fixed
// commit of the app's own repository and its SHA-256. TestPinnedScriptHash
// keeps the hash in step with deploy/node-install.sh; when the script
// changes, commit it, then point PinnedCommit at that commit.
const (
	PinnedRepo   = "meepo161/openfluxfork"
	PinnedCommit = "c11dd7276ddf5937de2b481d39db545542319a12"
	PinnedSHA256 = "a68ce60ecde8dccc5ef6e541974b874ff3a029b49a17f1255be968d00e1a9313"
)

// Pinned returns the script location for this build.
func Pinned() Script {
	return Script{
		URL:    "https://raw.githubusercontent.com/" + PinnedRepo + "/" + PinnedCommit + "/deploy/node-install.sh",
		SHA256: PinnedSHA256,
	}
}
