package hub2promotion

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/json"
	"encoding/pem"
	"errors"

	"golang.org/x/crypto/ssh"
)

// This is the fingerprint independently verified on signed base2c1eb0 and the
// configured release signing key. No private key, credential or signing operation
// is used by admission. A new financial trust key requires a reviewed code change.
const financialSigner = "SHA256:aA+JaHfEzDupOPiyZKoDPX0XQse4UGFtfbQWrYKneGs"
const financialNamespace = "hub2-complete-financial-v1"

// verifySSHSIG implements the OpenSSH PROTOCOL.sshsig wire format. Verification
// authenticates an owner's attestation; it does not re-execute financial proof.
func verifySSHSIG(message, armored []byte, fingerprint, namespace string) error {
	block, rest := pem.Decode(armored)
	if block == nil || block.Type != "SSH SIGNATURE" || len(rest) != 0 || len(block.Headers) != 0 {
		return errors.New("invalid financial SSHSIG armor")
	}
	data := block.Bytes
	if len(data) < 6 || string(data[:6]) != "SSHSIG" {
		return errors.New("invalid financial SSHSIG magic")
	}
	var wire struct {
		Version       uint32
		PublicKey     []byte
		Namespace     string
		Reserved      string
		HashAlgorithm string
		Signature     []byte
	}
	if e := ssh.Unmarshal(data[6:], &wire); e != nil {
		return errors.New("invalid financial SSHSIG wire")
	}
	if wire.Version != 1 || wire.Namespace != namespace || wire.Reserved != "" {
		return errors.New("financial signature namespace/version")
	}
	key, e := ssh.ParsePublicKey(wire.PublicKey)
	if e != nil || key.Type() != ssh.KeyAlgoED25519 || ssh.FingerprintSHA256(key) != fingerprint {
		return errors.New("untrusted financial signing authority")
	}
	var hash []byte
	switch wire.HashAlgorithm {
	case "sha512":
		h := sha512.Sum512(message)
		hash = h[:]
	case "sha256":
		h := sha256.Sum256(message)
		hash = h[:]
	default:
		return errors.New("unapproved financial signature hash")
	}
	signed := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Namespace     string
		Reserved      string
		HashAlgorithm string
		Hash          []byte
	}{wire.Namespace, wire.Reserved, wire.HashAlgorithm, hash})...)
	var signature ssh.Signature
	if e = ssh.Unmarshal(wire.Signature, &signature); e != nil || signature.Format != ssh.KeyAlgoED25519 || len(signature.Rest) != 0 {
		return errors.New("invalid financial signature algorithm")
	}
	if e = key.Verify(signed, &signature); e != nil {
		return errors.New("financial owner signature does not match sealed receipt")
	}
	return nil
}

// CanonicalBytes is only deterministic formatting for the final owner's review
// and signature. It makes no completeness or financial acceptance claim.
func (r FinancialReceipt) CanonicalBytes() ([]byte, error) { return json.Marshal(r) }

func (m Manifest) verifyFinancialAuthority() error {
	receipt, e := readPinnedBytes(m.Financial, 16<<20)
	if e != nil {
		return e
	}
	var decoded FinancialReceipt
	if e = json.Unmarshal(receipt, &decoded); e != nil {
		return e
	}
	canonical, e := decoded.CanonicalBytes()
	if e != nil {
		return e
	}
	if !bytes.Equal(receipt, canonical) {
		return errors.New("financial owner receipt must be exact canonical JSON, no duplicate/case/hidden fields")
	}
	signature, e := readPinnedBytes(m.FinancialSignature, 64<<10)
	if e != nil {
		return e
	}
	return verifySSHSIG(receipt, signature, financialSigner, financialNamespace)
}
