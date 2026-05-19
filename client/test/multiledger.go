// Copyright 2025 - See NOTICE file for copyright holders.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	"bytes"
	"context"
	"math/big"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"perun.network/go-perun/channel"
	"perun.network/go-perun/channel/multi"
	chtest "perun.network/go-perun/channel/test"
	"perun.network/go-perun/client"
	"perun.network/go-perun/log"
	"perun.network/go-perun/wallet"
	wtest "perun.network/go-perun/wallet/test"
	"perun.network/go-perun/watcher/local"
	"perun.network/go-perun/wire"
	"perun.network/go-perun/wire/perunio"
	wiretest "perun.network/go-perun/wire/test"
	"polycry.pt/poly-go/test"
)

// MultiLedgerSetup is the setup of a multi-ledger test.
type MultiLedgerSetup struct {
	Client1, Client2 MultiLedgerClient
	Coordinator      MultiLedgerCoordinator
	Asset1, Asset2   multi.Asset
	InitBalances     channel.Balances
	UpdateBalances1  channel.Balances
	UpdateBalances2  channel.Balances
	BalanceDelta     channel.Bal // Delta the final balances can be off due to gas costs for example.
}

// SetupMultiLedgerTest sets up a multi-ledger test.
func SetupMultiLedgerTest(t *testing.T) MultiLedgerSetup {
	t.Helper()
	rng := test.Prng(t)

	// Setup ledgers.
	l1 := NewMockBackend(rng, "1337")
	l2 := NewMockBackend(rng, "1338")

	// Setup message bus.
	bus := wire.NewLocalBus()

	// Setup clients.
	c1 := setupClient(t, rng, l1, l2, bus, channel.TestBackendID)
	c2 := setupClient(t, rng, l1, l2, bus, channel.TestBackendID)
	coord := setupCoordinator(t, rng, l1, l2, bus, channel.TestBackendID)

	// Define assets.
	a1 := NewMultiLedgerAsset(l1.ID(), chtest.NewRandomAsset(rng, channel.TestBackendID))
	a2 := NewMultiLedgerAsset(l2.ID(), chtest.NewRandomAsset(rng, channel.TestBackendID))

	return MultiLedgerSetup{
		Client1:     c1,
		Client2:     c2,
		Coordinator: coord,
		Asset1:      a1,
		Asset2:      a2,
		//nolint:mnd // We allow the balances to be magic numbers.
		InitBalances: channel.Balances{
			{big.NewInt(10), big.NewInt(0)}, // Asset 1.
			{big.NewInt(0), big.NewInt(10)}, // Asset 2.
		},
		//nolint:mnd
		UpdateBalances1: channel.Balances{
			{big.NewInt(5), big.NewInt(5)}, // Asset 1.
			{big.NewInt(3), big.NewInt(7)}, // Asset 2.
		},
		//nolint:mnd
		UpdateBalances2: channel.Balances{
			{big.NewInt(1), big.NewInt(9)}, // Asset 1.
			{big.NewInt(5), big.NewInt(5)}, // Asset 2.
		},
		BalanceDelta: big.NewInt(0), // The MockBackend does not incur gas costs.
	}
}

// MultiLedgerAsset is a multi-ledger asset.
type MultiLedgerAsset struct {
	id    multi.LedgerBackendID
	asset channel.Asset
}

// NewMultiLedgerAsset returns a new multi-ledger asset.
func NewMultiLedgerAsset(id multi.LedgerBackendID, asset channel.Asset) *MultiLedgerAsset {
	return &MultiLedgerAsset{
		id:    id,
		asset: asset,
	}
}

// LedgerBackendID returns the asset's ID.
func (a *MultiLedgerAsset) LedgerBackendID() multi.LedgerBackendID {
	return a.id
}

// Equal returns whether the two assets are equal.
func (a *MultiLedgerAsset) Equal(b channel.Asset) bool {
	bm, ok := b.(*MultiLedgerAsset)
	if !ok {
		return false
	}

	return a.id.LedgerID().MapKey() == bm.id.LedgerID().MapKey() && a.asset.Equal(bm.asset) && a.id.BackendID() == bm.id.BackendID()
}

// Address returns the asset's address.
func (a *MultiLedgerAsset) Address() []byte {
	return a.asset.Address()
}

// LedgerID returns the asset's ledger ID.
func (a *MultiLedgerAsset) LedgerID() multi.LedgerBackendID {
	return a.id
}

