/*
 * SPDX-FileCopyrightText: 2026 Rui Paiva <kcache.catapult615@passfwd.com>
 *
 * SPDX-License-Identifier: Apache-2.0
 */

//go:build ignore

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/tcp.h>
#include <linux/in.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

#define INTERCEPT_PORT 80
#define ETH_HLEN       14

// Map key: the connecting socket's {src_ip, src_port} in host byte order.
// Unique per connection since each pod has its own IP.
struct conn_key {
	__u32 src_ip;
	__u16 src_port;
	__u16 _pad;
};

struct conn_val {
	__u32 orig_dst_ip;   // host byte order
	__u16 orig_dst_port; // host byte order
	__u16 _pad;
};

// Proxy redirect target — single entry, populated by Go daemon at startup.
// ip and port in host byte order; zero ip means "not configured yet".
struct proxy_tgt {
	__u32 ip;
	__u16 port;
	__u16 _pad;
};

// {src_ip, src_port} → original {dst_ip, dst_port}
// Written on ingress (first SYN), read by Go daemon via bpfOrigDst(),
// cleaned up by egress on FIN/RST.
// LRU so stale entries are evicted automatically if the map fills.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__type(key, struct conn_key);
	__type(value, struct conn_val);
	__uint(max_entries, 65535);
} orig_dst SEC(".maps");

// Single-entry array: where to redirect intercepted port-80 connections.
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct proxy_tgt);
	__uint(max_entries, 1);
} proxy_tgt_map SEC(".maps");

// ── helpers ──────────────────────────────────────────────────────────────────

// Validate and return the ip header length in bytes.
// Returns 0 if the header is malformed or too small.
static __always_inline int ip_hdrlen(struct iphdr *iph)
{
	int len = iph->ihl * 4;
	if (len < 20 || len > 60)
		return 0;
	return len;
}

// ── TC ingress ────────────────────────────────────────────────────────────────
// Attached to pod veths (host side), fires on packets FROM the pod.
// Intercepts TCP connections to port 80, stores the original destination,
// and rewrites the destination to the kcache proxy.

SEC("tc")
int tc_ingress(struct __sk_buff *skb)
{
	void *data     = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;

	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end)
		return TC_ACT_OK;
	if (eth->h_proto != bpf_htons(ETH_P_IP))
		return TC_ACT_OK;

	struct iphdr *iph = (void *)(eth + 1);
	if ((void *)(iph + 1) > data_end)
		return TC_ACT_OK;
	if (iph->protocol != IPPROTO_TCP)
		return TC_ACT_OK;

	int ihl = ip_hdrlen(iph);
	if (!ihl)
		return TC_ACT_OK;

	struct tcphdr *tcph = (void *)iph + ihl;
	if ((void *)(tcph + 1) > data_end)
		return TC_ACT_OK;
	if (bpf_ntohs(tcph->dest) != INTERCEPT_PORT)
		return TC_ACT_OK;

	// Read redirect target; pass through if not yet configured.
	__u32 tgt_key = 0;
	struct proxy_tgt *tgt = bpf_map_lookup_elem(&proxy_tgt_map, &tgt_key);
	if (!tgt || tgt->ip == 0)
		return TC_ACT_OK;

	// Snapshot packet fields before any helper invalidates pointers.
	__u32 src_ip_h   = bpf_ntohl(iph->saddr);
	__u32 old_dst_ip = iph->daddr;          // network byte order
	__u16 old_dst_pt = tcph->dest;          // network byte order
	__u32 new_dst_ip = bpf_htonl(tgt->ip);  // network byte order
	__u16 new_dst_pt = bpf_htons(tgt->port);

	// Record original destination so the Go daemon and egress path can
	// restore it.
	struct conn_key ck = {
		.src_ip   = src_ip_h,
		.src_port = bpf_ntohs(tcph->source),
	};
	struct conn_val cv = {
		.orig_dst_ip   = bpf_ntohl(iph->daddr),
		.orig_dst_port = bpf_ntohs(tcph->dest),
	};
	bpf_map_update_elem(&orig_dst, &ck, &cv, BPF_ANY);

	// Compute packet offsets.
	__u32 ip_csum_off  = ETH_HLEN + offsetof(struct iphdr, check);
	__u32 ip_daddr_off = ETH_HLEN + offsetof(struct iphdr, daddr);
	__u32 tcp_csum_off = ETH_HLEN + ihl + offsetof(struct tcphdr, check);
	__u32 tcp_dport_off = ETH_HLEN + ihl + offsetof(struct tcphdr, dest);

	// Update IP checksum for the dst-IP change.
	bpf_l3_csum_replace(skb, ip_csum_off, old_dst_ip, new_dst_ip, 4);
	// Update TCP checksum: pseudo-header covers IP addrs (BPF_F_PSEUDO_HDR).
	bpf_l4_csum_replace(skb, tcp_csum_off, old_dst_ip, new_dst_ip,
	                    BPF_F_PSEUDO_HDR | 4);
	// Update TCP checksum for the dst-port change.
	bpf_l4_csum_replace(skb, tcp_csum_off, old_dst_pt, new_dst_pt, 2);

	// Write new dst IP and port (flags=0: raw write, no auto-csum).
	bpf_skb_store_bytes(skb, ip_daddr_off, &new_dst_ip, sizeof(new_dst_ip), 0);
	bpf_skb_store_bytes(skb, tcp_dport_off, &new_dst_pt, sizeof(new_dst_pt), 0);

	bpf_printk("kcache tc_ingress: %x:%u → proxy %x:%u",
	           src_ip_h, ck.src_port, tgt->ip, tgt->port);
	return TC_ACT_OK;
}

