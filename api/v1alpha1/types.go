// SPDX-FileCopyrightText: Copyright (c) 2026, the latch developers
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type CachePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec CachePolicySpec `json:"spec"`
}

type CachePolicySpec struct {
	PodSelector metav1.LabelSelector `json:"podSelector,omitempty"`
	Rules       []CachePolicyRule    `json:"rules"`
}

type CachePolicyRule struct {
	Host         string          `json:"host"`
	Port         uint16          `json:"port,omitempty"`
	Methods      []string        `json:"methods,omitempty"`
	Paths        []string        `json:"paths,omitempty"`
	TTL          metav1.Duration `json:"ttl"`
	MaxBodyBytes int64           `json:"maxBodyBytes,omitempty"`
	// VaryHeaders lists request header names included in the cache key.
	// Use when the upstream returns different content based on a request header
	// (e.g. Accept-Language). Empty means headers are not part of the key.
	VaryHeaders []string `json:"varyHeaders,omitempty"`
	// CacheBody includes the request body in the cache key.
	// Useful for POST requests where the body identifies the resource.
	CacheBody bool `json:"cacheBody,omitempty"`
}

type CachePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CachePolicy `json:"items"`
}
