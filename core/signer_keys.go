package core

import (
	"fmt"

	"github.com/DefiantLabs/cosmos-indexer/db/models"
	"github.com/DefiantLabs/probe/client"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/multisig"
	cryptoTypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/cosmos/cosmos-sdk/types"
	cosmosTx "github.com/cosmos/cosmos-sdk/types/tx"
)

// Cosmos SDK 0.54 adds bls12_381 keys and 0.55 adds mldsa65 and secp256k1eth
// keys. This build's SDK (v0.47) has none of them, so a transaction signed
// with one cannot have its signer address derived from its public key. Such a
// transaction is kept with every message stored as opaque evidence, which
// holds back its signers' reports, instead of failing the whole block.

// signerKey unpacks one signer_info public key with the codec's registry.
// Unpacking into the PubKey interface works whether or not the decoder already
// cached the value; a plain wire decode leaves it uncached.
func signerKey(registry codectypes.InterfaceRegistry, key *codectypes.Any) (cryptoTypes.PubKey, error) {
	var pubKey cryptoTypes.PubKey
	if err := registry.UnpackAny(key, &pubKey); err != nil {
		return nil, err
	}
	if pubKey == nil {
		return nil, fmt.Errorf("signer key %s did not decode", key.TypeUrl)
	}
	return pubKey, nil
}

// UnreadableSignerKeys lists the type URLs of signer public keys the codec
// cannot read, in signer_info order. A multisig key with an unreadable member
// is listed under the multisig type URL.
func UnreadableSignerKeys(registry codectypes.InterfaceRegistry, authInfo *cosmosTx.AuthInfo) []string {
	if authInfo == nil {
		return nil
	}
	var out []string
	for _, info := range authInfo.SignerInfos {
		if info == nil || info.PublicKey == nil {
			continue
		}
		if _, err := signerKey(registry, info.PublicKey); err != nil {
			out = append(out, info.PublicKey.TypeUrl)
		}
	}
	return out
}

func keyAddresses(pubKey cryptoTypes.PubKey) []string {
	if ms, ok := pubKey.(*multisig.LegacyAminoPubKey); ok {
		var out []string
		for _, member := range ms.GetPubKeys() {
			out = append(out, types.AccAddress(member.Address().Bytes()).String())
		}
		return out
	}
	return []string{types.AccAddress(pubKey.Address().Bytes()).String()}
}

// signersWithUnreadableKeys returns the signer addresses of a transaction
// whose signer_infos hold a key the codec cannot read, and the default fee
// payer, which by SDK rule is the first signer.
//
// The SDK orders signer_infos as the messages' GetSigners. When every message
// decoded, the first message signer is the fee payer. Otherwise the first
// signer_info names it only if its key is readable and not a multisig (whose
// member addresses are not the account). When neither holds, firstSigner is
// empty and the caller must not guess a payer.
func signersWithUnreadableKeys(cl *client.ChainClient, authInfo *cosmosTx.AuthInfo, messages []types.Msg, allDecoded bool) (signers []models.Address, firstSigner string) {
	seen := map[string]bool{}
	add := func(address string) {
		if address == "" || seen[address] {
			return
		}
		seen[address] = true
		signers = append(signers, models.Address{Address: address})
	}

	var messageSigners []string
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		for _, signer := range msg.GetSigners() {
			messageSigners = append(messageSigners, signer.String())
		}
	}

	var keyed [][]string
	firstReadableSingle := ""
	for i, info := range authInfo.SignerInfos {
		if info == nil || info.PublicKey == nil {
			continue
		}
		pubKey, err := signerKey(cl.Codec.InterfaceRegistry, info.PublicKey)
		if err != nil {
			continue
		}
		addresses := keyAddresses(pubKey)
		keyed = append(keyed, addresses)
		if _, isMultisig := pubKey.(*multisig.LegacyAminoPubKey); i == 0 && !isMultisig {
			firstReadableSingle = addresses[0]
		}
	}

	switch {
	case allDecoded && len(messageSigners) > 0:
		firstSigner = messageSigners[0]
	case firstReadableSingle != "":
		firstSigner = firstReadableSingle
	}

	add(firstSigner)
	for _, address := range messageSigners {
		add(address)
	}
	for _, addresses := range keyed {
		for _, address := range addresses {
			add(address)
		}
	}
	if authInfo.Fee != nil {
		add(authInfo.Fee.GetPayer())
	}
	return signers, firstSigner
}
