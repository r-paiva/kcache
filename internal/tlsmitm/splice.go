// SPDX-FileCopyrightText: 2026 The latch Contributors
//
// SPDX-License-Identifier: Apache-2.0

package tlsmitm

import (
	"io"
	"net"
	"sync"
)

// Splice copies data bidirectionally between a and b until either side closes,
// then closes both connections.
func Splice(a, b net.Conn) {
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(a, b) //nolint:errcheck
		closeAll()
	}()
	go func() {
		defer wg.Done()
		io.Copy(b, a) //nolint:errcheck
		closeAll()
	}()
	wg.Wait()
}
