// Command d256-sign creates the release signing key and signs release binaries.
//
//	d256-sign keygen                      print a new private key (secret!) and write the public key to internal/update/signing.pub
//	d256-sign sign <version> <os> <arch> <file>   write <file>.sig using the private key from $D256_SIGNING_KEY
//
// The private key lives only in the GitHub Actions secret D256_SIGNING_KEY; the public key is committed
// and compiled into every node.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dest1256/d256-node/internal/update"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "d256-sign:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: d256-sign keygen | sign <version> <os> <arch> <file>")
	}
	switch args[0] {
	case "keygen":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		const pubPath = "internal/update/signing.pub"
		if err := os.WriteFile(pubPath, []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "public key written to %s - commit it.\n", pubPath)
		fmt.Fprintln(os.Stderr, "Store the private key below as the GitHub Actions secret D256_SIGNING_KEY and keep no other copy in plain text:")
		fmt.Println(base64.StdEncoding.EncodeToString(priv))
		return nil
	case "sign":
		if len(args) != 5 {
			return fmt.Errorf("usage: d256-sign sign <version> <os> <arch> <file>")
		}
		version, goos, goarch, file := args[1], args[2], args[3], args[4]
		if !update.ValidVersion(version) {
			return fmt.Errorf("%q is not a release version like v1.2.3", version)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("D256_SIGNING_KEY")))
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			return fmt.Errorf("D256_SIGNING_KEY is missing or malformed")
		}
		priv := ed25519.PrivateKey(raw)
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		sig := ed25519.Sign(priv, update.SignedMessage(version, goos, goarch, hex.EncodeToString(h.Sum(nil))))
		return os.WriteFile(file+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644)
	}
	return fmt.Errorf("unknown command %q", args[0])
}
