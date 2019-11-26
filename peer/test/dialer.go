// Copyright (c) 2019 The Perun Authors. All rights reserved.
// This file is part of go-perun. Use of this source code is governed by a
// MIT-style license that can be found in the LICENSE file.

package test

import (
	"context"
	"net"

	"github.com/pkg/errors"

	"perun.network/go-perun/peer"
	"perun.network/go-perun/pkg/sync"
)

var _ peer.Dialer = (*Dialer)(nil)

// Dialer is a test dialer that can dial connections to Listeners via a ConnHub.
type Dialer struct {
	hub *ConnHub

	sync.Closer
}

func (d *Dialer) Dial(ctx context.Context, address peer.Address) (peer.Conn, error) {
	if d.IsClosed() {
		return nil, errors.New("dialer closed")
	}

	select {
	case <-ctx.Done():
		return nil, errors.New("manually aborted")
	default:
	}

	l, ok := d.hub.find(address)
	if !ok {
		return nil, errors.Errorf("peer with address %v not found", address)
	}

	local, remote := net.Pipe()
	l.Put(peer.NewIoConn(remote))
	return peer.NewIoConn(local), nil
}

func (d *Dialer) Close() error {
	return errors.WithMessage(d.Closer.Close(), "dialer was already closed")
}
