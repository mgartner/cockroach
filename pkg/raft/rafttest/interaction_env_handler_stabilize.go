// This code has been modified from its original form by The Cockroach Authors.
// All modifications are Copyright 2024 The Cockroach Authors.
//
// Copyright 2019 The etcd Authors
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

package rafttest

import (
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/raft/raftpb"
	"github.com/cockroachdb/datadriven"
)

func (env *InteractionEnv) handleStabilize(t *testing.T, d datadriven.TestData) error {
	idxs := nodeIdxs(t, d) // skips key=value args
	for _, arg := range d.CmdArgs {
		for i := range arg.Vals {
			switch arg.Key {
			case "log-level":
				//nolint:deferloop
				defer func(old int) {
					env.Output.Lvl = old
				}(env.Output.Lvl)
				var level string
				arg.Scan(t, i, &level)
				if err := env.LogLevel(level); err != nil {
					return err
				}
			}
		}
	}
	return env.Stabilize(idxs...)
}

// Stabilize repeatedly runs Ready handling on and message delivery to the set
// of nodes specified via the idxs slice until reaching a fixed point.
func (env *InteractionEnv) Stabilize(idxs ...int) error {
	var nodes []*Node
	if len(idxs) != 0 {
		for _, idx := range idxs {
			nodes = append(nodes, &env.Nodes[idx])
		}
	} else {
		for i := range env.Nodes {
			nodes = append(nodes, &env.Nodes[i])
		}
	}

	for {
		done := true
		for _, rn := range nodes {
			if rn.HasReady() {
				idx := int(rn.Status().ID - 1)
				fmt.Fprintf(env.Output, "> %d handling Ready\n", idx+1)
				var err error
				env.withIndent(func() { err = env.ProcessReady(idx) })
				if err != nil {
					return err
				}
				done = false
			}
		}
		for _, rn := range nodes {
			if id := rn.Status().ID; env.hasMessages(id) {
				fmt.Fprintf(env.Output, "> %d receiving messages\n", id)
				env.withIndent(func() { env.DeliverMsgs(-1 /* typ */, Recipient{ID: id}) })
				done = false
			}
		}
		for _, rn := range nodes {
			idx := int(rn.Status().ID - 1)
			if len(rn.AppendWork) > 0 {
				fmt.Fprintf(env.Output, "> %d processing append thread\n", idx+1)
				for len(rn.AppendWork) > 0 {
					var err error
					env.withIndent(func() { err = env.ProcessAppendThread(idx) })
					if err != nil {
						return err
					}
				}
				done = false
			}
		}
		for _, rn := range nodes {
			idx := int(rn.Status().ID - 1)
			if !rn.ApplyWork.Empty() {
				fmt.Fprintf(env.Output, "> %d processing apply thread\n", idx+1)
				for !rn.ApplyWork.Empty() {
					env.withIndent(func() { env.ProcessApplyThread(idx) })
				}
				done = false
			}
		}
		if done {
			return nil
		}
	}
}

func (env *InteractionEnv) hasMessages(id raftpb.PeerID) bool {
	if int(id) <= len(env.Nodes) && len(env.Nodes[id-1].AppendAcks) > 0 {
		return true
	}
	msgs, _ := splitMsgs(env.Messages, id, -1 /* typ */)
	return len(msgs) > 0
}

// splitMsgs extracts messages for the given recipient of the given type (-1 for
// all types) from msgs, and returns them along with the remainder of msgs.
func splitMsgs(
	msgs []raftpb.Message, to raftpb.PeerID, typ raftpb.MessageType,
) (toMsgs []raftpb.Message, rmdr []raftpb.Message) {
	// NB: this method does not reorder messages.
	for _, msg := range msgs {
		if msg.To == to && (typ < 0 || msg.Type == typ) {
			toMsgs = append(toMsgs, msg)
		} else {
			rmdr = append(rmdr, msg)
		}
	}
	return toMsgs, rmdr
}
