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
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"perun.network/go-perun/channel"
	"perun.network/go-perun/channel/multi"
	chtest "perun.network/go-perun/channel/test"
	"perun.network/go-perun/client"
	"perun.network/go-perun/wallet"
	"perun.network/go-perun/wire"
	"polycry.pt/poly-go/test"
)

// MultiLedgerVirtualBalances holds the balance schedule for the multi-ledger
// virtual channel tests.
type MultiLedgerVirtualBalances struct {
	// InitBalsAliceHub is [Asset1,Asset2][Alice,Hub] — initial balances for the
	// Alice-Hub parent ledger channel.
	InitBalsAliceHub channel.Balances
	// InitBalsBobHub is [Asset1,Asset2][Bob,Hub] — initial balances for the
	// Bob-Hub parent ledger channel.
	InitBalsBobHub channel.Balances
	// InitBalsVirtual is [Asset1,Asset2][Alice,Bob] — initial virtual channel
	// balances.
	InitBalsVirtual channel.Balances
	// UpdateBalsVirtual is the target allocation after one off-chain update.
	UpdateBalsVirtual channel.Balances
	// FinalBalsAlice is [Asset1,Asset2][Alice,Hub] — the expected final state
	// of the Alice-Hub parent channel after virtual channel settlement.
	FinalBalsAlice channel.Balances
	// FinalBalsBob is [Asset1,Asset2][Bob,Hub] — the expected final state of
	// the Bob-Hub parent channel after virtual channel settlement.
	FinalBalsBob channel.Balances
}

// MultiLedgerVirtualSetup is the injectable test fixture for all three
// multi-ledger virtual channel scenarios.  Concrete backends provide their own
// instance and call the scenario functions.
type MultiLedgerVirtualSetup struct {
	Alice, Bob, Hub    MultiLedgerClient
	Coordinator        MultiLedgerCoordinator
	Asset1, Asset2     multi.Asset
	Balances           MultiLedgerVirtualBalances
	ChallengeDuration  uint64
	BalanceDelta       channel.Bal
	WaitWatcherTimeout time.Duration
	// IsUTXO controls whether parent channel IDs are encoded as auxiliary data
	// in the virtual channel proposal (required for UTXO-based chains).
	IsUTXO bool
}

// SetupMultiLedgerVirtualTest creates a MultiLedgerVirtualSetup backed by two
// in-process MockBackends (l1 and l2).
func SetupMultiLedgerVirtualTest(t *testing.T) MultiLedgerVirtualSetup {
	t.Helper()
	rng := test.Prng(t)

	l1 := NewMockBackend(rng, "1337")
	l2 := NewMockBackend(rng, "1338")
	bus := wire.NewLocalBus()

	alice := setupClient(t, rng, l1, l2, bus, channel.TestBackendID)
	bob := setupClient(t, rng, l1, l2, bus, channel.TestBackendID)
	hub := setupClient(t, rng, l1, l2, bus, channel.TestBackendID)
	coord := setupCoordinator(t, rng, l1, l2, bus, channel.TestBackendID)

	a1 := NewMultiLedgerAsset(l1.ID(), chtest.NewRandomAsset(rng, channel.TestBackendID))
	a2 := NewMultiLedgerAsset(l2.ID(), chtest.NewRandomAsset(rng, channel.TestBackendID))

	return MultiLedgerVirtualSetup{
		Alice:       alice,
		Bob:         bob,
		Hub:         hub,
		Coordinator: coord,
		Asset1:      a1,
		Asset2:      a2,
		//nolint:mnd
		Balances: MultiLedgerVirtualBalances{
			// Alice and Hub each hold 10 of each asset in the Alice-Hub parent.
			InitBalsAliceHub: channel.Balances{
				{big.NewInt(10), big.NewInt(10)},
				{big.NewInt(10), big.NewInt(10)},
			},
			// Bob and Hub each hold 10 of each asset in the Bob-Hub parent.
			InitBalsBobHub: channel.Balances{
				{big.NewInt(10), big.NewInt(10)},
				{big.NewInt(10), big.NewInt(10)},
			},
			// Virtual channel: equal initial split.
			InitBalsVirtual: channel.Balances{
				{big.NewInt(5), big.NewInt(5)},
				{big.NewInt(5), big.NewInt(5)},
			},
			// After one update Alice transfers value to Bob.
			UpdateBalsVirtual: channel.Balances{
				{big.NewInt(2), big.NewInt(8)},
				{big.NewInt(3), big.NewInt(7)},
			},
			// Alice-Hub final: Alice=10-5+update_alice, Hub=10-5+update_bob
			FinalBalsAlice: channel.Balances{
				{big.NewInt(7), big.NewInt(13)},
				{big.NewInt(8), big.NewInt(12)},
			},
			// Bob-Hub final: Bob=10-5+update_bob, Hub=10-5+update_alice
			FinalBalsBob: channel.Balances{
				{big.NewInt(13), big.NewInt(7)},
				{big.NewInt(12), big.NewInt(8)},
			},
		},
		ChallengeDuration:  10,
		BalanceDelta:       big.NewInt(0),
		WaitWatcherTimeout: 100 * time.Millisecond,
		IsUTXO:             true,
	}
}

