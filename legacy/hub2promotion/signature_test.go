package hub2promotion

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// Uses an ephemeral fixture authority ONLY. It never signs with Dan's key and
// cannot manufacture a trusted complete-financial seal.
func fixtureSSHSIG(t *testing.T, message []byte, namespace string) ([]byte, string) {
	_, private, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	signer, e := ssh.NewSignerFromKey(private)
	require.NoError(t, e)
	h := sha512.Sum512(message)
	signed := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Namespace     string
		Reserved      string
		HashAlgorithm string
		Hash          []byte
	}{namespace, "", "sha512", h[:]})...)
	signature, e := signer.Sign(rand.Reader, signed)
	require.NoError(t, e)
	wire := append([]byte("SSHSIG"), ssh.Marshal(struct {
		Version       uint32
		PublicKey     []byte
		Namespace     string
		Reserved      string
		HashAlgorithm string
		Signature     []byte
	}{1, signer.PublicKey().Marshal(), namespace, "", "sha512", ssh.Marshal(signature)})...)
	return pem.EncodeToMemory(&pem.Block{Type: "SSH SIGNATURE", Bytes: wire}), ssh.FingerprintSHA256(signer.PublicKey())
}
func TestSSHSIGMatchesActualOpenSSHFixtureSignature(t *testing.T) {
	dir := artifactDir(t)
	_, private, e := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, e)
	signer, e := ssh.NewSignerFromKey(private)
	require.NoError(t, e)
	block, e := ssh.MarshalPrivateKey(private, "ephemeral fixture only")
	require.NoError(t, e)
	keypath := filepath.Join(dir, "ephemeral-fixture-key")
	require.NoError(t, os.WriteFile(keypath, pem.EncodeToMemory(block), 0600))
	message := []byte(`{"fixture_only":true,"no_financial_acceptance":true}`)
	path := filepath.Join(dir, "fixture-message.json")
	require.NoError(t, os.WriteFile(path, message, 0600))
	output, e := exec.Command("ssh-keygen", "-Y", "sign", "-f", keypath, "-n", "fixture-only-never-financial", path).CombinedOutput()
	require.NoError(t, e, string(output))
	signature, e := os.ReadFile(path + ".sig")
	require.NoError(t, e)
	require.NoError(t, verifySSHSIG(message, signature, ssh.FingerprintSHA256(signer.PublicKey()), "fixture-only-never-financial"))
	require.Error(t, verifySSHSIG(message, signature, financialSigner, financialNamespace))
}

func TestFinancialAuthorityRequiresTrustedNamespacedUnchangedReceipt(t *testing.T) {
	message := []byte(`{"fixture_only":true}`)
	sig, key := fixtureSSHSIG(t, message, "fixture-only-never-financial")
	require.NoError(t, verifySSHSIG(message, sig, key, "fixture-only-never-financial"))
	require.Error(t, verifySSHSIG(message, sig, financialSigner, financialNamespace))
	require.Error(t, verifySSHSIG(message, sig, key, financialNamespace))
	require.Error(t, verifySSHSIG([]byte(`{"changed":true}`), sig, key, "fixture-only-never-financial"))
	require.Error(t, verifySSHSIG(message, append(sig, []byte("trailing")...), key, "fixture-only-never-financial"))
	f, m, _ := strictEnvelopeFixture(t)
	m.FinancialSignature = savePin(t, f.dir, "untrusted-owner.sig", sig)
	require.Error(t, m.verifyFinancialAuthority(), "valid fixture cryptography is not financial authority")
}
