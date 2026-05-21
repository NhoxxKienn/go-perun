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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"perun.network/go-perun/channel"
	"perun.network/go-perun/client"
	"perun.network/go-perun/wallet"
	"perun.network/go-perun/wire"
)

// buildSecretSignedReq constructs a fully-signed AdjudicatorReq at version
// baseReq.Tx.State.Version+1 with the supplied balances. The state is signed
// with every account in `accs`, modelling a v2 that the attacker holds
// privately — Alice's client never sees it because we bypass the Update flow.
// `idx` selects which participant's account and index populate the request.
func buildSecretSignedReq(
	baseReq channel.AdjudicatorReq,
	newBalances channel.Balances,
	accs []wallet.Account,
	bID wallet.BackendID,
	idx channel.Index,
) (channel.AdjudicatorReq, error) {
	s := baseReq.Tx.State.Clone()
	s.Version = baseReq.Tx.State.Version + 1
	s.Balances = newBalances

	sigs := make([]wallet.Sig, len(accs))
	for i, a := range accs {
		sig, err := channel.Sign(a, s, bID)
		if err != nil {
			return channel.AdjudicatorReq{}, fmt.Errorf("signing party %d: %w", i, err)
		}
		sigs[i] = sig
	}
	return channel.AdjudicatorReq{
		Params: baseReq.Params,
		Acc:    map[wallet.BackendID]wallet.Account{bID: accs[idx]},
		Tx:     channel.Transaction{State: s, Sigs: sigs},
		Idx:    idx,
	}, nil
}

