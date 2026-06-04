# Multi-Ledger Divergent-Settlement Attack — PoC Implementation Guide

This document describes everything required to build a standalone proof-of-concept (PoC)
of the multi-ledger divergent-settlement attack against a Perun state channel, and the
coordinator mechanism that prevents it.  The PoC targets two independent Hardhat EVM
chains and uses this `perun-eth-backend` repository as the client library.

---

## Table of Contents

1. [Background](#1-background)
2. [Attack Model](#2-attack-model)
3. [Coordinator Protection Model](#3-coordinator-protection-model)
4. [Prerequisites](#4-prerequisites)
5. [Project Layout](#5-project-layout)
6. [Chain Setup (Hardhat)](#6-chain-setup-hardhat)
7. [Contract ABI Reference](#7-contract-abi-reference)
8. [Go Module & Dependencies](#8-go-module--dependencies)
9. [Go Implementation](#9-go-implementation)
   - 9.1 [Chain helpers](#91-chain-helpers)
   - 9.2 [Participant setup](#92-participant-setup)
   - 9.3 [Coordinator setup](#93-coordinator-setup)
   - 9.4 [Helper: buildSecretSignedReq](#94-helper-buildsecretreq)
   - 9.5 [Attack test — no coordinator](#95-attack-test--no-coordinator)
   - 9.6 [Coordinator test — divergence prevented](#96-coordinator-test--divergence-prevented)
10. [Running the PoC](#10-running-the-poc)
11. [Expected Output](#11-expected-output)
12. [Key Invariants Summary](#12-key-invariants-summary)
13. [Real Coordinator Service (`cross-chain-coordinator`)](#13-real-coordinator-service-cross-chain-coordinator)
    - 13.1 [Layout](#131-layout)
    - 13.2 [Configuration](#132-configuration-devnet_configyaml)
    - 13.3 [Running the service](#133-running-the-service)
    - 13.4 [Client-side wiring](#134-client-side-wiring-replaces-93-for-end-to-end-poc)
    - 13.5 [Event-driven coordination flow](#135-event-driven-coordination-flow)
    - 13.6 [Why the PoC keeps the inline version](#136-why-the-poc-keeps-the-inline-simplified-version)
    - 13.7 [Known gaps and TODOs](#137-known-gaps-and-todos)

---

## 1. Background

A **multi-ledger channel** is a Perun payment channel whose assets live on two separate
EVM chains.  Each chain hosts its own `Adjudicator.sol` and `AssetHolderETH.sol`.

```
Chain A (port 8545, chainID 1337)     Chain B (port 8546, chainID 1338)
─────────────────────────────────     ─────────────────────────────────
Adjudicator_A  AssetHolderETH_A       Adjudicator_B  AssetHolderETH_B
      │  Asset1 (ETH on Chain A)             │  Asset2 (ETH on Chain B)
      └──────────── multi-ledger channel (logical) ────────────────────┘
                        Alice ←——————→ Bob
```

Both chains maintain completely independent dispute windows.  This independence is the
root cause of the attack.

---

## 2. Attack Model

### Balance setup

| Version                                 | Alice A1 | Bob A1 | Alice A2 | Bob A2 | Bob total |
| --------------------------------------- | -------- | ------ | -------- | ------ | --------- |
| v0 (init)                               | 8 ETH    | 2 ETH  | 2 ETH    | 8 ETH  | 10        |
| v1 (agreed)                             | 5        | 5      | 3        | 7      | 12        |
| v2 (secret)                             | 1        | 9      | 5        | 5      | 14        |
| **Attack outcome** (v2 on A1, v1 on A2) | **1**    | **9**  | **3**    | **7**  | **16**    |

Bob profits **+4** above the honest v1 baseline by registering different versions
on each chain.

### Attack steps

```
t0  Alice & Bob agree on v1 off-chain through the normal Update flow.
    Bob secretly retains both signatures for a fabricated v2 (Alice never sees it).
    Alice's local machine remains at v1.

t1  [ATTACK 1] Bob registers v1 on Chain B (Adjudicator_B).
    Chain B dispute window opens at v1.

t2  Challenge window expires on Chain B (v1 frozen there).

t3  [ATTACK 2] Bob registers v2 on Chain A (Adjudicator_A).
    Chain A had no prior registration → v2 accepted (v2 > nothing).
    Chain A dispute window opens at v2.

t4  Challenge window expires on Chain A (v2 frozen there).

t5  Bob withdraws from Chain A at v2 → gets 9 ETH (Asset1).
    Bob withdraws from Chain B at v1 → gets 7 ETH (Asset2).
    Alice receives 1 + 3 = 4 ETH instead of 5 + 3 = 8 ETH.
```

### Why it works without a coordinator

The two `Adjudicator` contracts have no communication channel.  `registerSingle`
only checks that the new version exceeds whatever is registered **on that chain**.
Chain A has no registration when Bob submits v2, so it is accepted unconditionally.
There is no global synchronisation enforcing a single canonical version.

---

## 3. Coordinator Protection Model

A **trusted third-party (TTP) coordinator** C is encoded in the channel parameters
at open time.  The on-chain phase lifecycle becomes:

```
register()   ──► DISPUTE      (any participant, per chain, challenge window opens)
[timeout]    ──► FROZEN       (window expires; registerSingle now rejects the tx
                               because block.timestamp ≥ timeout, "refutation timeout passed")
coordinate() ──► COORDINATED  (coordinator C only, after dispute timeout has passed,
                               selects the highest-version canonical state)
conclude()   ──► CONCLUDED    (any participant, funds released)
```

The coordinator's protection mechanism:

1. Subscribe to on-chain events for the channel on **every** registered chain.
2. Wait until each chain delivers a `RegisteredEvent` whose timeout has elapsed.
3. Pick the **highest version** seen across all chains as the canonical state.
4. Call `multi.Coordinator.Coordinate(canonical)` — fans out concurrently to all chains.
5. Each chain's `coordinateSingle` accepts the canonical version (≥ its stored version)
   and transitions to COORDINATED.  Once COORDINATED, `registerSingle` rejects any
   further state submission ("incorrect phase").

The key guarantee: **both chains are locked to the same version before any withdrawal
can happen**.  Even if Bob successfully registered v2 on Chain A while Chain B stayed at
v1, the coordinator will coordinate v2 on both chains so the final payout is uniform.

Key contract invariants (from `Adjudicator.sol` / `MultiLedger.sol`):

| Check                                                          | Solidity revert message             |
| -------------------------------------------------------------- | ----------------------------------- |
| `coordinate()` requires prior `register()`                     | `"not registered"`                  |
| `coordinate()` requires `block.timestamp >= dispute.timeout`   | `"refutation timeout not passed"`   |
| `coordinate()` requires valid coordinator ECDSA sig over state | `"invalid coordinator signature"`   |
| `coordinate()` requires state version ≥ stored version         | `"invalid version"`                 |
| `register()` in COORDINATED phase is rejected                  | `"incorrect phase"`                 |
| `conclude()` without coordination on multi-ledger channels     | `"coordinated settlement required"` |

---

## 4. Prerequisites

### System tools

| Tool    | Version      | Purpose                                                     |
| ------- | ------------ | ----------------------------------------------------------- |
| Go      | ≥ 1.24       | PoC implementation (`go.mod` in this repo uses `go 1.24.0`) |
| Node.js | ≥ 18         | Hardhat                                                     |
| npm     | any          | Hardhat packages                                            |
| Hardhat | ≥ 2.22       | Two local EVM chains                                        |
| solc    | ≥ 0.8.15     | Compile contracts (`pragma solidity ^0.8.15`)               |
| abigen  | same as geth | Generate Go bindings from contract ABI                      |

### Go dependencies (see Section 8 for exact `go.mod`)

```
github.com/perun-network/perun-eth-backend   (this repo — Ethereum adjudicator/funder/wallet)
perun.network/go-perun                       (framework — replace directive points to fork)
github.com/ethereum/go-ethereum v1.17.2      (ethclient, keystore, accounts)
github.com/miguelmota/go-ethereum-hdwallet v0.1.1  (HD wallet for Hardhat mnemonic key derivation)
github.com/stretchr/testify                  (assertions)
polycry.pt/poly-go                           (sync primitives)
```

### Contracts

Both chains must run the **coordinator-enabled** `Adjudicator.sol` from this repository.
Source: `bindings/contracts/contracts/Adjudicator.sol`.  Pre-compiled ABI/bytecode is
available in `bindings/adjudicator/`.  You can regenerate Go bindings with
`cd bindings && ./generate.sh`.

---

## 5. Project Layout

```
multiledger-poc/
├── hardhat/
│   ├── hardhat.config.js
│   ├── contracts/
│   │   └── (copy from perun-eth-backend/bindings/contracts/contracts/)
│   ├── scripts/
│   │   └── deploy.js
│   └── addresses.json          # filled by deploy.js (git-ignored)
│
├── poc/
│   ├── helpers.go              # ChainConfig, helpers, key derivation
│   ├── participant.go          # Participant type (client + per-chain handles)
│   ├── coordinator.go          # standalone CoordinatorService
│   ├── attack_test.go          # TestAttackNoCoordinator
│   └── coordinate_test.go     # TestAttackWithCoordinator
│
├── go.mod
└── go.sum
```

---

## 6. Chain Setup (Hardhat)

### `hardhat/hardhat.config.js`

```javascript
require("@nomicfoundation/hardhat-toolbox");

module.exports = {
  solidity: "0.8.26",
  networks: {
    chainA: {
      url: "http://127.0.0.1:8545",
      chainId: 1337,
      accounts: {
        mnemonic: "test test test test test test test test test test test junk",
        count: 5,  // [0]=deployer, [1]=alice, [2]=bob, [3]=charlie(coord)
      },
    },
    chainB: {
      url: "http://127.0.0.1:8546",
      chainId: 1338,
      accounts: {
        mnemonic: "test test test test test test test test test test test junk",
        count: 5,
      },
    },
  },
};
```

Start both chains in separate terminals:

```bash
npx hardhat node --port 8545   # Chain A (chainID 1337)
npx hardhat node --port 8546   # Chain B (chainID 1338)
```

### `hardhat/scripts/deploy.js`

```javascript
const { ethers, network } = require("hardhat");
const fs = require("fs");

async function main() {
  const Adj   = await ethers.getContractFactory("Adjudicator");
  const Asset = await ethers.getContractFactory("AssetHolderETH");

  const adj   = await Adj.deploy();   await adj.waitForDeployment();
  const asset = await Asset.deploy(await adj.getAddress()); await asset.waitForDeployment();

  const out = { adjudicator: await adj.getAddress(), assetHolder: await asset.getAddress() };
  console.log(network.name, out);

  // Append to addresses.json keyed by network name.
  const path = "addresses.json";
  const prev = fs.existsSync(path) ? JSON.parse(fs.readFileSync(path)) : {};
  prev[network.name] = out;
  fs.writeFileSync(path, JSON.stringify(prev, null, 2));
}
main().catch(console.error);
```

```bash
cd hardhat
npx hardhat run scripts/deploy.js --network chainA
npx hardhat run scripts/deploy.js --network chainB
# → addresses.json updated with chainA and chainB entries
```

### Advancing time past the challenge window

**Critical**: `Adjudicator.sol` uses `block.timestamp` (not block number) for dispute
timeouts.  `challengeDuration` in `channel.Params` is measured in **seconds**.  To
advance past the timeout in tests use the standard EVM JSON-RPC methods:

```go
// AdvanceTime mines one block after increasing the node's clock by `seconds`.
func AdvanceTime(ctx context.Context, rpcURL string, seconds uint64) error {
    c, err := rpc.DialContext(ctx, rpcURL)
    if err != nil {
        return err
    }
    defer c.Close()
    if err := c.CallContext(ctx, nil, "evm_increaseTime",
        hexutil.EncodeUint64(seconds)); err != nil {
        return fmt.Errorf("evm_increaseTime: %w", err)
    }
    return c.CallContext(ctx, nil, "evm_mine")
}
```

In the attack tests, after registering a dispute with `challengeDuration = 15` seconds,
call `AdvanceTime(ctx, rpcURL, 16)` to expire the window.

---

## 7. Contract ABI Reference

The PoC uses Go bindings generated from the contracts.  The relevant on-chain
function signatures (from `Adjudicator.sol`) are:

```solidity
// SignedState bundles params, state, and all participant sigs.
struct SignedState {
    Channel.Params params;
    Channel.State  state;
    bytes[]        sigs;
}

// Opens or updates a dispute on one chain. Requires params.ledgerChannel == true.
// Accepts a higher-version state while block.timestamp < dispute.timeout.
function register(
    SignedState memory channel,
    SignedState[] memory subChannels   // sub-channels in DFS order (empty for leaf channels)
) external;

// Coordinator commits a canonical state after the dispute window closes.
// coordSigs[i] = coordinator.SignData(EncodeState(channel_i.state))
// len(coordSigs) == 1 + len(subChannels)
function coordinate(
    SignedState memory channel,
    SignedState[] memory subChannels,
    bytes[]      memory coordSigs
) external;

// Conclude a channel tree after it is coordinated (or non-coordinated with no coordinator).
function conclude(
    Channel.Params memory params,
    Channel.State  memory state,
    Channel.State[] memory subStates   // sub-channel states in DFS order
) external;

// Fast-path conclusion for a fully-signed final state (no dispute window needed).
// Requires state.isFinal == true and state.outcome.locked.length == 0.
function concludeFinal(
    Channel.Params memory params,
    Channel.State  memory state,
    bytes[]        memory sigs
) external;
```

The `Channel.Params` struct (from `Channel.sol`):

```solidity
struct Params {
    uint256        challengeDuration;   // seconds
    uint256        nonce;
    Participant[]  participants;        // [{ethAddress, ccAddress}, ...]
    address        app;
    bool           ledgerChannel;
    bool           virtualChannel;
    address        coordinator;         // address(0) if no coordinator
}
```

The coordinator signature is verified by:

```solidity
Sig.verify(abi.encode(state), coordSigs[i], params.coordinator)
```

which matches go-perun's `channel.Sign(acc, state, backendID)` →
`acc.SignData(EncodeState(state))` → `ethwallet.PrefixedHash(encodedState)` then ECDSA.

**Multi-ledger eligibility** (`MultiLedger.sol`): coordination is required only when
`params.coordinator != address(0)` AND the state has assets on more than one chain
(`isMultiLedgerState`).  A channel that has a coordinator but all assets on one chain
is treated as a normal single-ledger channel.

---

## 8. Go Module & Dependencies

### `go.mod`

```
module github.com/your-org/multiledger-poc

go 1.24

require (
    github.com/perun-network/perun-eth-backend  v0.0.0  // replace below
    perun.network/go-perun                      v0.15.1-0.20260408121133-2daea3fa699a
    github.com/ethereum/go-ethereum             v1.17.2
    github.com/miguelmota/go-ethereum-hdwallet  v0.1.1
    github.com/stretchr/testify                 v1.11.1
    polycry.pt/poly-go                          v0.0.0-20220301085937-fb9d71b45a37
)

replace (
    // Point at the local checkout of this repository.
    github.com/perun-network/perun-eth-backend => ../perun-eth-backend

    // The go-perun fork that includes CoordinatorSubscriber, CoordinatedEvent, etc.
    perun.network/go-perun => github.com/NhoxxKienn/go-perun v0.0.0-20260521103517-961fdb7beed3
)
```

The exact version of `go-perun` fork and `perun-eth-backend` in your replace directives
must match what is in `perun-eth-backend/go.mod`.

---

## 9. Go Implementation

### 9.1 Chain helpers

```go
// poc/helpers.go
package poc

import (
    "context"
    "crypto/ecdsa"
    "encoding/json"
    "fmt"
    "math/big"
    "os"

    "github.com/ethereum/go-ethereum/common"
    "github.com/ethereum/go-ethereum/common/hexutil"
    "github.com/ethereum/go-ethereum/core/types"
    "github.com/ethereum/go-ethereum/ethclient"
    "github.com/ethereum/go-ethereum/rpc"
    hdwallet "github.com/miguelmota/go-ethereum-hdwallet"
)

// ChainConfig holds all chain-specific handles.
type ChainConfig struct {
    RPC       string          // "http://127.0.0.1:8545"
    ChainID   *big.Int        // big.NewInt(1337)
    Client    *ethclient.Client
    AdjAddr   common.Address
    AssetAddr common.Address
}

// NewChainConfig dials an ethclient and returns a ChainConfig.
func NewChainConfig(rpcURL string, chainID int64, adjAddr, assetAddr string) (*ChainConfig, error) {
    c, err := ethclient.Dial(rpcURL)
    if err != nil {
        return nil, err
    }
    return &ChainConfig{
        RPC:       rpcURL,
        ChainID:   big.NewInt(chainID),
        Client:    c,
        AdjAddr:   common.HexToAddress(adjAddr),
        AssetAddr: common.HexToAddress(assetAddr),
    }, nil
}

// MakeSigner returns a London/EIP-1559 signer for the given chain.
func MakeSigner(chainID *big.Int) types.Signer {
    return types.LatestSignerForChainID(chainID)
}

// AdvanceTime increments the Hardhat node's timestamp by `seconds` and mines one block.
// This is required to expire a dispute window whose timeout uses block.timestamp.
func AdvanceTime(ctx context.Context, rpcURL string, seconds uint64) error {
    c, err := rpc.DialContext(ctx, rpcURL)
    if err != nil {
        return err
    }
    defer c.Close()
    if err := c.CallContext(ctx, nil, "evm_increaseTime",
        hexutil.EncodeUint64(seconds)); err != nil {
        return fmt.Errorf("evm_increaseTime: %w", err)
    }
    return c.CallContext(ctx, nil, "evm_mine")
}

const hardhatMnemonic = "test test test test test test test test test test test junk"

// DeriveKey returns the private key for BIP-44 account index i from the standard
// Hardhat mnemonic (m/44'/60'/0'/0/i).
func DeriveKey(index uint32) (*ecdsa.PrivateKey, error) {
    w, err := hdwallet.NewFromMnemonic(hardhatMnemonic)
    if err != nil {
        return nil, err
    }
    path := hdwallet.MustParseDerivationPath(
        fmt.Sprintf("m/44'/60'/0'/0/%d", index))
    acc, err := w.Derive(path, false)
    if err != nil {
        return nil, err
    }
    return w.PrivateKey(acc)
}

// LoadChains reads hardhat/addresses.json (written by deploy.js) and returns
// ChainConfigs for chainA and chainB.
func LoadChains() (chainA, chainB *ChainConfig, err error) {
    data, err := os.ReadFile("../hardhat/addresses.json")
    if err != nil {
        return nil, nil, fmt.Errorf("reading addresses.json: %w", err)
    }
    var addrs struct {
        ChainA struct {
            Adjudicator string `json:"adjudicator"`
            AssetHolder string `json:"assetHolder"`
        } `json:"chainA"`
        ChainB struct {
            Adjudicator string `json:"adjudicator"`
            AssetHolder string `json:"assetHolder"`
        } `json:"chainB"`
    }
    if err := json.Unmarshal(data, &addrs); err != nil {
        return nil, nil, fmt.Errorf("parsing addresses.json: %w", err)
    }
    chainA, err = NewChainConfig("http://127.0.0.1:8545", 1337,
        addrs.ChainA.Adjudicator, addrs.ChainA.AssetHolder)
    if err != nil {
        return nil, nil, fmt.Errorf("connecting chain A: %w", err)
    }
    chainB, err = NewChainConfig("http://127.0.0.1:8546", 1338,
        addrs.ChainB.Adjudicator, addrs.ChainB.AssetHolder)
    if err != nil {
        return nil, nil, fmt.Errorf("connecting chain B: %w", err)
    }
    return chainA, chainB, nil
}

// ethBalanceReader reads the native ETH balance of an account on one chain.
// It implements the BalanceReader interface defined in participant.go.
type ethBalanceReader struct {
    client *ethclient.Client
    addr   common.Address
}

func newETHBalanceReader(client *ethclient.Client, addr common.Address) *ethBalanceReader {
    return &ethBalanceReader{client: client, addr: addr}
}

func (r *ethBalanceReader) Balance() *big.Int {
    bal, err := r.client.BalanceAt(context.Background(), r.addr, nil)
    if err != nil {
        return big.NewInt(0)
    }
    return bal
}
```

### 9.2 Participant setup

```go
// poc/participant.go
package poc

import (
    "crypto/ecdsa"
    "math/big"
    "testing"

    "github.com/ethereum/go-ethereum/accounts"
    "github.com/ethereum/go-ethereum/crypto"
    "github.com/stretchr/testify/require"

    ethchannel "github.com/perun-network/perun-eth-backend/channel"
    ethwallet  "github.com/perun-network/perun-eth-backend/wallet"
    simplewallet "github.com/perun-network/perun-eth-backend/wallet/simple"
    "perun.network/go-perun/channel"
    "perun.network/go-perun/channel/multi"
    "perun.network/go-perun/client"
    "perun.network/go-perun/wallet"
    "perun.network/go-perun/watcher/local"
    "perun.network/go-perun/wire"
    wiretest "perun.network/go-perun/wire/test"
    "polycry.pt/poly-go/test"
)

const (
    // ethBackendID is the perun wallet.BackendID for the Ethereum backend.
    // Defined as constant 1 in github.com/perun-network/perun-eth-backend/wallet.
    ethBackendID = wallet.BackendID(ethwallet.BackendID) // == 1

    // gasLimit is used for all on-chain transactions.
    gasLimit = uint64(1_000_000)

    // challengeDuration is the dispute window in seconds for the ETH backend.
    // Note: the go-perun MockBackend interprets ChallengeDuration as milliseconds
    // for Register/Coordinate timeouts; the real on-chain adjudicator uses seconds.
    challengeDuration = uint64(15)
)

// BalanceReader checks an ETH address's balance on one chain.
type BalanceReader interface {
    Balance() *big.Int
}

// Participant bundles everything one channel participant needs.
type Participant struct {
    // Client is the go-perun client (used to propose/accept channels, settle).
    Client   *client.Client
    // WireAddress is the network identity of this participant.
    WireAddr map[wallet.BackendID]wire.Address
    // WalletAddress and WalletAccount are used to sign states.
    WalletAddr map[wallet.BackendID]wallet.Address
    WalletAcc  map[wallet.BackendID]wallet.Account

    // Per-chain adjudicators for direct on-chain calls (used in attack steps).
    AdjA, AdjB *ethchannel.Adjudicator

    // Per-chain balance readers.
    BalA, BalB BalanceReader

    // ethAccount is the go-ethereum accounts.Account (for txSender in constructors).
    ethAccount accounts.Account
}

// HandleAdjudicatorEvent implements client.AdjudicatorEventHandler so that
// Participant can be passed directly to Channel.Watch.
func (p *Participant) HandleAdjudicatorEvent(_ channel.AdjudicatorEvent) {}

// NewParticipant creates a fully-wired participant from a private key and two chain configs.
func NewParticipant(t *testing.T, key *ecdsa.PrivateKey, bus wire.Bus, chainA, chainB *ChainConfig) *Participant {
    t.Helper()
    rng := test.Prng(t)

    // Build an in-memory wallet from the private key.
    w := simplewallet.NewWallet(key)
    // The simple wallet's NewRandomAccount is not useful here; access the account directly.
    // simple.Wallet exposes a method to get accounts by address.
    addr := ethwallet.Address(crypto.PubkeyToAddress(key.PublicKey))
    acc, err := w.Unlock(&addr)  // simple.Wallet.Unlock returns the account for that address.
    require.NoError(t, err)

    ethAcc := accounts.Account{Address: addr.Address}

    // ContractBackend wraps ethclient + transactor for each chain.
    signerA := MakeSigner(chainA.ChainID)
    cbA := ethchannel.NewContractBackend(
        chainA.Client,
        ethchannel.MakeChainID(chainA.ChainID),
        simplewallet.NewTransactor(w, signerA),
        1, // txFinalityDepth
    )
    signerB := MakeSigner(chainB.ChainID)
    cbB := ethchannel.NewContractBackend(
        chainB.Client,
        ethchannel.MakeChainID(chainB.ChainID),
        simplewallet.NewTransactor(w, signerB),
        1,
    )

    // Per-chain adjudicators (receiver == participant's own address, funds go there).
    adjA := ethchannel.NewAdjudicator(cbA, chainA.AdjAddr, addr.Address, ethAcc, gasLimit)
    adjB := ethchannel.NewAdjudicator(cbB, chainB.AdjAddr, addr.Address, ethAcc, gasLimit)

    // Multi-ledger adjudicator fans out to both chains.
    mAdj := multi.NewAdjudicator()
    mAdj.RegisterAdjudicator(ethchannel.MakeLedgerBackendID(chainA.ChainID), adjA)
    mAdj.RegisterAdjudicator(ethchannel.MakeLedgerBackendID(chainB.ChainID), adjB)

    // Per-chain funders. Each chain's asset is funded on its own chain;
    // the other chain's asset uses a no-op depositor (zero-balance lock).
    assetA := ethchannel.NewAsset(chainA.ChainID, chainA.AssetAddr)
    assetB := ethchannel.NewAsset(chainB.ChainID, chainB.AssetAddr)

    funderA := ethchannel.NewFunder(cbA)
    funderA.RegisterAsset(*assetA, ethchannel.NewETHDepositor(gasLimit), ethAcc)
    funderA.RegisterAsset(*assetB, ethchannel.NewNoOpDepositor(), ethAcc)

    funderB := ethchannel.NewFunder(cbB)
    funderB.RegisterAsset(*assetA, ethchannel.NewNoOpDepositor(), ethAcc)
    funderB.RegisterAsset(*assetB, ethchannel.NewETHDepositor(gasLimit), ethAcc)

    mFund := multi.NewFunder()
    mFund.RegisterFunder(ethchannel.MakeLedgerBackendID(chainA.ChainID), funderA)
    mFund.RegisterFunder(ethchannel.MakeLedgerBackendID(chainB.ChainID), funderB)

    watcher, err := local.NewWatcher(mAdj)
    require.NoError(t, err)

    wireAddr := wiretest.NewRandomAddressesMap(rng, 1)
    perunWallet := map[wallet.BackendID]wallet.Wallet{ethBackendID: w}
    c, err := client.New(wireAddr[0], bus, mFund, mAdj, perunWallet, watcher)
    require.NoError(t, err)

    walletAddrMap := map[wallet.BackendID]wallet.Address{ethBackendID: &addr}
    walletAccMap  := map[wallet.BackendID]wallet.Account{ethBackendID: acc}

    return &Participant{
        Client:     c,
        WireAddr:   wireAddr[0],
        WalletAddr: walletAddrMap,
        WalletAcc:  walletAccMap,
        AdjA:       adjA,
        AdjB:       adjB,
        BalA:       newETHBalanceReader(chainA.Client, addr.Address),
        BalB:       newETHBalanceReader(chainB.Client, addr.Address),
        ethAccount: ethAcc,
    }
}
```

> `simplewallet.NewWallet(key)` accepts any number of `*ecdsa.PrivateKey` values and
> registers them.  `w.Unlock(&addr)` returns the `wallet.Account` for that address.

### 9.3 Coordinator setup

The coordinator is a **standalone service**, not a `client.Client`.  It uses the
`ethchannel.Coordinator` type (from `channel/coordinator.go`) which implements
`channel.CoordinatorSubscriber` (Coordinate + Subscribe).

```go
// poc/coordinator.go
package poc

import (
    "context"
    "crypto/ecdsa"
    "fmt"

    "github.com/ethereum/go-ethereum/accounts"
    "github.com/ethereum/go-ethereum/crypto"

    ethchannel "github.com/perun-network/perun-eth-backend/channel"
    ethwallet  "github.com/perun-network/perun-eth-backend/wallet"
    simplewallet "github.com/perun-network/perun-eth-backend/wallet/simple"
    "perun.network/go-perun/channel"
    "perun.network/go-perun/channel/multi"
    "perun.network/go-perun/wallet"
)

// CoordinatorService watches a multi-ledger channel and calls coordinate()
// once both dispute windows have elapsed.
type CoordinatorService struct {
    account    wallet.Account
    ethAccount accounts.Account
    coordA, coordB *ethchannel.Coordinator
    multiCoord *multi.Coordinator
    backendID  wallet.BackendID
}

// NewCoordinatorService creates a coordinator from a private key and two chain configs.
func NewCoordinatorService(key *ecdsa.PrivateKey, chainA, chainB *ChainConfig) *CoordinatorService {
    w := simplewallet.NewWallet(key)
    addr := ethwallet.Address(crypto.PubkeyToAddress(key.PublicKey))
    acc, _  := w.Unlock(&addr)
    ethAcc  := accounts.Account{Address: addr.Address}

    signerA := MakeSigner(chainA.ChainID)
    cbA := ethchannel.NewContractBackend(chainA.Client,
        ethchannel.MakeChainID(chainA.ChainID),
        simplewallet.NewTransactor(w, signerA), 1)
    signerB := MakeSigner(chainB.ChainID)
    cbB := ethchannel.NewContractBackend(chainB.Client,
        ethchannel.MakeChainID(chainB.ChainID),
        simplewallet.NewTransactor(w, signerB), 1)

    // The receiver address receives any withdrawn funds — for a coordinator
    // this is typically irrelevant (coordinator doesn't withdraw), but
    // NewCoordinator requires a receiver address.
    coordA := ethchannel.NewCoordinator(cbA, chainA.AdjAddr, addr.Address, ethAcc, gasLimit)
    coordB := ethchannel.NewCoordinator(cbB, chainB.AdjAddr, addr.Address, ethAcc, gasLimit)

    mc := multi.NewCoordinator()
    mc.RegisterCoordinator(ethchannel.MakeLedgerBackendID(chainA.ChainID), coordA)
    mc.RegisterCoordinator(ethchannel.MakeLedgerBackendID(chainB.ChainID), coordB)

    return &CoordinatorService{
        account:    acc,
        ethAccount: ethAcc,
        coordA:     coordA,
        coordB:     coordB,
        multiCoord: mc,
        backendID:  ethBackendID,
    }
}

// Coordinate coordinates the given request on all chains.
// The caller is responsible for choosing the canonical state (highest version).
// coordSigs[0] is the coordinator's signature over req.Tx.State;
// append one signature per sub-channel if any.
func (cs *CoordinatorService) Coordinate(
    ctx context.Context,
    req channel.AdjudicatorReq,
    subStates []channel.SignedState,
) error {
    coordSigs := make([]wallet.Sig, 1+len(subStates))
    var err error
    // Sign the main channel state.
    coordSigs[0], err = channel.Sign(cs.account, req.Tx.State, cs.backendID)
    if err != nil {
        return fmt.Errorf("signing main state: %w", err)
    }
    // Sign each sub-channel state.
    for i, ss := range subStates {
        coordSigs[i+1], err = channel.Sign(cs.account, ss.State, cs.backendID)
        if err != nil {
            return fmt.Errorf("signing sub-state %d: %w", i, err)
        }
    }
    return cs.multiCoord.Coordinate(ctx, req, subStates, coordSigs)
}

// WalletAddress returns the coordinator's wallet address map (for WithCoordinator).
func (cs *CoordinatorService) WalletAddress() map[wallet.BackendID]wallet.Address {
    addr := ethwallet.Address(cs.ethAccount.Address)
    return map[wallet.BackendID]wallet.Address{cs.backendID: &addr}
}
```

### 9.4 Helper: buildSecretReq

This bypasses the normal `client.Update` flow to fabricate a state that Alice has
signed but never delivered — simulating a key-extraction or side-channel attack.

```go
// poc/attack_test.go
package poc_test

import (
    "fmt"

    "perun.network/go-perun/channel"
    "perun.network/go-perun/wallet"
)

// buildSecretSignedReq fabricates a fully-signed AdjudicatorReq at
// baseReq.Tx.State.Version+1 with newBalances.  All accounts in accs sign
// the new state.  Alice's client never sees this state; the Update flow
// is bypassed entirely.  idx picks which participant's account/index to use.
func buildSecretSignedReq(
    baseReq    channel.AdjudicatorReq,
    newBalances channel.Balances,
    accs       []wallet.Account,
    bID        wallet.BackendID,
    idx        channel.Index,
) (channel.AdjudicatorReq, error) {
    s := baseReq.Tx.State.Clone()
    s.Version  = baseReq.Tx.State.Version + 1
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
```

### 9.5 Attack test — no coordinator

```go
// poc/attack_test.go (continued)
package poc_test

import (
    "context"
    "math/big"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    "perun.network/go-perun/channel"
    "perun.network/go-perun/client"
    "perun.network/go-perun/wallet"
    "perun.network/go-perun/wire"
    ctest "perun.network/go-perun/client/test"
    ethchannel "github.com/perun-network/perun-eth-backend/channel"
)

var (
    initBals = channel.Balances{
        {ether(8), ether(2)},  // Asset1: Alice 8, Bob 2
        {ether(2), ether(8)},  // Asset2: Alice 2, Bob 8
    }
    v1Bals = channel.Balances{
        {ether(5), ether(5)},
        {ether(3), ether(7)},
    }
    v2Bals = channel.Balances{
        {ether(1), ether(9)},
        {ether(5), ether(5)},
    }
    balanceDelta = ether(0.001) // gas tolerance
)

func ether(e float64) *big.Int {
    f := new(big.Float).SetFloat64(e)
    f.Mul(f, new(big.Float).SetFloat64(1e18))
    i, _ := f.Int(nil)
    return i
}

// TestAttackNoCoordinator proves that without a coordinator, divergent
// settlement SUCCEEDS: Bob registers different versions on each chain and
// withdraws more than the honest v1 outcome.
func TestAttackNoCoordinator(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
    defer cancel()
    require := require.New(t)
    assert  := assert.New(t)

    chainA, chainB, err := poc.LoadChains()
    require.NoError(err)
    bus := wire.NewLocalBus()

    aliceKey, err := poc.DeriveKey(1); require.NoError(err)
    bobKey,   err := poc.DeriveKey(2); require.NoError(err)

    alice := poc.NewParticipant(t, aliceKey, bus, chainA, chainB)
    bob   := poc.NewParticipant(t, bobKey,   bus, chainA, chainB)

    assetA := ethchannel.NewAsset(chainA.ChainID, chainA.AssetAddr)
    assetB := ethchannel.NewAsset(chainB.ChainID, chainB.AssetAddr)

    bID1 := wallet.BackendID(assetA.LedgerBackendID().BackendID())
    bID2 := wallet.BackendID(assetB.LedgerBackendID().BackendID())

    // ── Open channel WITHOUT a coordinator ─────────────────────────────────
    parts := []map[wallet.BackendID]wire.Address{alice.WireAddr, bob.WireAddr}
    initAlloc := channel.NewAllocation(2, []wallet.BackendID{bID1, bID2}, assetA, assetB)
    initAlloc.Balances = initBals

    prop, err := client.NewLedgerChannelProposal(
        challengeDuration,
        alice.WalletAddr,
        initAlloc,
        parts,
        // NO client.WithCoordinator(...)
    )
    require.NoError(err)

    chans := make(chan *client.Channel, 1)
    errs  := make(chan error, 2)

    go alice.Client.Handle(ctest.AlwaysRejectChannelHandler(ctx, errs),
        ctest.AlwaysAcceptUpdateHandler(ctx, errs))
    go bob.Client.Handle(ctest.AlwaysAcceptChannelHandler(ctx, bob.WalletAddr, chans, errs),
        ctest.AlwaysAcceptUpdateHandler(ctx, errs))

    chAliceBob, err := alice.Client.ProposeChannel(ctx, prop)
    require.NoError(err)
    var chBobAlice *client.Channel
    select {
    case chBobAlice = <-chans:
    case err := <-errs:
        t.Fatalf("channel open: %v", err)
    }

    // Legitimate update to v1.
    done := make(chan struct{}, 1)
    chBobAlice.OnUpdate(func(_, _ *channel.State) { done <- struct{}{} })
    require.NoError(chAliceBob.Update(ctx, func(s *channel.State) { s.Balances = v1Bals }))
    <-done; time.Sleep(100 * time.Millisecond)

    v1ReqAlice := client.NewTestChannel(chAliceBob).AdjudicatorReq()
    v1ReqBob   := client.NewTestChannel(chBobAlice).AdjudicatorReq()

    // Fabricate SECRET v2 — signed by both accounts, Alice's machine stays at v1.
    accs := []wallet.Account{alice.WalletAcc[bID1], bob.WalletAcc[bID1]}
    v2ReqBob,   err := buildSecretSignedReq(v1ReqBob,   v2Bals, accs, bID1, 1)
    require.NoError(err)
    v2ReqAlice, err := buildSecretSignedReq(v1ReqAlice, v2Bals, accs, bID1, 0)
    require.NoError(err)

    chID := chAliceBob.ID()

    // ── ATTACK STEP 1: Bob registers v1 on Chain B ─────────────────────────
    require.NoError(bob.AdjB.Register(ctx, v1ReqBob, nil))

    // Wait for Chain B's dispute window to expire (15 s + 1 s margin).
    require.NoError(poc.AdvanceTime(ctx, chainB.RPC, challengeDuration+1))
    sub2, err := bob.AdjB.Subscribe(ctx, chID)
    require.NoError(err)
    e := sub2.Next()
    require.IsType(&channel.RegisteredEvent{}, e)
    require.NoError(e.(*channel.RegisteredEvent).TimeoutV.Wait(ctx))
    require.NoError(sub2.Close())

    // ── ATTACK STEP 2: Bob reveals SECRET v2 on Chain A ────────────────────
    // Chain A has no prior registration, so v2 is accepted unconditionally.
    require.NoError(bob.AdjA.Register(ctx, v2ReqBob, nil))

    require.NoError(poc.AdvanceTime(ctx, chainA.RPC, challengeDuration+1))
    sub1, err := bob.AdjA.Subscribe(ctx, chID)
    require.NoError(err)
    e = sub1.Next()
    require.IsType(&channel.RegisteredEvent{}, e)
    require.NoError(e.(*channel.RegisteredEvent).TimeoutV.Wait(ctx))
    require.NoError(sub1.Close())

    // ── Withdraw: each chain at its locally registered state ──────────────
    //   Chain A (v2): Alice 1, Bob 9
    //   Chain B (v1): Alice 3, Bob 7
    require.NoError(bob.AdjA.Withdraw(ctx, v2ReqBob,   nil))
    require.NoError(alice.AdjA.Withdraw(ctx, v2ReqAlice, nil))
    require.NoError(bob.AdjB.Withdraw(ctx, v1ReqBob,   nil))
    require.NoError(alice.AdjB.Withdraw(ctx, v1ReqAlice, nil))

    _ = chAliceBob.Close(); _ = chBobAlice.Close()

    // ── Assert: attack SUCCEEDED (divergent outcome) ──────────────────────
    // v1 honest diff: Bob +(5-2) on A1, +(7-8) on A2 = +3-1 = +2 net
    // Attack diff:    Bob +(9-2) on A1, +(7-8) on A2 = +7-1 = +6 net
    attackDiff := channel.Balances{
        v2Bals.Sub(initBals)[0], // Asset1 at v2
        v1Bals.Sub(initBals)[1], // Asset2 at v1
    }
    balancesAfter := channel.Balances{
        {alice.BalA.Balance(), bob.BalA.Balance()},
        {alice.BalB.Balance(), bob.BalB.Balance()},
    }
    diff := balancesAfter.Sub(initBals)
    assert.True(ctest.EqualBalancesWithDelta(attackDiff, diff, balanceDelta),
        "divergent attack outcome: expected %v ±%v, got %v", attackDiff, balanceDelta, diff)
    t.Logf("Attack SUCCEEDED — Bob: %v (A1) + %v (A2), Alice: %v (A1) + %v (A2)",
        balancesAfter[0][1], balancesAfter[1][1],
        balancesAfter[0][0], balancesAfter[1][0])
}
```

### 9.6 Coordinator test — divergence prevented

```go
// poc/coordinate_test.go
package poc_test

import (
    "context"
    "testing"
    "time"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    "perun.network/go-perun/channel"
    "perun.network/go-perun/client"
    "perun.network/go-perun/wallet"
    "perun.network/go-perun/wire"
    ctest "perun.network/go-perun/client/test"
    ethchannel "github.com/perun-network/perun-eth-backend/channel"
)

// TestAttackWithCoordinator proves that the coordinator PREVENTS divergent
// settlement.  Bob still registers different versions on each chain, but the
// coordinator coordinates the SAME (highest) version on both before withdrawal.
//
// The coordinator does NOT prevent Bob's v2 registration — it ensures that
// whatever the highest version is, BOTH chains settle at that version.
//
// Protection guarantee:
//   Without coordinator: Chain A→v2, Chain B→v1  (divergent, Bob steals)
//   With coordinator:    Chain A→v2, Chain B→v2  (uniform, no divergence)
func TestAttackWithCoordinator(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
    defer cancel()
    require := require.New(t)
    assert  := assert.New(t)

    chainA, chainB, err := poc.LoadChains()
    require.NoError(err)
    bus := wire.NewLocalBus()

    aliceKey,   err := poc.DeriveKey(1); require.NoError(err)
    bobKey,     err := poc.DeriveKey(2); require.NoError(err)
    charlieKey, err := poc.DeriveKey(3); require.NoError(err) // coordinator

    alice   := poc.NewParticipant(t, aliceKey,   bus, chainA, chainB)
    bob     := poc.NewParticipant(t, bobKey,     bus, chainA, chainB)
    charlie := poc.NewCoordinatorService(charlieKey, chainA, chainB)

    assetA := ethchannel.NewAsset(chainA.ChainID, chainA.AssetAddr)
    assetB := ethchannel.NewAsset(chainB.ChainID, chainB.AssetAddr)
    bID1   := wallet.BackendID(assetA.LedgerBackendID().BackendID())

    // ── Open channel WITH coordinator ──────────────────────────────────────
    parts := []map[wallet.BackendID]wire.Address{alice.WireAddr, bob.WireAddr}
    initAlloc := channel.NewAllocation(2,
        []wallet.BackendID{bID1, wallet.BackendID(assetB.LedgerBackendID().BackendID())},
        assetA, assetB)
    initAlloc.Balances = initBals

    prop, err := client.NewLedgerChannelProposal(
        challengeDuration,
        alice.WalletAddr,
        initAlloc,
        parts,
        client.WithCoordinator(charlie.WalletAddress()), // ← encodes coordinator in Params
    )
    require.NoError(err)

    chans := make(chan *client.Channel, 1)
    errs  := make(chan error, 2)
    go alice.Client.Handle(ctest.AlwaysRejectChannelHandler(ctx, errs),
        ctest.AlwaysAcceptUpdateHandler(ctx, errs))
    go bob.Client.Handle(ctest.AlwaysAcceptChannelHandler(ctx, bob.WalletAddr, chans, errs),
        ctest.AlwaysAcceptUpdateHandler(ctx, errs))

    chAliceBob, err := alice.Client.ProposeChannel(ctx, prop)
    require.NoError(err)
    var chBobAlice *client.Channel
    select {
    case chBobAlice = <-chans:
    case err := <-errs:
        t.Fatalf("channel open: %v", err)
    }

    // Start watchers so the watcher replicates Chain B's registration to Chain A.
    // Participant implements client.AdjudicatorEventHandler via HandleAdjudicatorEvent.
    go func() { errs <- chAliceBob.Watch(alice) }()
    go func() { errs <- chBobAlice.Watch(bob) }()
    time.Sleep(100 * time.Millisecond)

    // Legitimate update to v1.
    done := make(chan struct{}, 1)
    chBobAlice.OnUpdate(func(_, _ *channel.State) { done <- struct{}{} })
    require.NoError(chAliceBob.Update(ctx, func(s *channel.State) { s.Balances = v1Bals }))
    <-done; time.Sleep(100 * time.Millisecond)

    v1ReqBob := client.NewTestChannel(chBobAlice).AdjudicatorReq()

    // Fabricate SECRET v2.
    accs := []wallet.Account{alice.WalletAcc[bID1], bob.WalletAcc[bID1]}
    v2ReqBob, err := buildSecretSignedReq(v1ReqBob, v2Bals, accs, bID1, 1)
    require.NoError(err)

    chID := chAliceBob.ID()

    // Subscribe to both chains BEFORE any registration so we don't miss events.
    sub1, err := bob.AdjA.Subscribe(ctx, chID); require.NoError(err)
    sub2, err := bob.AdjB.Subscribe(ctx, chID); require.NoError(err)

    // ── ATTACK STEP 1: Bob registers v1 on Chain B ─────────────────────────
    require.NoError(bob.AdjB.Register(ctx, v1ReqBob, nil))
    // Watcher sees Chain B's event and replicates v1 to Chain A.
    e2 := sub2.Next()
    require.IsType(&channel.RegisteredEvent{}, e2, "expected RegisteredEvent on Chain B")

    // Expire Chain B's window.  Chain B is now frozen at v1.
    require.NoError(poc.AdvanceTime(ctx, chainB.RPC, challengeDuration+1))
    require.NoError(e2.(*channel.RegisteredEvent).TimeoutV.Wait(ctx))

    // Collect the watcher-replicated v1 event on Chain A.
    e1 := sub1.Next()
    require.IsType(&channel.RegisteredEvent{}, e1, "expected watcher-replicated RegisteredEvent on Chain A")

    // ── ATTACK STEP 2: Bob registers v2 on Chain A BEFORE Chain A's window expires ──
    // Chain A's window is still open (only Chain B's was expired above).
    // v2 > v1 → accepted by registerSingle.
    require.NoError(bob.AdjA.Register(ctx, v2ReqBob, nil),
        "v2 must be accepted on Chain A while its dispute window is still open")

    // Collect the v2 registration event on Chain A (supersedes the watcher's v1).
    e1 = sub1.Next()
    require.IsType(&channel.RegisteredEvent{}, e1, "expected RegisteredEvent(v2) on Chain A")
    _ = sub1.Close(); _ = sub2.Close()

    // Expire Chain A's window.  Both chains are now frozen (Chain B at v1, Chain A at v2).
    require.NoError(poc.AdvanceTime(ctx, chainA.RPC, challengeDuration+1))
    require.NoError(e1.(*channel.RegisteredEvent).TimeoutV.Wait(ctx))

    // ── Charlie coordinates: picks v2 (highest version) on BOTH chains ─────
    // Passing v2ReqBob because v2 is the highest-version state observed (Chain A).
    // multi.Coordinator.Coordinate fans out concurrently to both chains.
    err = charlie.Coordinate(ctx, v2ReqBob, nil)
    require.NoError(err, "coordinator must succeed after both windows have elapsed")

    // Wait for the CoordinatedEvent to reach Alice's and Bob's watcher machines.
    // ensureCoordinated (called by Settle) would block until the machine phase
    // advances, but an explicit poll here avoids a race where Settle is called
    // before the event is delivered.
    require.Eventually(func() bool {
        return chAliceBob.Phase() == channel.Coordinated
    }, 10*time.Second, 200*time.Millisecond, "alice's machine must reach Coordinated phase")
    require.Eventually(func() bool {
        return chBobAlice.Phase() == channel.Coordinated
    }, 10*time.Second, 200*time.Millisecond, "bob's machine must reach Coordinated phase")

    // ── Settle: ensureCoordinated is a no-op (already Coordinated) ──────────
    require.NoError(chAliceBob.Settle(ctx, false))
    require.NoError(chBobAlice.Settle(ctx, false))
    require.NoError(chAliceBob.Close())
    require.NoError(chBobAlice.Close())

    // ── Assert: UNIFORM v2 outcome on both chains (no divergence) ────────────
    balancesAfter := channel.Balances{
        {alice.BalA.Balance(), bob.BalA.Balance()},
        {alice.BalB.Balance(), bob.BalB.Balance()},
    }
    diff := balancesAfter.Sub(initBals)
    allV2Diff := v2Bals.Sub(initBals)
    attackDiff := channel.Balances{
        v2Bals.Sub(initBals)[0], // Chain A at v2 — what the divergent attack would give
        v1Bals.Sub(initBals)[1], // Chain B at v1 — the other divergent half
    }
    assert.True(ctest.EqualBalancesWithDelta(allV2Diff, diff, balanceDelta),
        "coordinator must enforce uniform v2: expected %v ±%v, got %v",
        allV2Diff, balanceDelta, diff)
    assert.False(ctest.EqualBalancesWithDelta(attackDiff, diff, balanceDelta),
        "divergent outcome must not occur with coordinator: attack %v, got %v",
        attackDiff, diff)
    t.Logf("Attack PREVENTED — uniform v2: Bob %v (A1) + %v (A2), Alice %v (A1) + %v (A2)",
        balancesAfter[0][1], balancesAfter[1][1],
        balancesAfter[0][0], balancesAfter[1][0])
}
```

---

## 10. Running the PoC

```bash
# 1. Start both Hardhat chains (keep running in separate terminals).
cd hardhat
npx hardhat node --port 8545   # Chain A
npx hardhat node --port 8546   # Chain B

# 2. Deploy contracts to both chains.
npx hardhat run scripts/deploy.js --network chainA
npx hardhat run scripts/deploy.js --network chainB
# → hardhat/addresses.json updated

# 3. Generate Go bindings if not present (from perun-eth-backend repo root).
cd ../perun-eth-backend/bindings && ./generate.sh

# 4. Run the attack test (attack SUCCEEDS — demonstrates vulnerability).
cd ../multiledger-poc
go test -v -run TestAttackNoCoordinator -timeout 120s ./poc/

# 5. Run the coordinator test (attack PREVENTED — coordinator enforces uniformity).
go test -v -run TestAttackWithCoordinator -timeout 120s ./poc/

# 6. Run with race detector.
go test -count=1 -timeout 180s -race ./poc/
```

---

## 11. Expected Output

### TestAttackNoCoordinator

```
=== RUN   TestAttackNoCoordinator
    attack_test.go: Divergent attack outcome confirmed:
        Chain A at v2 → Alice: 1 ETH, Bob: 9 ETH
        Chain B at v1 → Alice: 3 ETH, Bob: 7 ETH
        Bob total: 16 ETH (honest v1 baseline: 12 ETH)
--- PASS: TestAttackNoCoordinator (30.x s)
```

### TestAttackWithCoordinator

```
=== RUN   TestAttackWithCoordinator
    coordinate_test.go: Attack PREVENTED — uniform v2:
        Bob  9 ETH (A1) + 5 ETH (A2)
        Alice 1 ETH (A1) + 5 ETH (A2)  ← no divergence; both chains at v2
--- PASS: TestAttackWithCoordinator (35.x s)
```

The coordinator picks v2 as the canonical state (highest version seen across all chains).
The protection guarantee is **no divergence**, not necessarily reverting to v1.  Bob
registered v2 legitimately (both Alice's and Bob's signatures are on v2), so the
coordinator locks that outcome uniformly on both chains.

---

## 12. Key Invariants Summary

| Layer            | Invariant                                                                           | Where enforced                                                        |
| ---------------- | ----------------------------------------------------------------------------------- | --------------------------------------------------------------------- |
| Contract         | `coordinate()` rejected if not registered                                           | `coordinateSingle`: `"not registered"`                                |
| Contract         | `coordinate()` rejected before timeout                                              | `coordinateSingle`: `block.timestamp >= dispute.timeout`              |
| Contract         | Coordinator ECDSA sig required                                                      | `Channel.validateCoordinatorSignature`                                |
| Contract         | `register()` rejected in COORDINATED phase                                          | `registerSingle`: `dispute.phase == DISPUTE` else `"incorrect phase"` |
| Contract         | `conclude()` on coordinated-eligible channels requires COORDINATED phase            | `concludeSingle`: `"coordinated settlement required"`                 |
| Contract         | `isCoordinatedEligible` requires `coordinator != address(0)` AND multi-chain assets | `MultiLedger.sol`                                                     |
| go-perun multi   | Both chains coordinated concurrently                                                | `multi.Coordinator.dispatch`                                          |
| go-perun client  | `Settle` calls `ensureCoordinated` before `Withdraw`                                | `client.Channel.Settle`                                               |
| go-perun watcher | Dispute replicated to all chains                                                    | `watcher/local` multi-ledger path                                     |
| Timing           | All timeouts use `block.timestamp` (seconds), not block number                      | `Adjudicator.sol` (use `evm_increaseTime` in tests)                   |

### Timing diagram (with coordinator)

```
         t0          t1 (B frozen)       t2      t3 (A frozen)     t4
Chain B:  register(v1)──[window: 15 s]──timeout────────────────── COORDINATED(v2)──CONCLUDED
                                            │                            ↑
Chain A:  register(v1)─── register(v2) ────│─[window: 15 s]──timeout───┘
          ↑ watcher          ↑ Bob          │                       Charlie calls
          replication        (before A      │                       coordinate(v2)
                             window closes) │                       on both chains
```

The key sequencing: Chain B's window expires first (t1).  Only then does Bob register
v2 on Chain A (still within Chain A's open window).  Chain A's window expires (t3)
with v2 frozen.  The coordinator then picks v2 as canonical and locks both chains.

Without the coordinator, nobody calls `coordinate()` and both chains pay out
independently — Chain B at v1, Chain A at v2.  With the coordinator, once both are
COORDINATED at v2, `registerSingle` rejects any further state on either chain.

---

## 13. Real Coordinator Service (`cross-chain-coordinator`)

The inline `CoordinatorService` in Section 9.3 is a **manual-trigger test double** — the
caller picks the canonical state and invokes `Coordinate(ctx, req, subStates)` directly.
The production coordinator (this repository, `cross-chain-coordinator/`) wraps that same
primitive in a libp2p relay service that:

- accepts watch notifications from go-perun clients over a circuit relay,
- subscribes to on-chain `RegisteredEvent` / `ProgressedEvent` / `CoordinatedEvent` /
  `ConcludedEvent` on every chain,
- waits the per-chain challenge duration (wall-clock),
- selects the highest-version canonical state across all chains,
- builds DFS-ordered `coordSigs` and dispatches `coordinate()` to all chains concurrently.

### 13.1 Layout

```
cross-chain-coordinator/
├── main.go                       # CLI: -mode keygen | -mode relay
├── devnet_config.yaml            # per-chain config (see 13.2)
├── service/
│   └── service.go                # service.New(coords, ecdsaKey, libp2pKey) thin wrapper
├── backends/
│   ├── config.go                 # YAML loader + Validate (uniqueness on backend_id/ledger_id)
│   ├── multicoordinator.go       # builds *multi.Coordinator + wallet.Account per backend
│   └── ethcoordinator.go         # one *ethchannel.Coordinator per chain
└── coordinator/
    ├── host.go                   # libp2p host, relay reservation (renewed every 4 min)
    ├── handlers.go               # JSON-decode + dispatch for 3 protocol streams
    ├── protocol.go               # protocol IDs, request/response types
    ├── registry.go               # in-memory coordCh + registry, per-chain dispute records
    ├── watcher.go                # startWatching*, event loop, awaitFinalisationAndCoordinate
    └── coordinator.go            # coordinate(), selectCanonicalSignedState, buildCoordSigs
```

| File                                                     | Role                                                                                                                           |
| -------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `main.go`                                                | `-mode keygen` writes libp2p identity; `-mode relay` runs the service                                                          |
| `backends/multicoordinator.go: SetupMultiCoordinator`    | Per chain in YAML, registers an `*ethchannel.Coordinator` under `ethchannel.MakeLedgerBackendID(big.NewInt(ledgerID))`         |
| `coordinator/host.go: SetupRelayCoordinator`             | `libp2p.New(NoListenAddrs, EnableRelay)`, dials `relay.perun.network:5574`, reserves a slot, registers three stream handlers   |
| `coordinator/watcher.go: handleEventsFromChain`          | Single goroutine per channel; consumes the multi-ledger `*multi.AdjudicatorSubscription` and routes events                     |
| `coordinator/watcher.go: awaitFinalisationAndCoordinate` | Spawned on first `RegisteredEvent`; **wall-clock** sleep for `ChallengeDuration`, then calls `coordinator.coordinate(ctx, ch)` |
| `coordinator/coordinator.go: coordinate`                 | `selectCanonicalSignedState` → `retrieveSignedSubStates` (DFS) → `buildCoordSigs` → `multi.Coordinator.Coordinate`             |

### 13.2 Configuration (`devnet_config.yaml`)

```yaml
private_key_path: ./sign_private.key   # 64-char hex ECDSA key (one line, no 0x prefix)
coordinators:
  - backend_id: 1                       # ethwallet.BackendID == 1
    ledger_id: 1337                     # Chain A chain ID
    chainURL: "ws://127.0.0.1:8545"     # WebSocket endpoint for SubscribeNewHead
    adjudicator_addr: "0xDEADBEEF..."   # deployed Adjudicator address on chain 1337
  - backend_id: 1
    ledger_id: 1338
    chainURL: "ws://127.0.0.1:8546"
    adjudicator_addr: "0xCAFEBABE..."
```

Important notes:

- **`chainURL` must be `ws://` or `wss://`.** The `ethchannel.Coordinator` calls
  `SubscribeNewHead` inside `BlockTimeout.Wait` and `confirmNTimes`; the JSON-RPC HTTP
  transport does not support subscriptions and will silently fail at first use.
- **The ECDSA key is the coordinator's signing key.** Its derived ETH address is what
  clients must embed via `client.WithCoordinator(map[BackendID]wallet.Address{1: addr})`
  at channel-open time. Channel IDs are derived from `Params` including this map — a
  mismatch causes `CalcID` to differ on the client side and the channel is unrecognisable.
- **The libp2p key (`-keyfile`) is independent of the ECDSA key** and determines the
  service's `peer.ID`. Keep it stable across restarts — clients hardcode this peer ID.
- `backends/config.go: Validate` rejects duplicate `backend_id/ledger_id` pairs.

### 13.3 Running the service

```bash
# 1) Generate the libp2p identity (one-time). Despite the filename, this writes an
#    RSA-2048 libp2p private key (NOT an ECDSA key).
go run main.go -mode keygen -keyfile sign_private.key

# 2) Generate the ECDSA signing key separately (one-time). The path is whatever
#    `private_key_path` in your YAML points at.
openssl rand -hex 32 > coord_ecdsa.key
# Update devnet_config.yaml: private_key_path: ./coord_ecdsa.key

# 3) Start the relay coordinator.
go run main.go -mode relay -keyfile sign_private.key -config devnet_config.yaml
# Logs: "Relay server started with ID: 12D3KooW..."   ← this is the peer.ID
```

The service connects to `relay.perun.network:5574` (relay peer ID
`QmcxeYpYpYPX4J3478YZUaxFytYfUDbNe1jUWVYeZjL3gY`, hardcoded in
`coordinator/protocol.go: relayID`), reserves a relay slot (renewed every 4 minutes),
and registers three stream handlers:

| Protocol ID                              | Triggered by client API                             |
| ---------------------------------------- | --------------------------------------------------- |
| `/coordinator/notify-watch-ledger/1.0.0` | `RelayCoordinatorNotifier.NotifyWatchLedgerChannel` |
| `/coordinator/notify-watch-sub/1.0.0`    | `RelayCoordinatorNotifier.NotifyWatchSubChannel`    |
| `/coordinator/notify-stop-watch/1.0.0`   | `RelayCoordinatorNotifier.NotifyStopWatch`          |

All inbound streams arrive via the circuit relay — the coordinator never listens
directly (`libp2p.NoListenAddrs`).

### 13.4 Client-side wiring (replaces 9.3 for end-to-end PoC)

To drive the real service over libp2p instead of calling `charlie.Coordinate(...)`
inline, replace the test-double `CoordinatorService` with the relay notifier:

```go
import (
    libp2pwire "perun.network/go-perun/wire/net/libp2p"
    "perun.network/go-perun/client"
)

// One per participant — must be installed BEFORE the first ProposeChannel.
account, err := libp2pwire.NewAccount(privKey /* + dialer/listener config */)
require.NoError(err)
// coordinatorPeerID is the peer.ID printed by the running coordinator service
pid, err := peer.Decode("<COORDINATOR_PEER_ID>")
require.NoError(err)
notifier := libp2pwire.NewRelayCoordinatorNotifier(account, pid)
client.EnableCoordinationNotifier(notifier)

// Channel proposals must include the coordinator's address.
prop, err := client.NewLedgerChannelProposal(
    challengeDuration, alice.WalletAddr, initAlloc, parts,
    client.WithCoordinator(map[wallet.BackendID]wallet.Address{1: charlieEthAddr}),
)
```

When `ProposeChannel` runs, `RelayCoordinatorNotifier.NotifyWatchLedgerChannel` opens
a stream to `coordID` (over the circuit relay), JSON-encodes the `SignedState`, and
waits for the OK response. The coordinator service then watches all chains
automatically — the PoC's manual `charlie.Coordinate(...)` call is removed entirely.

**CRITICAL — current hardcoding limitation:** in the go-perun fork, the relay target
peer ID is a placeholder:

```go
// go-perun/wire/net/libp2p/coordinator.go:22
const coordID = "coordinator" // TODO: replace with real peer.ID
```

This MUST be replaced with the peer.ID printed by your running service
(`Relay server started with ID: 12D3KooW...`) **before** the PoC can wire through
libp2p. Until that change lands, every `NotifyWatch*` call fails silently on the
client side and the coordinator never sees the channel — Settle would then block
indefinitely on `ensureCoordinated`.

### 13.5 Event-driven coordination flow

After notification reaches the service, no caller intervention is required:

```
NotifyWatchLedgerChannel(signedState)
    │
    ▼
handlers.go: handleNotifyWatchLedger
    │
    ▼
watcher.go: startWatchingLedger → startWatching
    │
    ├─ validateCoordinatorDesignation       (checks Params.Coordinator has our address)
    ├─ multi.Coordinator.Subscribe(ctx, id) (one ethchannel sub per chain)
    └─ spawn handleEventsFromChain goroutine
            │
            ▼
        for ev := range *multi.AdjudicatorSubscription:
            *channel.RegisteredEvent  → handleRegisteredEvent
                                          - record chainDisputes[ledger]{version, state, timeout}
                                          - on first registration: spawn awaitFinalisationAndCoordinate
            *channel.ProgressedEvent  → handleProgressedEvent  (reset that chain's timeout)
            *channel.CoordinatedEvent → registry.remove(id) + return (channel finalised)
            *channel.ConcludedEvent   → registry.remove(id) + return

awaitFinalisationAndCoordinate (one per channel)
    │
    ▼
loop until every chainDispute is finalised:
    for each unfinalised key:
        wait time.Until(d.timeout) using time.After   ← WALL-CLOCK, not block.timestamp
        mark d.finalised = true
    │
    ▼
isReadyForCoordination()?
    │  yes
    ▼
coordinator.go: coordinate(ctx, ch)
    │
    ├─ selectCanonicalSignedState         (highest .State.Version across all chainDisputes)
    ├─ retrieveSignedSubStates            (walks canonical.State.Locked in DFS order;
    │                                       falls back to ch.archivedSubChStates if a
    │                                       sub-channel was de-registered)
    ├─ buildCoordSigs                     (coordSigs[0]=parent canonical, then DFS sub-states)
    └─ multi.Coordinator.Coordinate(ctx, req, signedSubStates, coordSigs)
              │
              ▼
        dispatch fans out one goroutine per asset's chain;
        each calls ethchannel.Coordinator.Coordinate → on-chain coordinate() tx
```

The wall-clock dispute timer is intentional — it decouples the coordinator from each
chain's block-production behaviour. Trade-off: in integration tests that fast-forward
the chain clock (e.g. Hardhat's `evm_increaseTime`), the coordinator still sleeps the
full `ChallengeDuration` seconds in real time. Use a short `ChallengeDuration` (1–15 s)
in tests rather than minutes.

### 13.6 Why the PoC keeps the inline simplified version

Driving the real service from the PoC requires three preconditions:

1. A running libp2p relay (`relay.perun.network:5574`) or an in-process `MockRelay`.
2. `coordID` set to the running service's peer.ID **at go-perun compile time**
   (current fork limitation, see 13.4).
3. `client.EnableCoordinationNotifier` installed before any `ProposeChannel`.

The inline `CoordinatorService` in 9.3 sidesteps all three by calling
`multi.Coordinator.Coordinate` directly — exactly what `coordinator/coordinator.go:
coordinate()` does after canonical-state selection. Both implementations produce the
same on-chain effect; the production service layers libp2p transport, event-driven
triggering, and channel lifecycle management on top.

For a faithful end-to-end demo, prefer the libp2p wiring of 13.4. For a fast,
self-contained correctness proof of the canonical-state-selection logic and the
contract invariants, the inline version of 9.3 is sufficient.


#### Test-shutdown subscription nil-receipt panic

The integration test (`coordinator/eth_integration_test.go: TestEthIntegration_FullFlow`)
originally panicked in `perun-eth-backend/channel/contractbackend.go:176`
(`receipt.Status` dereference) when `confirmNTimes` returned `(nil, nil)` after its
header subscription closed mid-call. Root cause: the coordinator's `coordinate()` runs
asynchronously, so its `ConfirmTransaction` was still in flight when the test torn
down the simulated backend. `sb.Close()` triggered `ErrClientQuit` in the RPC
subscription, which the go-perun stack converts to a nil error — propagated through
`errors.WithMessage(nil, …) == nil` and crashing on `receipt.Status`.

The fix combines two changes:
1. **Cleanup ordering** — `t.Cleanup` is LIFO, so the test registers `sb.Close` first,
   then `host.Wait(coordinateTimeout)`, then `host.stopWatching(id)`, then
   `sb.StopMining`. On teardown the order is StopMining → stopWatching → Wait →
   Close, so the coordinate goroutine drains before the backend dies.
2. **Auto-mining instead of hand-rolled clock advance** — `sb.StartMining(50 ms)`
   replaces the previous `for i := 0; i < 40; i++ { AdjustTime + Commit }` goroutine.
   The hand-rolled version raced with `SendTransaction`'s auto-commit and occasionally
   flooded the subscription channel; auto-mining matches the pattern used by
   `perun-eth-backend`'s own `TestCoordinate_Basic`.

---

## Audit Notes: Corrections from Previous Version

The following errors were present in the original document and have been fixed:

| #   | Original error                                                                                  | Correction                                                                                                                       |
| --- | ----------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| 1   | Import path `perun.network/perun-eth-backend/adjudicator`                                       | Correct: `github.com/perun-network/perun-eth-backend/channel`                                                                    |
| 2   | `ethadj.NewAdjudicator(acc, client, addr, chainID)`                                             | Correct: `ethchannel.NewAdjudicator(ContractBackend, contract, receiver, txSender, gasLimit)`                                    |
| 3   | `NewCoordinatorAdjudicator` — doesn't exist                                                     | Correct: `ethchannel.NewCoordinator(...)` (same signature as `NewAdjudicator`)                                                   |
| 4   | `ethfund.NewFunder(client, chainID, acc, assetAddr)`                                            | Correct: `ethchannel.NewFunder(ContractBackend)` + `RegisterAsset` calls                                                         |
| 5   | Custom `EthMultiAsset` struct                                                                   | Correct: `ethchannel.NewAsset(chainID, assetHolderAddr)` implements `multi.Asset`                                                |
| 6   | Custom `EthLedgerBackendID` struct                                                              | Correct: `ethchannel.MakeLedgerBackendID(chainID)`                                                                               |
| 7   | `ethwallet.NewWallet()` (doesn't exist)                                                         | Correct: `simplewallet.NewWallet(privateKey...)` from `wallet/simple` package                                                    |
| 8   | `hardhat_mine` advances block numbers only                                                      | Correct: timeouts use `block.timestamp`; use `evm_increaseTime + evm_mine`                                                       |
| 9   | `register()` error: `"channel already coordinated"`                                             | Correct: `"incorrect phase"` (from `registerSingle: dispute.phase == DISPUTE`)                                                   |
| 10  | `coordinate()` only allows DISPUTE → COORDINATED                                                | Correct: also allows COORDINATED → COORDINATED (idempotent, checks stateHash)                                                    |
| 11  | `State.Params()` method used                                                                    | Correct: `Params` must be stored separately; `*channel.State` has no `Params()` method                                           |
| 12  | Solidity `register()` ABI shows wrong params                                                    | Correct signature: `register(SignedState, SignedState[])`                                                                        |
| 13  | Go version `≥ 1.22`                                                                             | Correct: `≥ 1.24` (go.mod uses `go 1.24.0`)                                                                                      |
| 14  | Missing `go-ethereum-hdwallet` dependency                                                       | Added for Hardhat mnemonic key derivation                                                                                        |
| 15  | Coordinator "races" to prevent Bob's v2 (Scenario A)                                            | Correct: coordinator coordinates the HIGHEST known version uniformly (Scenario B) — this is what the actual tests implement      |
| 16  | `Watch(alice.Client)` — `*client.Client` does not implement `AdjudicatorEventHandler`           | Correct: `Participant` now implements `HandleAdjudicatorEvent`; Watch calls use `alice`/`bob` directly                           |
| 17  | Both `AdvanceTime` calls before `Register(v2)` — Chain A already frozen when Bob registers      | Correct: Chain B's time advanced first, Bob registers v2 on Chain A while its window is still open, then Chain A's time advanced |
| 18  | `_ = assert.True` stub assertions never ran                                                     | Correct: replaced with real `ctest.EqualBalancesWithDelta` calls in both tests                                                   |
| 19  | Missing `"github.com/ethereum/go-ethereum/crypto"` in participant.go and coordinator.go imports | Correct: added; imports now sorted stdlib / external / internal                                                                  |
| 20  | `challengeDuration` comment said "seconds" without qualification                                | Correct: note added that ETH backend uses seconds; go-perun MockBackend uses milliseconds                                        |
| 21  | No wait for `CoordinatedEvent` before calling `Settle`                                          | Correct: `require.Eventually` polls `chAliceBob.Phase() == channel.Coordinated` before Settle                                    |
