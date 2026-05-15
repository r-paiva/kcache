// SPDX-FileCopyrightText: 2026 Rui Paiva <kcache.catapult615@passfwd.com>
//
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"net/http"
	"time"
)

type Entry struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	CachedAt   time.Time
	TTL        time.Duration
}

func (e *Entry) Expired() bool {
	return time.Since(e.CachedAt) > e.TTL
}

type Cache interface {
	Get(key string) (*Entry, bool)
	Set(key string, entry *Entry)
	Delete(key string)
	Len() int
	SizeBytes() int64
}
