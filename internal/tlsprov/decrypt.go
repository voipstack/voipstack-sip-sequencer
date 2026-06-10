package tlsprov

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
)

// decryptKeyPEM returns an unencrypted key PEM ready for tls.X509KeyPair, given an
// encrypted key PEM and its passphrase. It uses the standard library only.
//
//   - PKCS#8 ENCRYPTED PRIVATE KEY blocks are unsupported (the stdlib cannot decrypt
//     the DER-embedded EncryptedPrivateKeyInfo): returns an actionable error.
//   - Legacy PKCS#1 (DEK-Info) blocks are decrypted with x509.DecryptPEMBlock and
//     re-wrapped as the same key type, now unencrypted.
//   - A passphrase set on an already-unencrypted key is ignored (debug note, no fail).
//
// No key or passphrase bytes appear in any returned error.
func decryptKeyPEM(log *slog.Logger, keyPEM []byte, passphrase string) ([]byte, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("no PEM block in private key")
	}

	if block.Type == "ENCRYPTED PRIVATE KEY" {
		return nil, errors.New("PKCS#8-encrypted private keys are not supported; provide an unencrypted key or a legacy (PKCS#1, DEK-Info) encrypted key")
	}

	if x509.IsEncryptedPEMBlock(block) { //nolint:staticcheck // SA1019: stdlib-only legacy decrypt by design
		der, err := x509.DecryptPEMBlock(block, []byte(passphrase)) //nolint:staticcheck // SA1019: see above
		if err != nil {
			return nil, errors.New("could not decrypt private key")
		}
		return pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: der}), nil
	}

	log.Debug("passphrase set but key is not encrypted")
	return keyPEM, nil
}
