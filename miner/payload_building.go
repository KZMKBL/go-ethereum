// Copyright 2022 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>

package miner

import (
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/beacon"
	"github.com/ethereum/go-ethereum/core/types"
)

// BuildPayloadArgs contains the provided parameters for building payload.
// Check engine-api specification for more details.
// https://github.com/ethereum/execution-apis/blob/main/src/engine/specification.md#payloadattributesv1
type BuildPayloadArgs struct {
	Parent       common.Hash    // The parent block to build payload on top
	Timestamp    uint64         // The provided timestamp of generated payload
	FeeRecipient common.Address // The provided recipient address for collecting transaction fee
	Random       common.Hash    // The provided randomness value
}

// Payload wraps the built payload(block waiting for sealing). According to the
// engine-api specification, EL should build the initial version of the payload
// which has an empty transaction set and then keep update it in order to maximize
// the revenue. Therefore, the empty-block here is always available and full-block
// will be set/updated afterwards.
type Payload struct {
	empty    *types.Block
	full     *types.Block
	fullFees *big.Int
	stop     chan struct{}
	lock     *sync.Mutex
	cond     *sync.Cond
}

// newPayload initializes the payload object.
func newPayload(empty *types.Block) *Payload {
	lock := new(sync.Mutex)
	return &Payload{
		empty: empty,
		stop:  make(chan struct{}),
		lock:  lock,
		cond:  sync.NewCond(lock),
	}
}

// update updates the full-block with latest built version.
func (payload *Payload) update(block *types.Block, fees *big.Int) {
	payload.lock.Lock()
	defer payload.lock.Unlock()

	select {
	case <-payload.stop:
		return // reject stale update
	default:
	}
	// Ensure the newly provided full block has a higher transaction fee.
	// In post-merge stage, there is no uncle reward anymore and transaction
	// fee(apart from the mev revenue) is the only indicator for comparison.
	if payload.full == nil || fees.Cmp(payload.fullFees) > 0 {
		payload.full = block
		payload.fullFees = fees
	}
	payload.cond.Broadcast() // fire signal for notifying full block
}

// Resolve returns the latest built payload and also terminates the background
// thread for updating payload. It's safe to be called multiple times.
func (payload *Payload) Resolve() *beacon.ExecutableDataV1 {
	payload.lock.Lock()
	defer payload.lock.Unlock()

	select {
	case <-payload.stop:
	default:
		close(payload.stop)
	}
	if payload.full != nil {
		return beacon.BlockToExecutableData(payload.full)
	}
	return beacon.BlockToExecutableData(payload.empty)
}

// ResolveEmpty is basically identical to Resolve, but it expects empty block only.
// It's only used in tests.
func (payload *Payload) ResolveEmpty() *beacon.ExecutableDataV1 {
	payload.lock.Lock()
	defer payload.lock.Unlock()

	return beacon.BlockToExecutableData(payload.empty)
}

// ResolveFull is basically identical to Resolve, but it expects full block only.
// It's only used in tests.
func (payload *Payload) ResolveFull() *beacon.ExecutableDataV1 {
	payload.lock.Lock()
	defer payload.lock.Unlock()

	if payload.full == nil {
		select {
		case <-payload.stop:
			return nil
		default:
		}
		payload.cond.Wait()
	}
	return beacon.BlockToExecutableData(payload.full)
}

// buildPayload builds the payload according to the provided parameters.
func (w *worker) buildPayload(args *BuildPayloadArgs) (*Payload, error) {
	// Build the initial version with no transaction included. It should be fast
	// enough to run. The empty payload can at least make sure there is something
	// to deliver for not missing slot.
	empty, _, err := w.getSealingBlock(args.Parent, args.Timestamp, args.FeeRecipient, args.Random, true)
	if err != nil {
		return nil, err
	}
	// Construct a payload object for return.
	payload := newPayload(empty)

	// Spin up a routine for updating the payload in background. This strategy
	// can maximum the revenue for including transactions with highest fee.
	go func() {
		// Setup the timer for re-building the payload. The initial clock is kept
		// for triggering process immediately.
		timer := time.NewTimer(0)
		defer timer.Stop()

		// Setup the timer for terminating the process if SECONDS_PER_SLOT (12s in
		// the Mainnet configuration) have passed since the point in time identified
		// by the timestamp parameter.
		endTimer := time.NewTimer(time.Second * 12)

		for {
			select {
			case <-timer.C:
				block, fees, err := w.getSealingBlock(args.Parent, args.Timestamp, args.FeeRecipient, args.Random, false)
				if err == nil {
					payload.update(block, fees)
				}
				timer.Reset(w.recommit)
			case <-payload.stop:
				return
			case <-endTimer.C:
				return
			}
		}
	}()
	return payload, nil
}
