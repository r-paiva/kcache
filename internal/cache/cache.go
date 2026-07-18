// SPDX-FileCopyrightText: Copyright (c) 2026, the kcache developers
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
	Proto      string
	ProtoMajor int
	ProtoMinor int
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
