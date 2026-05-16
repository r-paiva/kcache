// SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers
//
// SPDX-License-Identifier: Apache-2.0

package iface

import (
	"testing"

	"github.com/vishvananda/netlink"
)

// mockLink implements netlink.Link with a fixed name and type.
type mockLink struct {
	name    string
	linkTyp string
}

func (m *mockLink) Attrs() *netlink.LinkAttrs { return &netlink.LinkAttrs{Name: m.name} }
func (m *mockLink) Type() string              { return m.linkTyp }

func veth(name string) *mockLink  { return &mockLink{name: name, linkTyp: "veth"} }
func dummy(name string) *mockLink { return &mockLink{name: name, linkTyp: "dummy"} }

func TestIsPodVeth(t *testing.T) {
	tests := []struct {
		desc string
		link *mockLink
		want bool
	}{
		// ── Flannel (veth + 7+ hex chars) ────────────────────────────────────
		{"flannel exact minimum (7 hex)", veth("veth1234567"), true},
		{"flannel typical (8 hex)", veth("veth1a2b3c4d"), true},
		{"flannel long", veth("veth1a2b3c4d5e6f"), true},
		{"flannel too short (6 hex)", veth("veth123456"), false},
		{"flannel uppercase hex rejected", veth("vethABCDEF1"), false},
		{"flannel non-hex chars", veth("vethxyz12345"), false},

		// ── Cilium (lxc + 8+ hex chars) ──────────────────────────────────────
		{"cilium exact minimum (8 hex)", veth("lxc12345678"), true},
		{"cilium typical", veth("lxcaf76531335cb"), true},
		{"cilium too short (7 hex)", veth("lxc1234567"), false},

		// ── Calico (cali + 7+ hex chars) ─────────────────────────────────────
		{"calico exact minimum (7 hex)", veth("cali1234567"), true},
		{"calico typical", veth("cali4b85ddffbda"), true},
		{"calico too short (6 hex)", veth("cali123456"), false},

		// ── Cilium internal interfaces (must be excluded) ─────────────────────
		{"cilium_host excluded", veth("cilium_host"), false},
		{"cilium_net excluded", veth("cilium_net"), false},
		{"lxc_health excluded", veth("lxc_health"), false},
		{"lxc_netdev excluded", veth("lxc_netdev"), false},

		// ── Common host interfaces ────────────────────────────────────────────
		{"eth0 rejected", veth("eth0"), false},
		{"lo rejected", veth("lo"), false},
		{"docker0 rejected", veth("docker0"), false},
		{"flannel.1 rejected", veth("flannel.1"), false},

		// ── Wrong link type ───────────────────────────────────────────────────
		{"veth name but wrong type", dummy("veth1a2b3c4d"), false},
		{"lxc name but wrong type", dummy("lxcaf76531335cb"), false},
		{"cali name but wrong type", dummy("cali4b85ddffbda"), false},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if got := isPodVeth(tt.link); got != tt.want {
				t.Errorf("isPodVeth(%q, type=%q) = %v, want %v",
					tt.link.name, tt.link.linkTyp, got, tt.want)
			}
		})
	}
}

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

func TestDetach_EmptyLinkSlice(t *testing.T) {
	// Legacy cls_bpf path stores an empty slice; detach must not panic.
	mgr := &Manager{
		links: map[int][]linkCloser{
			3: {}, // legacy attach: no TCX link objects
		},
	}
	mgr.detach(3, "veth33333333")
	if _, ok := mgr.links[3]; ok {
		t.Error("expected ifindex 3 to be removed")
	}
}
