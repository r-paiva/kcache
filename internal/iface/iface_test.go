// SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
//
// SPDX-License-Identifier: Apache-2.0

package iface

import (
	"testing"
)

// mockCloser records whether Close was called.
type mockCloser struct{ closed bool }

func (m *mockCloser) Close() error {
	m.closed = true
	return nil
}

func TestDetach_UnknownIfindex(t *testing.T) {
	mgr := &Manager{links: make(map[int][]linkCloser)}
	mgr.detach(99, "veth99")
	if len(mgr.links) != 0 {
		t.Error("links map should remain empty")
	}
}

func TestDetach_ClosesLinksAndRemovesEntry(t *testing.T) {
	a, b := &mockCloser{}, &mockCloser{}
	mgr := &Manager{
		links: map[int][]linkCloser{
			5: {a, b},
		},
	}

	mgr.detach(5, "veth1a2b3c4d")

	if !a.closed || !b.closed {
		t.Error("expected both links to be closed")
	}
	if _, ok := mgr.links[5]; ok {
		t.Error("expected ifindex 5 to be removed from links map")
	}
}

func TestDetach_LeavesOtherEntriesIntact(t *testing.T) {
	keep := &mockCloser{}
	mgr := &Manager{
		links: map[int][]linkCloser{
			1: {&mockCloser{}},
			2: {keep},
		},
	}

	mgr.detach(1, "veth11111111")

	if _, ok := mgr.links[1]; ok {
		t.Error("expected ifindex 1 to be removed")
	}
	if _, ok := mgr.links[2]; !ok {
		t.Error("expected ifindex 2 to remain")
	}
	if keep.closed {
		t.Error("expected ifindex 2's link to remain open")
	}
}

// TestReconcile_DetachesUndesiredKeepsDesired verifies that veths absent from
// the desired set are detached while desired ones already attached are left
// untouched. (Attaching new ifindexes needs a real kernel and is covered by the
// on-node live test.)
func TestReconcile_DetachesUndesiredKeepsDesired(t *testing.T) {
	keep, drop := &mockCloser{}, &mockCloser{}
	mgr := &Manager{
		links: map[int][]linkCloser{
			10: {keep},
			20: {drop},
		},
	}

	mgr.Reconcile(map[int]struct{}{10: {}})

	if _, ok := mgr.links[10]; !ok {
		t.Error("expected desired ifindex 10 to remain attached")
	}
	if keep.closed {
		t.Error("expected ifindex 10's link to stay open")
	}
	if _, ok := mgr.links[20]; ok {
		t.Error("expected undesired ifindex 20 to be detached")
	}
	if !drop.closed {
		t.Error("expected ifindex 20's link to be closed")
	}
}