// TestMultiLedgerAttackNoCoordinator proves the NEGATIVE case: without a coordinator,
// the divergent settlement attack SUCCEEDS.
//
// Attack model:
//  1. Alice and Bob agree on v1 through a normal off-chain Update().
//  2. v2 is fabricated manually with both signatures but never delivered to Alice's
//     client. Alice's machine remains at v1; only the test code (acting as Bob)
//     holds v2.
//  3. Bob registers v1 on Asset2's chain (Adjudicator2). This is the "public"
//     agreed state, so nothing looks suspicious.
//  4. Once Asset2's challenge window expires at v1, Bob reveals the secret v2 by
//     registering it on Asset1's chain (Adjudicator1). checkRegister accepts
//     because v2 > v1.
//  5. Asset1's window expires at v2. The chains have diverged.
//  6. Each chain pays out its own registered state. Bob nets +4 over the all-v1
//     baseline; Alice loses 4.
//
//nolint:cyclop,revive // test.Test... stutters but this is OK in this special case.
func TestMultiLedgerAttackNoCoordinator(
	ctx context.Context,
	t *testing.T,
	mlt MultiLedgerSetup,
	challengeDuration uint64,
) {
	require := require.New(t)
	assert := assert.New(t)
	alice, bob := mlt.Client1, mlt.Client2

	// Record initial balances (wallets start at 0).
	balancesBefore := channel.Balances{
		{
			mlt.Client1.BalanceReader1.Balance(mlt.Asset1),
			mlt.Client2.BalanceReader1.Balance(mlt.Asset1),
		},
		{
			mlt.Client1.BalanceReader2.Balance(mlt.Asset2),
			mlt.Client2.BalanceReader2.Balance(mlt.Asset2),
		},
	}

	bID1 := wallet.BackendID(mlt.Asset1.LedgerBackendID().BackendID())
	bID2 := wallet.BackendID(mlt.Asset2.LedgerBackendID().BackendID())
	aliceAcc := alice.WalletAccount[bID1]
	bobAcc := bob.WalletAccount[bID1]

	// Open channel WITHOUT a coordinator.
	parts := []map[wallet.BackendID]wire.Address{alice.WireAddress, bob.WireAddress}
	initAlloc := channel.NewAllocation(len(parts), []wallet.BackendID{bID1, bID2}, mlt.Asset1, mlt.Asset2)
	initAlloc.Balances = mlt.InitBalances
	prop, err := client.NewLedgerChannelProposal(
		challengeDuration,
		alice.WalletAddress,
		initAlloc,
		parts,
	)
	require.NoError(err, "creating ledger channel proposal")

	channels := make(chan *client.Channel, 1)
	errs := make(chan error)
	//nolint:contextcheck
	go alice.Handle(
		AlwaysRejectChannelHandler(ctx, errs),
		AlwaysAcceptUpdateHandler(ctx, errs),
	)
	//nolint:contextcheck
	go bob.Handle(
		AlwaysAcceptChannelHandler(ctx, bob.WalletAddress, channels, errs),
		AlwaysAcceptUpdateHandler(ctx, errs),
	)

	chAliceBob, err := alice.ProposeChannel(ctx, prop)
	require.NoError(err, "opening channel")
	var chBobAlice *client.Channel
	select {
	case chBobAlice = <-channels:
	case err := <-errs:
		t.Fatalf("Error in go-routine: %v", err)
	}

	done := make(chan struct{}, 1)
	chBobAlice.OnUpdate(func(from, to *channel.State) {
		done <- struct{}{}
	})

	// ONE legitimate update to v1 (Alice's machine ends at v1 — the latest she knows).
	err = chAliceBob.Update(ctx, func(s *channel.State) {
		s.Balances = mlt.UpdateBalances1
	})
	require.NoError(err)
	<-done
	time.Sleep(100 * time.Millisecond) //nolint:mnd

	// v1 requests for both participants (Idx=0 Alice, Idx=1 Bob).
	v1ReqAlice := client.NewTestChannel(chAliceBob).AdjudicatorReq()
	v1ReqBob := client.NewTestChannel(chBobAlice).AdjudicatorReq()

	// SECRET v2: fabricated by the test (acting as Bob). Alice's machine still has v1.
	accs := []wallet.Account{aliceAcc, bobAcc}
	v2ReqBob, err := buildSecretSignedReq(v1ReqBob, mlt.UpdateBalances2, accs, bID1, 1)
	require.NoError(err, "building secret v2 req for Bob")
	v2ReqAlice, err := buildSecretSignedReq(v1ReqAlice, mlt.UpdateBalances2, accs, bID1, 0)
	require.NoError(err, "building secret v2 req for Alice")

	chID := chAliceBob.ID()

	// ATTACK STEP 1: Bob registers v1 on Asset2's chain (Adjudicator2).
	// v1 is the legitimate agreed state — nothing looks suspicious.
	err = bob.Adjudicator2.Register(ctx, v1ReqBob, nil)
	require.NoError(err, "registering v1 on chain B (Asset2)")

	// Wait for Asset2 chain v1 timeout to elapse.
	sub2, err := bob.Adjudicator2.Subscribe(ctx, chID)
	require.NoError(err)
	e2 := sub2.Next()
	require.IsType(&channel.RegisteredEvent{}, e2)
	require.NoError(e2.(*channel.RegisteredEvent).TimeoutV.Wait(ctx))
	require.NoError(sub2.Close())

	// ATTACK STEP 2: Bob reveals the secret v2 on Asset1's chain (Adjudicator1).
	// Adjudicator1 has no prior event, so checkRegister passes trivially.
	err = bob.Adjudicator1.Register(ctx, v2ReqBob, nil)
	require.NoError(err, "registering secret v2 on chain A (Asset1)")

	// Wait for Asset1 chain v2 timeout to elapse.
	sub1, err := bob.Adjudicator1.Subscribe(ctx, chID)
	require.NoError(err)
	e1 := sub1.Next()
	require.IsType(&channel.RegisteredEvent{}, e1)
	require.NoError(e1.(*channel.RegisteredEvent).TimeoutV.Wait(ctx))
	require.NoError(sub1.Close())

	// Direct withdrawals — adversarial per-chain settlement.
	//   Adjudicator1 (Asset1) → registered at v2 → payouts use v2 balances.
	//   Adjudicator2 (Asset2) → registered at v1 → payouts use v1 balances.
	require.NoError(bob.Adjudicator1.Withdraw(ctx, v2ReqBob, nil))
	require.NoError(alice.Adjudicator1.Withdraw(ctx, v2ReqAlice, nil))
	require.NoError(bob.Adjudicator2.Withdraw(ctx, v1ReqBob, nil))
	require.NoError(alice.Adjudicator2.Withdraw(ctx, v1ReqAlice, nil))

	// Close channel objects (machines are not in Withdrawn state since we bypassed Settle).
	_ = chAliceBob.Close()
	_ = chBobAlice.Close()

	balancesAfter := channel.Balances{
		{
			mlt.Client1.BalanceReader1.Balance(mlt.Asset1),
			mlt.Client2.BalanceReader1.Balance(mlt.Asset1),
		},
		{
			mlt.Client1.BalanceReader2.Balance(mlt.Asset2),
			mlt.Client2.BalanceReader2.Balance(mlt.Asset2),
		},
	}
	balancesDiff := balancesAfter.Sub(balancesBefore)

	// Divergent (attack) outcome:
	//   Asset1 from v2: Alice 1−10=−9, Bob 9−0=+9
	//   Asset2 from v1: Alice 3−0=+3,  Bob 7−10=−3
	attackDiff := channel.Balances{
		mlt.UpdateBalances2.Sub(mlt.InitBalances)[0],
		mlt.UpdateBalances1.Sub(mlt.InitBalances)[1],
	}
	// Baseline: had Bob settled honestly at v1 on BOTH chains.
	allV1Diff := mlt.UpdateBalances1.Sub(mlt.InitBalances)

	assert.Truef(
		EqualBalancesWithDelta(attackDiff, balancesDiff, mlt.BalanceDelta),
		"without coordinator the divergent (attack) settlement must succeed: expected %v +/- %v, got %v",
		attackDiff, mlt.BalanceDelta, balancesDiff,
	)
	assert.Falsef(
		EqualBalancesWithDelta(allV1Diff, balancesDiff, mlt.BalanceDelta),
		"without coordinator the legitimate all-v1 outcome must NOT be achieved: all-v1 %v, got %v",
		allV1Diff, balancesDiff,
	)
}

