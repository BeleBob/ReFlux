// scriptsign is a small dev/ops CLI for the script-transport signing
// scheme in transport/script/sign.go and transport/script/flux.go. It has
// no dependency on the rest of the OpenFlux binary and does not need to run
// on the node/exit itself.
//
//	scriptsign genkey <priv.key> <pub.key>
//	scriptsign sign   <priv.key> <transport.js>        writes transport.js.sig
//	scriptsign verify <pub.key>  <transport.js>        (or a .flux package)
//	scriptsign pack [-store] <priv.key> <manifest.json> <script.js> <out.flux> [icon-file]
//
// Keys are stored raw-hex, one line, no headers.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/transport/script"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "genkey":
		err = genKey(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	case "pack":
		err = pack(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "scriptsign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  scriptsign genkey <priv.key> <pub.key>")
	fmt.Fprintln(os.Stderr, "  scriptsign sign   <priv.key> <transport.js>")
	fmt.Fprintln(os.Stderr, "  scriptsign verify <pub.key>  <transport.js|transport.flux>")
	fmt.Fprintln(os.Stderr, "  scriptsign pack [-store] <priv.key> <manifest.json> <script.js> <out.flux> [icon-file]")
	os.Exit(2)
}

func genKey(args []string) error {
	if len(args) != 2 {
		usage()
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := writeHexFile(args[0], priv); err != nil {
		return err
	}
	if err := writeHexFile(args[1], pub); err != nil {
		return err
	}
	fmt.Printf("wrote %s (private) and %s (public)\n", args[0], args[1])
	fmt.Printf("public key hex (for a transport config's \"pubkey\" param):\n%s\n", hex.EncodeToString(pub))
	return nil
}

func sign(args []string) error {
	if len(args) != 2 {
		usage()
	}
	privHex, err := readHexFile(args[0])
	if err != nil {
		return err
	}
	if len(privHex) != ed25519.PrivateKeySize {
		return fmt.Errorf("private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(privHex))
	}
	src, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	sig := ed25519.Sign(ed25519.PrivateKey(privHex), src)
	sigPath := args[1] + ".sig"
	if err := os.WriteFile(sigPath, sig, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", sigPath)
	return nil
}

func verify(args []string) error {
	if len(args) != 2 {
		usage()
	}
	pubHex, err := readHexFile(args[0])
	if err != nil {
		return err
	}
	if strings.HasSuffix(args[1], ".flux") {
		pkg, err := script.LoadSignedPackage(args[1], ed25519.PublicKey(pubHex))
		if err != nil {
			return err
		}
		fmt.Printf("OK: signature verified (name=%s version=%s author=%s)\n",
			pkg.Manifest.Name, pkg.Manifest.Version, pkg.Manifest.Author)
		return nil
	}
	if _, err := script.LoadSigned(args[1], ed25519.PublicKey(pubHex)); err != nil {
		return err
	}
	fmt.Println("OK: signature verified")
	return nil
}

// pack builds and signs a .flux package from a manifest.json + script.js
// (+ optional icon file), writing out.flux. manifest.json's own "icon"
// field, if any, is overwritten to match the icon file actually given (or
// cleared if none is) - a package's manifest can never reference an icon
// that isn't in the archive next to it.
func pack(args []string) error {
	fs := flag.NewFlagSet("pack", flag.ExitOnError)
	store := fs.Bool("store", false, "store the script/icon uncompressed instead of deflating")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) != 4 && len(rest) != 5 {
		usage()
	}
	privHex, err := readHexFile(rest[0])
	if err != nil {
		return err
	}
	if len(privHex) != ed25519.PrivateKeySize {
		return fmt.Errorf("private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(privHex))
	}
	manifestPath, scriptPath, outPath := rest[1], rest[2], rest[3]

	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest script.PackageManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return fmt.Errorf("decode %s: %w", manifestPath, err)
	}
	if manifest.Name == "" {
		return fmt.Errorf("%s must set \"name\"", manifestPath)
	}

	scriptSrc, err := os.ReadFile(scriptPath)
	if err != nil {
		return err
	}

	var icon []byte
	manifest.Icon = ""
	if len(rest) == 5 {
		iconPath := rest[4]
		icon, err = os.ReadFile(iconPath)
		if err != nil {
			return err
		}
		manifest.Icon = filepath.Base(iconPath)
	}

	if err := script.WritePackage(outPath, manifest, scriptSrc, icon, ed25519.PrivateKey(privHex), !*store); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", outPath)
	return nil
}

func writeHexFile(path string, key []byte) error {
	return os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600)
}

func readHexFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimSpace(string(b)))
}
