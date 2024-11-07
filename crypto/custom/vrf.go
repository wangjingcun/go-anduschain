package custom

import (
	"crypto/ecdsa"
	"github.com/vechain/go-ecvrf"
)

// `beta`: the VRF hash output
// `pi`: the VRF proof
func VrfProve(privateKey *ecdsa.PrivateKey, alpha string) ([]byte, []byte, error) {
	return ecvrf.Secp256k1Sha256Tai.Prove(privateKey, []byte(alpha))
}

func VrfVerify(publicKey *ecdsa.PublicKey, alpha string, pi []byte) ([]byte, error) {
	return ecvrf.Secp256k1Sha256Tai.Verify(publicKey, []byte(alpha), pi)
}
