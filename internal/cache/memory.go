// SPDX-FileCopyrightText: Copyright (c) 2026, the kcache developers
//
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"sync"
	"sync/atomic"
	"time"
)

type store struct {
	mu         sync.RWMutex
	entries    map[string]*Entry
	maxBytes   int64
	count      atomic.Int64
	totalBytes atomic.Int64
	onEvicted  func(n int)
}

func New(maxBytes int64, onEvicted func(n int)) Cache {
	s := &store{
		entries:   make(map[string]*Entry),
		maxBytes:  maxBytes,
		onEvicted: onEvicted,
	}
	go s.sweepLoop()
	return s
}

func (s *store) Get(key string) (*Entry, bool) {
	s.mu.RLock()
	e, ok := s.entries[key]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if e.Expired() {
		s.Delete(key)
		return nil, false
	}
	return e, true
}

func (s *store) Set(key string, e *Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	newSize := e.size()

	s.removeLocked(key)

	for s.totalBytes.Load()+newSize > s.maxBytes {
		if !s.evictOneLocked(true) && !s.evictOneLocked(false) {
			return
		}
	}

	s.addLocked(key, e)
}

func (s *store) Delete(key string) {
	s.mu.Lock()
	s.removeLocked(key)
	s.mu.Unlock()
}

func (s *store) Len() int {
	return int(s.count.Load())
}

func (s *store) SizeBytes() int64 {
	return s.totalBytes.Load()
}

func (s *store) removeLocked(key string) int64 {
	if e, ok := s.entries[key]; ok {
		sz := e.size()
		s.totalBytes.Add(-sz)
		s.count.Add(-1)
		delete(s.entries, key)
		return sz
	}
	return 0
}

func (s *store) addLocked(key string, e *Entry) {
	s.entries[key] = e
	s.totalBytes.Add(e.size())
	s.count.Add(1)
}

func (s *store) evictOneLocked(expiredOnly bool) bool {
	for k, v := range s.entries {
		if expiredOnly && !v.Expired() {
			continue
		}
		s.removeLocked(k)
		if s.onEvicted != nil {
			s.onEvicted(1)
		}
		return true
	}
	return false
}

func (s *store) sweepLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		var evictedN int
		for k, v := range s.entries {
			if v.Expired() {
				s.removeLocked(k)
				evictedN++
			}
		}
		s.mu.Unlock()
		if evictedN > 0 && s.onEvicted != nil {
			s.onEvicted(evictedN)
		}
	}
}

func (e *Entry) size() int64 {
	n := int64(len(e.Body))
	for k, vs := range e.Header {
		n += int64(len(k))
		for _, v := range vs {
			n += int64(len(v))
		}
	}
	return n
}
