/*
 * SPDX-FileCopyrightText: Copyright (c) 2026, the kcache developers
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

#define INTERCEPT_PORT     80
#define INTERCEPT_PORT_TLS 443
#define ETH_HLEN       14

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

struct proxy_tgt {
	__u32 ip;   // host byte order; zero means not yet configured
	__u16 port; // host byte order
	__u16 _pad;
};

// LRU so stale entries are evicted automatically when the map fills.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__type(key, struct conn_key);
	__type(value, struct conn_val);
	__uint(max_entries, 65535);
} orig_dst SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct proxy_tgt);
	__uint(max_entries, 1);
} proxy_tgt_map SEC(".maps");

static __always_inline int ip_hdrlen(struct iphdr *iph)
{
	int len = iph->ihl * 4;
	if (len < 20 || len > 60)
		return 0;
	return len;
}

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
	__u16 dst_port = bpf_ntohs(tcph->dest);
	if (dst_port != INTERCEPT_PORT && dst_port != INTERCEPT_PORT_TLS)
		return TC_ACT_OK;

	__u32 tgt_key = 0;
	struct proxy_tgt *tgt = bpf_map_lookup_elem(&proxy_tgt_map, &tgt_key);
	if (!tgt || tgt->ip == 0)
		return TC_ACT_OK;

	// Snapshot before bpf_map_update_elem invalidates PTR_TO_PACKET registers.
	__u32 src_ip_h   = bpf_ntohl(iph->saddr);
	__u32 old_dst_ip = iph->daddr;          // network byte order
	__u16 old_dst_pt = tcph->dest;          // network byte order
	__u32 new_dst_ip = bpf_htonl(tgt->ip);  // network byte order
	__u16 new_dst_pt = bpf_htons(tgt->port);

	struct conn_key ck = {
		.src_ip   = src_ip_h,
		.src_port = bpf_ntohs(tcph->source),
	};
	struct conn_val cv = {
		.orig_dst_ip   = bpf_ntohl(iph->daddr),
		.orig_dst_port = bpf_ntohs(tcph->dest),
	};
	bpf_map_update_elem(&orig_dst, &ck, &cv, BPF_ANY);

	__u32 ip_csum_off   = ETH_HLEN + offsetof(struct iphdr, check);
	__u32 ip_daddr_off  = ETH_HLEN + offsetof(struct iphdr, daddr);
	__u32 tcp_csum_off  = ETH_HLEN + ihl + offsetof(struct tcphdr, check);
	__u32 tcp_dport_off = ETH_HLEN + ihl + offsetof(struct tcphdr, dest);

	bpf_l3_csum_replace(skb, ip_csum_off, old_dst_ip, new_dst_ip, 4);
	// BPF_F_PSEUDO_HDR: TCP pseudo-header covers IP addresses.
	bpf_l4_csum_replace(skb, tcp_csum_off, old_dst_ip, new_dst_ip,
	                    BPF_F_PSEUDO_HDR | 4);
	bpf_l4_csum_replace(skb, tcp_csum_off, old_dst_pt, new_dst_pt, 2);

	// flags=0: raw write, checksums already updated manually above.
	bpf_skb_store_bytes(skb, ip_daddr_off, &new_dst_ip, sizeof(new_dst_ip), 0);
	bpf_skb_store_bytes(skb, tcp_dport_off, &new_dst_pt, sizeof(new_dst_pt), 0);

	bpf_printk("kcache tc_ingress: %x:%u → proxy %x:%u",
	           src_ip_h, ck.src_port, tgt->ip, tgt->port);
	return TC_ACT_OK;
}

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

	// Snapshot before bpf_map_lookup_elem invalidates PTR_TO_PACKET registers.
	__u32 pod_ip_h   = bpf_ntohl(iph->daddr); // host byte order
	__u16 pod_port_h = bpf_ntohs(tcph->dest); // host byte order
	__u32 old_src_ip = iph->saddr;             // network byte order
	__u16 old_src_pt = tcph->source;           // network byte order

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
	// No FIN/RST cleanup: re-reading tcph->fin after bpf_map_lookup_elem is
	// rejected by the verifier (PTR_TO_PACKET invalidated). LRU_HASH evicts.

	return TC_ACT_OK;
}

char __license[] SEC("license") = "Dual MIT/GPL";
