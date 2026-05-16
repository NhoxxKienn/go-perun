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

package multi

import (
	"context"

	"perun.network/go-perun/channel"
)

// Subscribe creates a new multi-ledger AdjudicatorSubscription.
func (a *Adjudicator) Subscribe(ctx context.Context, chID channel.ID) (channel.AdjudicatorSubscription, error) {
	asub := &AdjudicatorSubscription{
		events: make(chan LedgerAdjudicatorEvent),
		errors: make(chan error),
		done:   make(chan struct{}),
	}

	for key, la := range a.adjudicators {
		sub, err := la.Subscribe(ctx, chID)
		if err != nil {
			asub.Close()
			return nil, err
		}
		asub.subs = append(asub.subs, sub)

		go func() {
			for {
				e := sub.Next()
				select {
				case asub.events <- LedgerAdjudicatorEvent{LedgerKey: key, AdjudicatorEvent: e}:
				case <-asub.done:
					return
				}
			}
		}()

		go func() {
			asub.errors <- sub.Err()
		}()
	}

	return asub, nil
}

// LedgerAdjudicatorEvent is a wrapper for channel.AdjudicatorEvent with the ledger key.
type LedgerAdjudicatorEvent struct {
	LedgerKey LedgerBackendKey
	channel.AdjudicatorEvent
}

// AdjudicatorSubscription is a multi-ledger adjudicator subscription.
type AdjudicatorSubscription struct {
	subs   []channel.AdjudicatorSubscription
	events chan LedgerAdjudicatorEvent
	errors chan error
	done   chan struct{}
}

// Next returns the next event.
func (s *AdjudicatorSubscription) Next() channel.AdjudicatorEvent {
	e, _, ok := s.NextWithKey()
	if !ok {
		return nil
	}
	return e
}

// NextWithKey is the coordinator-facing variant of Next.
// It returns the same concrete inner event as Next, together with the
// LedgerBackendKey identifying which chain emitted it.
// The boolean is false when the subscription is closed.
//
// Usage (coordinator only):
//
//	if sub, ok := rawSub.(*multi.AdjudicatorSubscription); ok {
//	    e, key, ok := sub.NextWithKey()
//	}
func (s *AdjudicatorSubscription) NextWithKey() (channel.AdjudicatorEvent, LedgerBackendKey, bool) {
	select {
	case e := <-s.events:
		return e.AdjudicatorEvent, e.LedgerKey, true
	case <-s.done:
		return nil, LedgerBackendKey{}, false
	}
}

// Err blocks until an error occurred and returns it.
func (s *AdjudicatorSubscription) Err() error {
	for range len(s.subs) {
		err := <-s.errors
		if err != nil {
			return err
		}
	}
	return nil
}

// Close closes the subscription.
func (s *AdjudicatorSubscription) Close() error {
	for _, sub := range s.subs {
		sub.Close()
	}

	close(s.done)
	return nil
}
