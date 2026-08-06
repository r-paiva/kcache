// SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
//
// SPDX-License-Identifier: Apache-2.0

package iface

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// narrow interface so Manager is testable without a real kernel.
type linkCloser interface {
	Close() error
}

type Manager struct {
	ingress *ebpf.Program
	egress  *ebpf.Program

	mu    sync.Mutex
	links map[int][]linkCloser // ifindex → open TCX links for that interface
}

// New creates a Manager; Reconcile decides which veths get TC BPF (policy-gated).
func New(ingress, egress *ebpf.Program) *Manager {
	return &Manager{
		ingress: ingress,
		egress:  egress,
		links:   make(map[int][]linkCloser),
	}
}

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
			if update.Header.Type == unix.RTM_DELLINK {
				m.detach(update.Link.Attrs().Index, update.Link.Attrs().Name)
			}
		}
	}
}

func (m *Manager) detach(ifindex int, name string) {
	// Kernel already removed TC programs when the interface was destroyed;
	// release the link FDs and free the ifindex for reuse by a future pod
	m.mu.Lock()
	ls, ok := m.links[ifindex]
	delete(m.links, ifindex)
	m.mu.Unlock()
	if !ok {
		return
	}
	for _, l := range ls {
		_ = l.Close()
	}
	slog.Info("TC BPF detached", "iface", name)
}

func (m *Manager) attachIndex(idx int, name string) error {
	m.mu.Lock()
	if _, already := m.links[idx]; already {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	ing, egr, err := attachTCX(idx, m.ingress, m.egress)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.links[idx] = []linkCloser{ing, egr}
	m.mu.Unlock()

	slog.Info("TC BPF attached", "iface", name)
	return nil
}

// Reconcile attaches TC BPF to the desired host veth ifindexes and detaches the
// rest. desired is the set of veths for pods a CachePolicy matches
func (m *Manager) Reconcile(desired map[int]struct{}) {
	m.mu.Lock()
	current := make(map[int]struct{}, len(m.links))
	for idx := range m.links {
		current[idx] = struct{}{}
	}
	m.mu.Unlock()

	var attached, detached int
	for idx := range desired {
		if _, ok := current[idx]; ok {
			continue
		}
		if err := m.attachIndex(idx, ifaceName(idx)); err != nil {
			slog.Warn("reconcile: attach", "ifindex", idx, "err", err)
			continue
		}
		attached++
	}
	for idx := range current {
		if _, ok := desired[idx]; ok {
			continue
		}
		m.detach(idx, ifaceName(idx))
		detached++
	}
	if attached > 0 || detached > 0 {
		slog.Info("reconcile: attachment updated",
			"attached", attached, "detached", detached, "desired", len(desired))
	}
}

// ifaceName resolves ifindex → name for logging; "if<idx>" if the link is gone.
func ifaceName(idx int) string {
	if l, err := netlink.LinkByIndex(idx); err == nil {
		return l.Attrs().Name
	}
	return fmt.Sprintf("if%d", idx)
}

// attachTCX attaches via TCX. link.Head() is
// required — Cilium's TC programs return TC_ACT_REDIRECT which terminates the
// chain, so latch must run before them
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
		_ = ing.Close()
		return nil, nil, fmt.Errorf("tcx egress: %w", err)
	}

	return ing, egr, nil
}
