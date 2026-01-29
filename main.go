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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

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
)

func init() {
	prometheus.MustRegister(rxBytes)
	prometheus.MustRegister(txBytes)
}

func main() {
	go func() {
		for {
			updateMetrics()
			time.Sleep(5 * time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	log.Println("Starting server on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal(err)
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