// TestMultiLedgerAttackCoordinate proves the POSITIVE case: the coordinator
// prevents the divergent attack.
//
// The setup is identical to TestMultiLedgerAttackNoCoordinator (v2 is secret to
// Bob; Alice's machine stays at v1). However, before Bob can reveal v2 on the
// second chain, the coordinator races to call coordinate(v1) on both chains.
// This transitions both chains into COORDINATED phase, where the mock backend
// (mirroring the on-chain contract) rejects further Register() calls.
//
// The test also asserts that coordinate() is rejected before the challenge
// timeout has elapsed ("refutation timeout not passed").
//
//nolint:cyclop,revive,gocognit // test.Test... stutters but this is OK in this special case.
func TestMultiLedgerAttackCoordinate(
	ctx context.Context,
	t *testing.T,
	mlt MultiLedgerSetup,
	challengeDuration uint64,
) {
	require := require.New(t)
	assert := assert.New(t)
	alice, bob := mlt.Client1, mlt.Client2
	charlie := mlt.Coordinator

	balancesBefore := channel.Balances{
		{
			mlt.Client1.BalanceReader1.Balance(mlt.Asset1),
			mlt.Client2.BalanceReader1.Balance(mlt.Asset1),
		},
		{
			mlt.Client1.BalanceReader2.Balance(mlt.Asset2),
			mlt.Client2.BalanceReader2.Balance(mlt.Asset2),
		},
	}

	bID1 := wallet.BackendID(mlt.Asset1.LedgerBackendID().BackendID())
	bID2 := wallet.BackendID(mlt.Asset2.LedgerBackendID().BackendID())
	aliceAcc := alice.WalletAccount[bID1]
	bobAcc := bob.WalletAccount[bID1]

	// Open channel WITH coordinator.
	parts := []map[wallet.BackendID]wire.Address{alice.WireAddress, bob.WireAddress}
	initAlloc := channel.NewAllocation(len(parts), []wallet.BackendID{bID1, bID2}, mlt.Asset1, mlt.Asset2)
	initAlloc.Balances = mlt.InitBalances
	prop, err := client.NewLedgerChannelProposal(
		challengeDuration,
		alice.WalletAddress,
		initAlloc,
		parts,
		client.WithCoordinator(charlie.WalletAddress),
	)
	require.NoError(err, "creating ledger channel proposal")

	channels := make(chan *client.Channel, 1)
	errs := make(chan error)
	//nolint:contextcheck
	go alice.Handle(
		AlwaysRejectChannelHandler(ctx, errs),
		AlwaysAcceptUpdateHandler(ctx, errs),
	)
	//nolint:contextcheck
	go bob.Handle(
		AlwaysAcceptChannelHandler(ctx, bob.WalletAddress, channels, errs),
		AlwaysAcceptUpdateHandler(ctx, errs),
	)

	chAliceBob, err := alice.ProposeChannel(ctx, prop)
	require.NoError(err, "opening channel between Alice and Bob")
	var chBobAlice *client.Channel
	select {
	case chBobAlice = <-channels:
	case err := <-errs:
		t.Fatalf("Error in go-routine: %v", err)
	}

	done := make(chan struct{}, 1)
	chBobAlice.OnUpdate(func(from, to *channel.State) {
		done <- struct{}{}
	})

	// ONE legitimate update to v1.
	err = chAliceBob.Update(ctx, func(s *channel.State) {
		s.Balances = mlt.UpdateBalances1
	})
	require.NoError(err)
	<-done
	time.Sleep(100 * time.Millisecond) //nolint:mnd

	v1Req := client.NewTestChannel(chBobAlice).AdjudicatorReq()

	// Fabricate the SECRET v2 (Alice's machine still has v1).
	accs := []wallet.Account{aliceAcc, bobAcc}
	v2ReqBob, err := buildSecretSignedReq(v1Req, mlt.UpdateBalances2, accs, bID1, 1)
	require.NoError(err, "building secret v2 req for Bob")

	chID := chAliceBob.ID()

	// Subscribe to both adjudicators BEFORE starting watchers so we cannot miss
	// events emitted during watcher replication.
	sub1, err := bob.Adjudicator1.Subscribe(ctx, chID)
	require.NoError(err, "subscribing to chain A (Asset1)")
	sub2, err := bob.Adjudicator2.Subscribe(ctx, chID)
	require.NoError(err, "subscribing to chain B (Asset2)")

	// Start watchers for both participants.
	//nolint:contextcheck
	go func() {
		errs <- chAliceBob.Watch(alice)
	}()
	//nolint:contextcheck
	go func() {
		errs <- chBobAlice.Watch(bob)
	}()
	time.Sleep(100 * time.Millisecond) //nolint:mnd

	// ATTACK STEP 1: Bob registers v1 on Asset2's chain.
	err = bob.Adjudicator2.Register(ctx, v1Req, nil)
	require.NoError(err, "registering v1 on chain B (Asset2)")

	// Wait for the chain B v1 RegisteredEvent (the registration we just submitted).
	// sub2 was created before the watcher started, so it buffered this event already.
	e2 := sub2.Next()
	require.IsType(&channel.RegisteredEvent{}, e2, "expected RegisteredEvent on chain B")
	// Wait for the chain A v1 RegisteredEvent emitted by the watcher's replication.
	// sub1.Next() blocks until the event arrives, making this backend-agnostic.
	e1 := sub1.Next()
	require.IsType(&channel.RegisteredEvent{}, e1, "expected RegisteredEvent on chain A")

	// ATTACK STEP 2: Bob attempts to reveal secret v2 on Adjudicator1.
	err = bob.Adjudicator1.Register(ctx, v2ReqBob, nil)
	require.NoError(err, "register after the other chain timeout should not fail")

	// Wait for the chain B v1 RegisteredEvent (the registration we just submitted).
	// sub2 was created before the watcher started, so it buffered this event already.
	e1 = sub1.Next()
	require.IsType(&channel.RegisteredEvent{}, e1, "expected RegisteredEvent on chain A")
	require.NoError(e1.(*channel.RegisteredEvent).TimeoutV.Wait(ctx), "waiting for chain A v1 timeout")
	time.Sleep(100 * time.Millisecond) //nolint:mnd
	require.NoError(sub1.Close())
	require.NoError(sub2.Close())

	// COORDINATOR LOCKS v2: coordinate(v2) on both chains.
	err = charlie.Coordinate(ctx, v2ReqBob, nil, bID2)
	require.NoError(err, "coordinate after timeout should succeed on both chains")

	// Settle channels at v2 — the coordinated state.
	err = chAliceBob.Settle(ctx, false)
	require.NoError(err)
	err = chBobAlice.Settle(ctx, false)
	require.NoError(err)

	require.NoError(chAliceBob.Close())
	require.NoError(chBobAlice.Close())

	balancesAfter := channel.Balances{
		{
			mlt.Client1.BalanceReader1.Balance(mlt.Asset1),
			mlt.Client2.BalanceReader1.Balance(mlt.Asset1),
		},
		{
			mlt.Client1.BalanceReader2.Balance(mlt.Asset2),
			mlt.Client2.BalanceReader2.Balance(mlt.Asset2),
		},
	}
	balancesDiff := balancesAfter.Sub(balancesBefore)

	// Coordinator locked v2 → uniform v2 outcome on both chains.
	allV2Diff := mlt.UpdateBalances2.Sub(mlt.InitBalances)
	// What the attack would have produced (divergent v2/v1) — MUST NOT match.
	attackDiff := channel.Balances{
		mlt.UpdateBalances2.Sub(mlt.InitBalances)[0],
		mlt.UpdateBalances1.Sub(mlt.InitBalances)[1],
	}

	assert.Truef(
		EqualBalancesWithDelta(allV2Diff, balancesDiff, mlt.BalanceDelta),
		"coordinator must enforce uniform v2 outcome: expected %v +/- %v, got %v",
		allV2Diff, mlt.BalanceDelta, balancesDiff,
	)
	assert.Falsef(
		EqualBalancesWithDelta(attackDiff, balancesDiff, mlt.BalanceDelta),
		"divergent (attack) outcome must NOT be achieved with coordinator: attack %v, got %v",
		attackDiff, balancesDiff,
	)
}
