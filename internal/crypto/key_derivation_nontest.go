//go:build !testing
// +build !testing

package crypto

import (
	"github.com/pkg/errors"
	"golang.org/x/crypto/scrypt"
)

// DefaultKeyDerivationAlgorithm is the key derivation algorithm for new configurations.
const DefaultKeyDerivationAlgorithm = "scrypt-65536-8-1"

// DeriveKeyFromPassword derives encryption key using the provided password and per-repository unique ID.
func DeriveKeyFromPassword(password string, salt []byte, algorithm string) ([]byte, error) {
	const masterKeySize = 32

	switch algorithm {
	case "scrypt-65536-8-1":
		//nolint:wrapcheck,gomnd
		return scrypt.Key([]byte(password), salt, 65536, 8, 1, masterKeySize)

	default:
		return nil, errors.Errorf("unsupported key algorithm: %v", algorithm)
	}
}
