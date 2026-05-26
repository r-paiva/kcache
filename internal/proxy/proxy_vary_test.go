// SPDX-FileCopyrightText: Copyright (c) 2026, the k-cache developers
//
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"reflect"
	"testing"
)

func TestParseVaryHeader(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"*", nil},
		{"Accept-Encoding", []string{"Accept-Encoding"}},
		{"accept-encoding", []string{"Accept-Encoding"}},
		{"Accept-Encoding, Authorization", []string{"Accept-Encoding", "Authorization"}},
		{" Accept-Encoding , Authorization ", []string{"Accept-Encoding", "Authorization"}},
	}
	for _, tc := range cases {
		got := parseVaryHeader(tc.input)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseVaryHeader(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestMergeVaryHeaders(t *testing.T) {
	got := mergeVaryHeaders(
		[]string{"Authorization"},
		[]string{"Accept-Encoding", "authorization"},
	)
	want := []string{"Accept-Encoding", "Authorization"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMergeVaryHeadersEmptyB(t *testing.T) {
	a := []string{"Authorization"}
	got := mergeVaryHeaders(a, nil)
	if !reflect.DeepEqual(got, a) {
		t.Errorf("got %v, want %v", got, a)
	}
}

func TestMergeVaryHeadersBothEmpty(t *testing.T) {
	got := mergeVaryHeaders(nil, nil)
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}
