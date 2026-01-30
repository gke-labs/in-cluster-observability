// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target bpfel,bpfeb metrics bpf/metrics.bpf.c -- -I/usr/include/bpf -I/usr/include/x86_64-linux-gnu -I/usr/include

var (
	rxBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_network_receive_bytes_total",
			Help: "Total number of bytes received by the interface.",
		},
		[]string{"device"},
	)
	txBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_network_transmit_bytes_total",
			Help: "Total number of bytes transmitted by the interface.",
		},
		[]string{"device"},
	)
	tcpConnections = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_tcp_connections_total",
			Help: "Total number of TCP connections established.",
		},
		[]string{"direction"},
	)
	httpRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_http_requests_total",
			Help: "Total number of HTTP requests detected.",
		},
		[]string{"method"},
	)
)

func init() {
	prometheus.MustRegister(rxBytes)
	prometheus.MustRegister(txBytes)
	prometheus.MustRegister(tcpConnections)
	prometheus.MustRegister(httpRequests)
}

func main() {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		log.Fatal(err)
	}

	// Load pre-compiled programs and maps into the kernel.
	objs := metricsObjects{}
	if err := loadMetricsObjects(&objs, nil); err != nil {
		log.Fatalf("loading objects: %v", err)
	}
	defer objs.Close()

	// Attach kprobes.
	kpConnect, err := link.Kprobe("tcp_v4_connect", objs.KprobeTcpV4Connect, nil)
	if err != nil {
		log.Fatalf("opening kprobe: %s", err)
	}
	defer kpConnect.Close()

	kpAccept, err := link.Kprobe("inet_csk_accept", objs.KprobeInetCskAccept, nil)
	if err != nil {
		log.Fatalf("opening kprobe: %s", err)
	}
	defer kpAccept.Close()

	// Attach tracepoint.
	tpWrite, err := link.Tracepoint("syscalls", "sys_enter_write", objs.TracepointSysEnterWrite, nil)
	if err != nil {
		log.Fatalf("opening tracepoint: %s", err)
	}
	defer tpWrite.Close()

	go func() {
		for {
			updateMetrics()
			updateBPFMetrics(&objs)
			time.Sleep(5 * time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	log.Println("Starting server on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
	}
}

var lastTCPOutbound, lastTCPInbound, lastHTTPGet uint64

func updateBPFMetrics(objs *metricsObjects) {
	var value uint64

	if err := objs.TcpMetrics.Lookup(uint32(1), &value); err == nil {
		if value > lastTCPOutbound {
			tcpConnections.WithLabelValues("outbound").Add(float64(value - lastTCPOutbound))
			lastTCPOutbound = value
		}
	}

	if err := objs.TcpMetrics.Lookup(uint32(2), &value); err == nil {
		if value > lastTCPInbound {
			tcpConnections.WithLabelValues("inbound").Add(float64(value - lastTCPInbound))
			lastTCPInbound = value
		}
	}

	if err := objs.HttpMetrics.Lookup(uint32(1), &value); err == nil {
		if value > lastHTTPGet {
			httpRequests.WithLabelValues("GET").Add(float64(value - lastHTTPGet))
			lastHTTPGet = value
		}
	}
}

func updateMetrics() {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		log.Printf("Error opening /proc/net/dev: %v", err)
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// Skip the first two header lines
	for i := 0; i < 2 && scanner.Scan(); i++ {
	}

	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) < 10 {
			continue
		}

		device := strings.TrimSuffix(parts[0], ":")
		rx, err := strconv.ParseFloat(parts[1], 64)
		if err == nil {
			rxBytes.WithLabelValues(device).Set(rx)
		}

		tx, err := strconv.ParseFloat(parts[9], 64)
		if err == nil {
			txBytes.WithLabelValues(device).Set(tx)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Error reading /proc/net/dev: %v", err)
	}
}
