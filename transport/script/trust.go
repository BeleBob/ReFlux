package script

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// OfficialKeyHex is the OpenFlux project's own ed25519 signing key (hex). A
// script signed by it is shown as a first-party transport; every other key
// is an unknown author the app pins on trust (TOFU). It is a public key, so
// sharing the literal between the CLI and the gomobile bindings is fine -
// nothing secret crosses that boundary.
const OfficialKeyHex = "d8bf9c958b994c2faab886cade5f28213f254911f87abe5e34756a289ae91354"

// TrustReport is what an app shows in its "trust this transport?" dialog
// before installing a downloaded script, and what it stores alongside the
// file afterwards (name, fingerprint, params). See InspectTrust.
type TrustReport struct {
	OK            bool    `json:"ok"`
	Error         string  `json:"error,omitempty"`
	Name          string  `json:"name,omitempty"`
	Version       string  `json:"version,omitempty"`
	Params        []Param `json:"params,omitempty"`
	Signature     string  `json:"signature"` // "valid" | "invalid" | "unverified"
	Fingerprint   string  `json:"fingerprint,omitempty"`
	Official      bool    `json:"official,omitempty"`
	Author        string  `json:"author,omitempty"`
	PackageAuthor string  `json:"packageAuthor,omitempty"`
}

// InspectTrust reads a downloaded transport before it is trusted or run and
// reports what it claims to be and whether it is signed by pubkeyHex,
// WITHOUT ever running open() or touching the network. data is a .flux
// package (zip) or a bare .js source; sig is the detached signature for a
// bare .js (ignored for .flux, which carries its own); pubkeyHex is the
// candidate author key (from a share link, a GitHub release, or manual
// entry), or "" to inspect without a key. officialKeyHex, if non-empty, is
// compared against pubkeyHex to set Official/Author.
func InspectTrust(data, sig []byte, pubkeyHex, officialKeyHex string) TrustReport {
	report := TrustReport{Signature: "unverified"}

	pubkeyHex = strings.ReplaceAll(strings.TrimSpace(pubkeyHex), " ", "")
	var pub []byte
	if pubkeyHex != "" {
		p, err := DecodePublicKeyHex(pubkeyHex)
		if err != nil {
			report.Error = "ключ автора: " + err.Error()
			return report
		}
		pub = p
		sum := sha256.Sum256(pub)
		report.Fingerprint = hex.EncodeToString(sum[:])
		if officialKeyHex != "" && strings.EqualFold(pubkeyHex, officialKeyHex) {
			report.Official = true
			report.Author = "OpenFlux"
		}
	}

	var src []byte
	if len(data) >= 2 && data[0] == 'P' && data[1] == 'K' { // .flux (zip)
		pkg, err := ReadPackage(data)
		if err != nil {
			report.Error = "пакет .flux: " + err.Error()
			return report
		}
		src = pkg.Script
		if pkg.Manifest.Author != "" {
			report.PackageAuthor = pkg.Manifest.Author
		}
		if pub != nil {
			if pkg.Verify(pub) == nil {
				report.Signature = "valid"
			} else {
				report.Signature = "invalid"
			}
		}
	} else { // bare .js
		src = data
		if pub != nil && len(sig) > 0 {
			if VerifyScript(src, sig, pub) == nil {
				report.Signature = "valid"
			} else {
				report.Signature = "invalid"
			}
		}
	}

	info, err := Inspect(src)
	if err != nil {
		report.Error = "чтение манифеста: " + err.Error()
		return report
	}
	report.Name = info.Name
	report.Version = info.Version
	report.Params = info.Params
	report.OK = true
	return report
}

// Fingerprint returns the SHA-256 (hex) of an author public key, the stable
// id an app shows the user to compare out of band. "" if the key can't be
// decoded.
func Fingerprint(pubkeyHex string) string {
	pub, err := DecodePublicKeyHex(strings.ReplaceAll(strings.TrimSpace(pubkeyHex), " ", ""))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}
