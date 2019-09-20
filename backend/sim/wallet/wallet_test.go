// Copyright (c) 2019 The Perun Authors. All rights reserved.
// This file is part of go-perun. Use of this source code is governed by a
// MIT-style license that can be found in the LICENSE file.

package wallet // import "perun.network/go-perun/backend/sim/wallet"

import (
	"math/big"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"

	"perun.network/go-perun/wallet"
	"perun.network/go-perun/wallet/test"
)

// TestSignatureSerialize tests serializeSignature and deserializeSignature since
// a signature is only a []byte, we cant use io.serializable here
func TestSignatureSerialize(t *testing.T) {
	a := assert.New(t)
	// Constant seed for determinism
	rng := rand.New(rand.NewSource(1337))

	// More iterations are better for catching value dependent bugs
	for i := 0; i < 10; i++ {
		rBytes := make([]byte, 32)
		sBytes := make([]byte, 32)

		// These always return nil error
		rng.Read(rBytes)
		rng.Read(sBytes)

		r := new(big.Int).SetBytes(rBytes)
		s := new(big.Int).SetBytes(sBytes)

		sig, err1 := serializeSignature(r, s)
		a.Nil(err1, "Serialization should not fail")
		R, S, err2 := deserializeSignature(sig)

		a.Nil(err2, "Deserialization should not fail")
		a.Equal(r, R, "Serialized and deserialized r values should be equal")
		a.Equal(s, S, "Serialized and deserialized s values should be equal")
	}
}

func TestGenericTests(t *testing.T) {
	t.Run("Generic Address Test", func(t *testing.T) {
		t.Parallel()
		test.GenericAddressTest(t, newWalletSetup())
	})
	t.Run("Generic Wallet Test", func(t *testing.T) {
		t.Parallel()
		test.GenericWalletTest(t, newWalletSetup())
	})
	t.Run("Generic Signature Test", func(t *testing.T) {
		t.Parallel()
		test.GenericSignatureTest(t, newWalletSetup())
	})

	// NewRandomAddress is also tested in channel_test but since they are two packages,
	// we also need to test it here
	rng := rand.New(rand.NewSource(1337))
	for i := 0; i < 10; i++ {
		assert.NotEqual(t, NewRandomAddress(rng), NewRandomAddress(rng), "Two random accounts should not be the same")
	}
}

func newWalletSetup() *test.Setup {
	rng := rand.New(rand.NewSource(1337))

	accountA := NewRandomAccount(rng)
	accountB := NewRandomAccount(rng)
	initWallet := func(w wallet.Wallet) error { return w.Connect("", "") }
	unlockedAccount := func() (wallet.Account, error) { return &accountA, nil }

	return &test.Setup{
		Wallet:          &Wallet{directory: "", account: accountA},
		Backend:         new(Backend),
		UnlockedAccount: unlockedAccount,
		InitWallet:      initWallet,
		AddrString:      accountB.Address().String(),
	}
}