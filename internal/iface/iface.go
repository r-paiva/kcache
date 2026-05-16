// SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers
//
// SPDX-License-Identifier: Apache-2.0

// Package iface manages TC BPF attachment to pod veth interfaces.
// It attaches tc_ingress and tc_egress programs to every veth it discovers,
// and watches for new veths as pods start.
// (≥6.6), which places kcache in the same program chain as Cilium rather
// than in the legacy cls_bpf chain. On older kernels it falls back to the
// legacy cls_bpf filter mechanism.
package iface

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"syscall"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// linkCloser is the only operation we need from an attached BPF link.
// Using a narrow interface keeps the Manager testable without a real kernel.
type linkCloser interface {
	Close() error
}

// podVethRe matches the naming patterns CNIs use for pod-facing veth interfaces:
//   - Flannel / standard containerd: veth + 8 hex chars  (e.g. veth1a2b3c4d)
//   - Cilium veth mode:              lxc  + 12 hex chars (e.g. lxcaf76531335cb)
//   - Calico:                        cali + 10 hex chars (e.g. cali1a2b3c4d5e)
// are deliberately excluded because attaching TC programs to them breaks Cilium's
// internal packet forwarding.
var podVethRe = regexp.MustCompile(`^(veth[0-9a-f]{7,}|lxc[0-9a-f]{8,}|cali[0-9a-f]{7,})$`)

// Manager attaches TC BPF programs to pod veth interfaces and watches for new ones.
type Manager struct {
	ingress *ebpf.Program
	egress  *ebpf.Program

	mu    sync.Mutex
	links map[int][]linkCloser // ifindex → open TCX links for that interface
}

func New(ingress, egress *ebpf.Program) *Manager {
	return &Manager{
		ingress: ingress,
		egress:  egress,
		links:   make(map[int][]linkCloser),
	}
}

// AttachExisting attaches to all veth interfaces that are already present.
func (m *Manager) AttachExisting() error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("list links: %w", err)
	}
	for _, l := range links {
		if !isPodVeth(l) {
			continue
		}
		if err := m.attach(l); err != nil {
			slog.Warn("attach TC to existing veth", "iface", l.Attrs().Name, "err", err)
		}
	}
	return nil
}

// Watch subscribes to netlink link events and attaches TC programs to new veths.
// Blocks until ctx is cancelled.
func (m *Manager) Watch(ctx context.Context) {
	ch := make(chan netlink.LinkUpdate, 16)
	done := make(chan struct{})
	if err := netlink.LinkSubscribe(ch, done); err != nil {
		slog.Error("netlink link subscribe", "err", err)
		return
	}
	defer close(done)

	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-ch:
			if !ok {
				return
			}
			switch update.Header.Type {
			case unix.RTM_NEWLINK:
				if !isPodVeth(update.Link) {
					continue
				}
				if err := m.attach(update.Link); err != nil {
					slog.Warn("attach TC to new veth", "iface", update.Link.Attrs().Name, "err", err)
				}
			case unix.RTM_DELLINK:
				m.detach(update.Link.Attrs().Index, update.Link.Attrs().Name)
			}
		}
	}
}

// detach removes a deleted interface from the tracking map and closes any open
// TCX links for it. The kernel has already removed the TC programs when the
// interface was destroyed; we just need to release the file descriptors and
// clear the entry so the ifindex can be reused by a future pod's veth.
func (m *Manager) detach(ifindex int, name string) {
	m.mu.Lock()
	ls, ok := m.links[ifindex]
	delete(m.links, ifindex)
	m.mu.Unlock()
	if !ok {
		return
	}
	for _, l := range ls {
		l.Close()
	}
	slog.Info("TC BPF detached", "iface", name)
}

func isPodVeth(l netlink.Link) bool {
	return l.Type() == "veth" && podVethRe.MatchString(l.Attrs().Name)
}

func (m *Manager) attach(l netlink.Link) error {
	idx := l.Attrs().Index
	name := l.Attrs().Name

	m.mu.Lock()
	if _, already := m.links[idx]; already {
		m.mu.Unlock()
		return nil // already attached to this interface
	}
	m.mu.Unlock()

	var ls []linkCloser

	// Try TCX first — puts us in the same program chain as Cilium (kernel ≥6.6).
	ing, egr, err := attachTCX(idx, m.ingress, m.egress)
	if err != nil {
		// Fall back to legacy cls_bpf filter if TCX is not supported.
		slog.Debug("TCX not available, falling back to cls_bpf", "iface", name, "err", err)
		if err2 := attachLegacy(idx, m.ingress, m.egress); err2 != nil {
			return fmt.Errorf("attach TC (tcx: %v, legacy: %w)", err, err2)
		}
	} else {
		ls = append(ls, ing, egr)
	}

	m.mu.Lock()
	m.links[idx] = ls
	m.mu.Unlock()

	slog.Info("TC BPF attached", "iface", name)
	return nil
}

// attachTCX attaches programs using the TCX bpf_link mechanism (kernel ≥6.6).
// Both programs are inserted at the HEAD of their respective chains so they
// run before any existing programs (e.g. Cilium's cil_from_container), which
// is required because Cilium returns TC_ACT_REDIRECT and would terminate the
// chain before kcache's program gets to run.
// Returns the ingress and egress links, which must be kept alive.
func attachTCX(ifindex int, ingress, egress *ebpf.Program) (link.Link, link.Link, error) {
	ing, err := link.AttachTCX(link.TCXOptions{
		Interface: ifindex,
		Program:   ingress,
		Attach:    ebpf.AttachTCXIngress,
		Anchor:    link.Head(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("tcx ingress: %w", err)
	}

	egr, err := link.AttachTCX(link.TCXOptions{
		Interface: ifindex,
		Program:   egress,
		Attach:    ebpf.AttachTCXEgress,
		Anchor:    link.Head(),
	})
	if err != nil {
		ing.Close()
		return nil, nil, fmt.Errorf("tcx egress: %w", err)
	}

	return ing, egr, nil
}

// attachLegacy attaches programs using the traditional cls_bpf TC filter mechanism.
func attachLegacy(ifindex int, ingress, egress *ebpf.Program) error {
	if err := ensureClsact(ifindex); err != nil {
		return fmt.Errorf("clsact qdisc: %w", err)
	}
	if err := replaceFilter(ifindex, ingress, netlink.HANDLE_MIN_INGRESS, "kcache/ingress"); err != nil {
		return fmt.Errorf("ingress filter: %w", err)
	}
	if err := replaceFilter(ifindex, egress, netlink.HANDLE_MIN_EGRESS, "kcache/egress"); err != nil {
		return fmt.Errorf("egress filter: %w", err)
	}
	return nil
}

func ensureClsact(ifindex int) error {
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: ifindex,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	err := netlink.QdiscAdd(qdisc)
	if err != nil && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	return nil
}

func replaceFilter(ifindex int, prog *ebpf.Program, parent uint32, label string) error {
	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: ifindex,
			Parent:    parent,
			Handle:    netlink.MakeHandle(0, 1),
			Protocol:  unix.ETH_P_ALL,
			Priority:  1,
		},
		Fd:           prog.FD(),
		Name:         label,
		DirectAction: true,
	}
	err := netlink.FilterReplace(filter)
	if err != nil {
		if addErr := netlink.FilterAdd(filter); addErr != nil && !errors.Is(addErr, syscall.EEXIST) {
			return addErr
		}
	}
	return nil
}
