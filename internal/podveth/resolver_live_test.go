// SPDX-FileCopyrightText: 2026 The latch Contributors
//
// SPDX-License-Identifier: Apache-2.0

package podveth

import (
	"os"
	"testing"
)

// TestResolveLive walks the real procfs and prints podIP → host veth ifindex.
// Root-gated: it needs CAP_SYS_ADMIN to enter other network namespaces, so it
// skips unless run as root (compile with `go test -c` and run with sudo on a node).
func TestResolveLive(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to enter pod network namespaces")
	}

	r := NewResolver("/proc")
	mappings, err := r.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	t.Logf("resolved %d pod veths", len(mappings))
	for ip, idx := range mappings {
		t.Logf("  %s → ifindex %d", ip, idx)
	}
}
