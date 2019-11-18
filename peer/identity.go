// Copyright (c) 2019 The Perun Authors. All rights reserved.
// This file is part of go-perun. Use of this source code is governed by a
// MIT-style license that can be found in the LICENSE file.

package peer

import (
	"io"

	"github.com/pkg/errors"

	"perun.network/go-perun/wallet"
	"perun.network/go-perun/wire/msg"
)

func init() {
	msg.RegisterDecoder(msg.AuthResponse,
		func(r io.Reader) (msg.Msg, error) {
			var m AuthResponseMsg
			return &m, m.Decode(r)
		})
}

// Identity is a node's permanent Perun identity, which is used to establish
// authenticity within the Perun peer-to-peer network. For now, it is just a
// stub.
type Identity = wallet.Account

// ExchangeAddrs exchanges Perun addresses of peers. It's the initial protocol
// that is run when a new peer connection is established. It returns the address
// of the peer on the other end of the connection.
//
// In the future, ExchangeAddrs will be replaced by Authenticate to run a proper
// authentication protocol.  The protocol will then exchange Perun addresses and
// establish authenticity.
func ExchangeAddrs(id Identity, conn Conn) (Address, error) {
	sent := make(chan error, 1)
	if id == nil || conn == nil {
		// catch a nil id early to not cause a panic in the following go routine
		panic("Authenticate(): nil Identity or Conn")
	}
	go func() { sent <- conn.Send(NewAuthResponseMsg(id)) }()

	if m, err := conn.Recv(); err != nil {
		return nil, errors.WithMessage(err, "Failed to receive message")
	} else if addrM, ok := m.(*AuthResponseMsg); !ok {
		return nil, errors.Errorf("Expected AuthResponse wire msg, got %v", m.Type())
	} else {
		err := <-sent // Wait until the message was sent.
		return addrM.Address, err
	}
}

var _ msg.Msg = (*AuthResponseMsg)(nil)

// AuthResponseMsg is the response message in the peer authentication protocol.
type AuthResponseMsg struct {
	Address Address
}

func (m *AuthResponseMsg) Type() msg.Type {
	return msg.AuthResponse
}

func (m *AuthResponseMsg) Encode(w io.Writer) error {
	return m.Address.Encode(w)
}

func (m *AuthResponseMsg) Decode(r io.Reader) (err error) {
	m.Address, err = wallet.DecodeAddress(r)
	return
}

// NewAuthResponseMsg creates an authentication response message.
// In the future, it will also take an authentication challenge message as
// additional argument.
func NewAuthResponseMsg(id Identity) msg.Msg {
	return &AuthResponseMsg{id.Address()}
}