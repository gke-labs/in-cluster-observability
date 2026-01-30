#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>

char LICENSE[] SEC("license") = "Dual BSD/GPL";

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, __u32);
    __type(value, __u64);
} tcp_metrics SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, __u32);
    __type(value, __u64);
} http_metrics SEC(".maps");

// TCP metrics: 1 for connect, 2 for accept
SEC("kprobe/tcp_v4_connect")
int kprobe_tcp_v4_connect(void *ctx) {
    __u32 key = 1;
    __u64 *count = bpf_map_lookup_elem(&tcp_metrics, &key);
    if (count) {
        __sync_fetch_and_add(count, 1);
    } else {
        __u64 initial = 1;
        bpf_map_update_elem(&tcp_metrics, &key, &initial, BPF_ANY);
    }
    return 0;
}

SEC("kprobe/inet_csk_accept")
int kprobe_inet_csk_accept(void *ctx) {
    __u32 key = 2;
    __u64 *count = bpf_map_lookup_elem(&tcp_metrics, &key);
    if (count) {
        __sync_fetch_and_add(count, 1);
    } else {
        __u64 initial = 1;
        bpf_map_update_elem(&tcp_metrics, &key, &initial, BPF_ANY);
    }
    return 0;
}

// HTTP: Count calls to sys_enter_write where the buffer starts with "GET "
// This is a very rough approximation for "transparent" HTTP capture.
// sys_enter_write(int fd, const char *buf, size_t count)
SEC("tracepoint/syscalls/sys_enter_write")
int tracepoint_sys_enter_write(struct {
    __u64 common_tp_fields;
    int common_syscall_nr;
    int fd;
    const char *buf;
    __u64 count;
} *ctx) {
    char p[4];
    if (bpf_probe_read_user(p, sizeof(p), ctx->buf) == 0) {
        if (p[0] == 'G' && p[1] == 'E' && p[2] == 'T' && p[3] == ' ') {
            __u32 key = 1; // GET requests
            __u64 *count = bpf_map_lookup_elem(&http_metrics, &key);
            if (count) {
                __sync_fetch_and_add(count, 1);
            } else {
                __u64 initial = 1;
                bpf_map_update_elem(&http_metrics, &key, &initial, BPF_ANY);
            }
        }
    }
    return 0;
}