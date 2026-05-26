package libp2p

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	coreprotocol "github.com/libp2p/go-libp2p/core/protocol"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/pkg/errors"
	"perun.network/go-perun/channel"
	"perun.network/go-perun/channel/multi"
)

const (
	notifyWatchLedgerProtocolID = "/coordinator/notify-watch-ledger/1.0.0"
	notifyWatchSubProtocolID    = "/coordinator/notify-watch-sub/1.0.0"
	notifyStopWatchProtocolID   = "/coordinator/notify-stop-watch/1.0.0"

	responseStatusOK    = "ok"
	responseStatusError = "error"
)

// Response is the generic protocol response envelope returned by request and
// witness endpoints.
type Response struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type NotifyWatchLedgerChannelRequest struct {
	SignedState channel.SignedState `json:"signed_state"`
}

type NotifyWatchSubChannelRequest struct {
	ParentID    channel.ID          `json:"parent_id"`
	SignedState channel.SignedState `json:"signed_state"`
}

type NotifyStopWatchRequest struct {
	ID channel.ID `json:"id"`
}

var _ multi.CoordinatorNotifier = (*RelayCoordinatorNotifier)(nil)

// RelayCoordinatorNotifier is a simple implementation of CoordinatorNotifier that relays notifications to the coordinator via the Account.
type RelayCoordinatorNotifier struct {
	account     *Account
	coordPeerID peer.ID
}

// NewRelayCoordinatorNotifier creates a new RelayCoordinatorNotifier with the
// given Account and the coordinator's libp2p peer.ID.
func NewRelayCoordinatorNotifier(acc *Account, coordPeerID peer.ID) *RelayCoordinatorNotifier {
	return &RelayCoordinatorNotifier{account: acc, coordPeerID: coordPeerID}
}

// NotifyWatchLedgerChannel sends a notification to the coordinator to start watching a ledger channel.
func (r *RelayCoordinatorNotifier) NotifyWatchLedgerChannel(ctx context.Context, signedState channel.SignedState) error {
	if r.coordPeerID == (peer.ID)("") {
		return errors.New("coordinator peer ID not configured; use NewRelayCoordinatorNotifierWithPeerID")
	}
	s, err := r.account.newRelayStream(ctx, r.coordPeerID, notifyWatchLedgerProtocolID)
	if err != nil {
		return err
	}
	defer s.Close()

	req := NotifyWatchLedgerChannelRequest{SignedState: signedState}
	resp := &Response{}

	err = json.NewEncoder(s).Encode(req)
	if err != nil {
		return errors.WithMessage(err, "encoding notify watch ledger channel request")
	}

	err = s.CloseWrite()
	if err != nil {
		return errors.WithMessage(err, "half-closing coordinator request stream")
	}

	err = json.NewDecoder(s).Decode(resp)
	if err != nil {
		return errors.WithMessage(err, "decoding coordinator response")
	}

	status := normalizeStatus(resp.Status)
	if status == responseStatusOK {
		return nil
	}
	if resp.Reason != "" {
		return errors.New(resp.Reason)
	}
	if status == "" {
		return errors.New("empty status in coordination response")
	}

	return errors.Errorf("coordination request failed with status %q", resp.Status)
}

// NotifyWatchSubChannel sends a notification to the coordinator to start watching a sub channel.
func (r *RelayCoordinatorNotifier) NotifyWatchSubChannel(ctx context.Context, parent channel.ID, signedState channel.SignedState) error {
	if r.coordPeerID == (peer.ID)("") {
		return errors.New("coordinator peer ID not configured; use NewRelayCoordinatorNotifierWithPeerID")
	}
	s, err := r.account.newRelayStream(ctx, r.coordPeerID, notifyWatchSubProtocolID)
	if err != nil {
		return err
	}
	defer s.Close()

	req := NotifyWatchSubChannelRequest{ParentID: parent, SignedState: signedState}
	resp := &Response{}

	err = json.NewEncoder(s).Encode(req)
	if err != nil {
		return errors.WithMessage(err, "encoding notify watch sub channel request")
	}

	err = s.CloseWrite()
	if err != nil {
		return errors.WithMessage(err, "half-closing coordinator request stream")
	}

	err = json.NewDecoder(s).Decode(resp)
	if err != nil {
		return errors.WithMessage(err, "decoding coordinator response")
	}

	status := normalizeStatus(resp.Status)
	if status == responseStatusOK {
		return nil
	}
	if resp.Reason != "" {
		return errors.New(resp.Reason)
	}
	if status == "" {
		return errors.New("empty status in coordination response")
	}

	return errors.Errorf("coordination request failed with status %q", resp.Status)
}

// NotifyStopWatch sends a notification to the coordinator to stop watching a channel.
func (r *RelayCoordinatorNotifier) NotifyStopWatch(ctx context.Context, id channel.ID) error {
	if r.coordPeerID == (peer.ID)("") {
		return errors.New("coordinator peer ID not configured; use NewRelayCoordinatorNotifierWithPeerID")
	}
	s, err := r.account.newRelayStream(ctx, r.coordPeerID, notifyStopWatchProtocolID)
	if err != nil {
		return err
	}
	defer s.Close()

	req := NotifyStopWatchRequest{ID: id}
	resp := &Response{}
	err = json.NewEncoder(s).Encode(req)
	if err != nil {
		return errors.WithMessage(err, "encoding notify stop watch request")
	}

	err = s.CloseWrite()
	if err != nil {
		return errors.WithMessage(err, "half-closing coordinator request stream")
	}

	err = json.NewDecoder(s).Decode(resp)
	if err != nil {
		return errors.WithMessage(err, "decoding coordinator response")
	}

	status := normalizeStatus(resp.Status)
	if status == responseStatusOK {
		return nil
	}
	if resp.Reason != "" {
		return errors.New(resp.Reason)
	}
	if status == "" {
		return errors.New("empty status in coordination response")
	}

	return errors.Errorf("coordination request failed with status %q", resp.Status)
}

func (acc *Account) newRelayStream(ctx context.Context, remoteID peer.ID, protocolID coreprotocol.ID) (network.Stream, error) {
	fullAddr := acc.relayAddr + "/p2p/" + relayID + "/p2p-circuit/p2p/" + remoteID.String()
	peerMultiAddr, err := ma.NewMultiaddr(fullAddr)
	if err != nil {
		return nil, errors.WithMessage(err, "parsing relay circuit multiaddress")
	}

	peerAddrInfo, err := peer.AddrInfoFromP2pAddr(peerMultiAddr)
	if err != nil {
		return nil, errors.WithMessage(err, "converting relay circuit multiaddress")
	}

	err = acc.Connect(ctx, *peerAddrInfo)
	if err != nil {
		return nil, errors.WithMessage(err, "connecting to remote coordinator")
	}

	limitedConnTag := string(protocolID)
	if len(limitedConnTag) > 0 && limitedConnTag[0] == '/' {
		limitedConnTag = limitedConnTag[1:]
	}

	s, err := acc.NewStream(network.WithAllowLimitedConn(ctx, limitedConnTag), peerAddrInfo.ID, protocolID)
	if err != nil {
		return nil, errors.WithMessage(err, "creating coordination stream")
	}

	return s, nil
}

func normalizeStatus(status string) string {
	return strings.ToLower(strings.TrimSpace(status))
}