// mlvChannels holds all channel handles produced by openMultiLedgerVirtualChannels.
type mlvChannels struct {
	chAliceHub, chHubAlice *client.Channel
	chBobHub, chHubBob     *client.Channel
	chAliceBob, chBobAlice *client.Channel
	bID1, bID2             wallet.BackendID
}

// openMultiLedgerVirtualChannels opens Alice-Hub and Bob-Hub multi-ledger
// ledger channels, then an Alice-Bob virtual channel routed through the Hub.
// When withCoord is true the parent ledger channels are created with
// mls.Coordinator's wallet address.
func openMultiLedgerVirtualChannels(
	ctx context.Context,
	t *testing.T,
	mls MultiLedgerVirtualSetup,
	withCoord bool,
) (mlvChannels, chan error) {
	t.Helper()
	require := require.New(t)

	bID1 := wallet.BackendID(mls.Asset1.LedgerBackendID().BackendID())
	bID2 := wallet.BackendID(mls.Asset2.LedgerBackendID().BackendID())

	errs := make(chan error, 10)

	// Hub accepts up to two ledger channel proposals (from Alice and Bob).
	hubChannels := make(chan *client.Channel, 2)
	//nolint:contextcheck
	go mls.Hub.Handle(
		AlwaysAcceptChannelHandler(ctx, mls.Hub.WalletAddress, hubChannels, errs),
		AlwaysAcceptUpdateHandler(ctx, errs),
	)

	// Optional coordinator for parent channel proposals.
	var parentOpts []client.ProposalOpts
	if withCoord {
		parentOpts = append(parentOpts, client.WithCoordinator(mls.Coordinator.WalletAddress))
	}

	// --- Open Alice-Hub ledger channel ---
	partsAliceHub := []map[wallet.BackendID]wire.Address{mls.Alice.WireAddress, mls.Hub.WireAddress}
	allocAliceHub := channel.NewAllocation(2, []wallet.BackendID{bID1, bID2}, mls.Asset1, mls.Asset2)
	allocAliceHub.Balances = mls.Balances.InitBalsAliceHub
	propAliceHub, err := client.NewLedgerChannelProposal(
		mls.ChallengeDuration, mls.Alice.WalletAddress, allocAliceHub, partsAliceHub, parentOpts...,
	)
	require.NoError(err, "creating Alice-Hub proposal")
	chAliceHub, err := mls.Alice.ProposeChannel(ctx, propAliceHub)
	require.NoError(err, "opening Alice-Hub channel")
	var chHubAlice *client.Channel
	select {
	case chHubAlice = <-hubChannels:
	case e := <-errs:
		t.Fatalf("hub: Alice-Hub accept error: %v", e)
	}

	// --- Open Bob-Hub ledger channel ---
	partsBobHub := []map[wallet.BackendID]wire.Address{mls.Bob.WireAddress, mls.Hub.WireAddress}
	allocBobHub := channel.NewAllocation(2, []wallet.BackendID{bID1, bID2}, mls.Asset1, mls.Asset2)
	allocBobHub.Balances = mls.Balances.InitBalsBobHub
	propBobHub, err := client.NewLedgerChannelProposal(
		mls.ChallengeDuration, mls.Bob.WalletAddress, allocBobHub, partsBobHub, parentOpts...,
	)
	require.NoError(err, "creating Bob-Hub proposal")
	chBobHub, err := mls.Bob.ProposeChannel(ctx, propBobHub)
	require.NoError(err, "opening Bob-Hub channel")
	var chHubBob *client.Channel
	select {
	case chHubBob = <-hubChannels:
	case e := <-errs:
		t.Fatalf("hub: Bob-Hub accept error: %v", e)
	}

	// --- Virtual channel proposal handlers ---
	// Bob accepts VirtualChannelProposalMsg.
	bobVCChannels := make(chan *client.Channel, 1)
	//nolint:contextcheck
	go mls.Bob.Handle(
		client.ProposalHandlerFunc(func(cp client.ChannelProposal, pr *client.ProposalResponder) {
			switch cp := cp.(type) {
			case *client.VirtualChannelProposalMsg:
				ch, err := pr.Accept(ctx, cp.Accept(mls.Bob.WalletAddress))
				if err != nil {
					errs <- errors.WithMessage(err, "Bob: accepting virtual channel proposal")
				}
				if ch != nil {
					bobVCChannels <- ch
				}
			default:
				errs <- errors.Errorf("Bob: unexpected proposal type: %T", cp)
			}
		}),
		AlwaysAcceptUpdateHandler(ctx, errs),
	)
	// Alice's handle loop must be running before she proposes: the hub may send
	// her a sub-channel message as part of the virtual channel protocol.
	//nolint:contextcheck
	go mls.Alice.Handle(
		client.ProposalHandlerFunc(func(cp client.ChannelProposal, pr *client.ProposalResponder) {
			switch cp := cp.(type) {
			case *client.VirtualChannelProposalMsg:
				ch, err := pr.Accept(ctx, cp.Accept(mls.Bob.WalletAddress))
				if err != nil {
					errs <- errors.WithMessage(err, "Alice: accepting virtual channel proposal")
				}
				// Alice's ProposeChannel returns chAliceBob directly; discard
				// any channel received via this handler.
				_ = ch
			default:
				errs <- errors.Errorf("Alice: unexpected proposal type: %T", cp)
			}
		}),
		AlwaysAcceptUpdateHandler(ctx, errs),
	)

	// --- Open Alice-Bob virtual channel ---
	allocVirtual := channel.NewAllocation(2, []wallet.BackendID{bID1, bID2}, mls.Asset1, mls.Asset2)
	allocVirtual.Balances = mls.Balances.InitBalsVirtual

	// indexMapAlice: virtual[0]=Alice → Alice-Hub[0]=Alice
	//                virtual[1]=Bob   → Alice-Hub[1]=Hub  (Hub proxies Bob)
	// indexMapBob:   virtual[0]=Alice → Bob-Hub[1]=Hub    (Hub proxies Alice)
	//                virtual[1]=Bob   → Bob-Hub[0]=Bob
	indexMapAlice := []channel.Index{0, 1}
	indexMapBob := []channel.Index{1, 0}

	virtualPeers := []map[wallet.BackendID]wire.Address{
		mls.Alice.WireAddress,
		mls.Bob.WireAddress,
	}
	parentIDs := []channel.ID{chAliceHub.ID(), chBobHub.ID()}
	indexMaps := [][]channel.Index{indexMapAlice, indexMapBob}

	var vcp *client.VirtualChannelProposalMsg
	if mls.IsUTXO {
		var aux channel.Aux
		aliceHubID := chAliceHub.ID()
		bobHubID := chBobHub.ID()
		copy(aux[:channel.IDLen], aliceHubID[:])
		copy(aux[channel.IDLen:], bobHubID[:])
		parentOpts = append(parentOpts, client.WithAux(aux))
	}
	vcp, err = client.NewVirtualChannelProposal(
		mls.ChallengeDuration, mls.Alice.WalletAddress, allocVirtual,
		virtualPeers, parentIDs, indexMaps, parentOpts...,
	)

	require.NoError(err, "creating virtual channel proposal")

	chAliceBob, err := mls.Alice.ProposeChannel(ctx, vcp)
	require.NoError(err, "opening Alice-Bob virtual channel")
	var chBobAlice *client.Channel
	select {
	case chBobAlice = <-bobVCChannels:
	case e := <-errs:
		t.Fatalf("virtual channel: Bob accept error: %v", e)
	}

	// One off-chain update.
	err = chAliceBob.Update(ctx, func(s *channel.State) {
		s.Balances = mls.Balances.UpdateBalsVirtual
	})
	require.NoError(err, "updating virtual channel")

	return mlvChannels{
		chAliceHub: chAliceHub,
		chHubAlice: chHubAlice,
		chBobHub:   chBobHub,
		chHubBob:   chHubBob,
		chAliceBob: chAliceBob,
		chBobAlice: chBobAlice,
		bID1:       bID1,
		bID2:       bID2,
	}, errs
}