// ── TC egress ─────────────────────────────────────────────────────────────────
// Attached to pod veths (host side), fires on packets TO the pod.
// For connections we redirected, rewrites the source address back to the
// original server so the pod's TCP stack sees a consistent conversation.

SEC("tc")
int tc_egress(struct __sk_buff *skb)
{
	void *data     = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;

	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end)
		return TC_ACT_OK;
	if (eth->h_proto != bpf_htons(ETH_P_IP))
		return TC_ACT_OK;

	struct iphdr *iph = (void *)(eth + 1);
	if ((void *)(iph + 1) > data_end)
		return TC_ACT_OK;
	if (iph->protocol != IPPROTO_TCP)
		return TC_ACT_OK;

	int ihl = ip_hdrlen(iph);
	if (!ihl)
		return TC_ACT_OK;

	struct tcphdr *tcph = (void *)iph + ihl;
	if ((void *)(tcph + 1) > data_end)
		return TC_ACT_OK;

	// Snapshot ALL packet fields before any helper call — bpf_map_lookup_elem
	// invalidates PTR_TO_PACKET registers in the verifier's eyes.
	__u32 pod_ip_h   = bpf_ntohl(iph->daddr);  // pod IP, host byte order
	__u16 pod_port_h = bpf_ntohs(tcph->dest);  // pod port, host byte order
	__u32 old_src_ip = iph->saddr;              // proxy IP, network byte order
	__u16 old_src_pt = tcph->source;            // proxy port, network byte order

	// On the return path, the packet's dst is the pod (src_ip:src_port from
	// the forward path). Look up the original server for this connection.
	struct conn_key ck = {
		.src_ip   = pod_ip_h,
		.src_port = pod_port_h,
	};
	struct conn_val *cv = bpf_map_lookup_elem(&orig_dst, &ck);
	if (!cv)
		return TC_ACT_OK;

	__u32 new_src_ip = bpf_htonl(cv->orig_dst_ip);
	__u16 new_src_pt = bpf_htons(cv->orig_dst_port);

	__u32 ip_csum_off   = ETH_HLEN + offsetof(struct iphdr, check);
	__u32 ip_saddr_off  = ETH_HLEN + offsetof(struct iphdr, saddr);
	__u32 tcp_csum_off  = ETH_HLEN + ihl + offsetof(struct tcphdr, check);
	__u32 tcp_sport_off = ETH_HLEN + ihl + offsetof(struct tcphdr, source);

	bpf_l3_csum_replace(skb, ip_csum_off, old_src_ip, new_src_ip, 4);
	bpf_l4_csum_replace(skb, tcp_csum_off, old_src_ip, new_src_ip,
	                    BPF_F_PSEUDO_HDR | 4);
	bpf_l4_csum_replace(skb, tcp_csum_off, old_src_pt, new_src_pt, 2);

	bpf_skb_store_bytes(skb, ip_saddr_off, &new_src_ip, sizeof(new_src_ip), 0);
	bpf_skb_store_bytes(skb, tcp_sport_off, &new_src_pt, sizeof(new_src_pt), 0);
	// LRU_HASH evicts stale entries automatically; explicit FIN/RST cleanup
	// would require re-reading tcph->fin after the map lookup, which the
	// verifier rejects (PTR_TO_PACKET invalidated by bpf_map_lookup_elem).

	return TC_ACT_OK;
}

char __license[] SEC("license") = "Dual MIT/GPL";