// MarshalBinary encodes the asset to its byte representation.
func (a *MultiLedgerAsset) MarshalBinary() ([]byte, error) {
	var buf bytes.Buffer
	err := perunio.Encode(&buf, string(a.id.LedgerID().MapKey()), a.id.BackendID(), a.asset)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// UnmarshalBinary decodes the asset from its byte representation.
func (a *MultiLedgerAsset) UnmarshalBinary(data []byte) error {
	buf := bytes.NewBuffer(data)
	return perunio.Decode(buf, string(a.id.LedgerID().MapKey()), a.id.BackendID(), a.asset)
}

// MultiLedgerClient represents a test client.
type MultiLedgerClient struct {
	*client.Client

	WireAddress                    map[wallet.BackendID]wire.Address
	WalletAddress                  map[wallet.BackendID]wallet.Address
	Events                         chan channel.AdjudicatorEvent
	Adjudicator1, Adjudicator2     channel.Adjudicator
	BalanceReader1, BalanceReader2 BalanceReader
}

// HandleAdjudicatorEvent handles an incoming adjudicator event.
func (c MultiLedgerClient) HandleAdjudicatorEvent(e channel.AdjudicatorEvent) {
	log.Infof("Client %v: Received adjudicator event %T", c.WireAddress, e)
	c.Events <- e
}

func setupClient(
	t *testing.T, rng *rand.Rand,
	l1, l2 *MockBackend, bus wire.Bus,
	bID wallet.BackendID,
) MultiLedgerClient {
	t.Helper()
	require := require.New(t)

	// Setup identity.
	wireAddr := wiretest.NewRandomAddressesMap(rng, 1)

	// Setup wallet and account.
	w := wtest.NewWallet(bID)
	acc := w.NewRandomAccount(rng)

	// Setup funder.
	funder := multi.NewFunder()
	funder.RegisterFunder(l1.ID(), l1.NewFunder(acc.Address()))
	funder.RegisterFunder(l2.ID(), l2.NewFunder(acc.Address()))

	// Setup adjudicator.
	adj := multi.NewAdjudicator()
	adj.RegisterAdjudicator(l1.ID(), l1.NewAdjudicator(acc.Address()))
	adj.RegisterAdjudicator(l2.ID(), l2.NewAdjudicator(acc.Address()))

	// Setup watcher.
	watcher, err := local.NewWatcher(adj)
	require.NoError(err)

	c, err := client.New(
		wireAddr[0],
		bus,
		funder,
		adj,
		map[wallet.BackendID]wallet.Wallet{channel.TestBackendID: w},
		watcher,
	)
	require.NoError(err)

	return MultiLedgerClient{
		Client:         c,
		WireAddress:    wireAddr[0],
		WalletAddress:  map[wallet.BackendID]wallet.Address{channel.TestBackendID: acc.Address()},
		Events:         make(chan channel.AdjudicatorEvent),
		Adjudicator1:   l1.NewAdjudicator(acc.Address()),
		Adjudicator2:   l2.NewAdjudicator(acc.Address()),
		BalanceReader1: l1.NewBalanceReader(acc.Address()),
		BalanceReader2: l2.NewBalanceReader(acc.Address()),
	}
}

type MultiLedgerCoordinator struct {
	WireAddress                map[wallet.BackendID]wire.Address
	WalletAccount              map[wallet.BackendID]wallet.Account
	WalletAddress              map[wallet.BackendID]wallet.Address
	Multicoordinator           *multi.Coordinator
	Coordinator1, Coordinator2 channel.CoordinatorSubscriber
}

func setupCoordinator(
	t *testing.T, rng *rand.Rand,
	l1, l2 *MockBackend, bus wire.Bus,
	bID wallet.BackendID,
) MultiLedgerCoordinator {
	t.Helper()
	// require := require.New(t)

	// Setup identity.
	wireAddr := wiretest.NewRandomAddressesMap(rng, 1)

	// Setup wallet and account.
	w := wtest.NewWallet(bID)
	acc := w.NewRandomAccount(rng)

	coord1 := l1.NewCoordinator(acc.Address())
	coord2 := l2.NewCoordinator(acc.Address())

	coord := multi.NewCoordinator()
	coord.RegisterCoordinator(l1.ID(), coord1)
	coord.RegisterCoordinator(l2.ID(), coord2)

	return MultiLedgerCoordinator{
		WireAddress:      wireAddr[0],
		WalletAccount:    map[wallet.BackendID]wallet.Account{channel.TestBackendID: acc},
		WalletAddress:    map[wallet.BackendID]wallet.Address{channel.TestBackendID: acc.Address()},
		Multicoordinator: coord,
		Coordinator1:     coord1,
		Coordinator2:     coord2,
	}
}

// Sign signs the given adjudicator request for the given backend ID.
func (c *MultiLedgerCoordinator) Sign(req channel.AdjudicatorReq, bid wallet.BackendID) (wallet.Sig, error) {
	coordSig, err := channel.Sign(c.WalletAccount[bid], req.Tx.State, bid)
	return coordSig, err
}

// Coordinate coordinates a multi-ledger channel by dispatching the call to the multi-ledger coordinator.
func (c *MultiLedgerCoordinator) Coordinate(ctx context.Context, req channel.AdjudicatorReq, signedStates []channel.SignedState, coordSigs []wallet.Sig) error {
	return c.Multicoordinator.Coordinate(ctx, req, signedStates, coordSigs)
}
