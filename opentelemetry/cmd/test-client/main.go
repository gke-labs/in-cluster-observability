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
	"flag"
	"log"
	"net/http"
	"time"
)

func main() {
	target := flag.String("target", "http://test-app:8080", "Target URL")
	qps := flag.Int("qps", 10, "Requests per second")
	flag.Parse()

	if *qps <= 0 {
		log.Fatal("qps must be greater than 0")
	}

	interval := time.Second / time.Duration(*qps)
	ticker := time.NewTicker(interval)

	log.Printf("Starting test-client, targeting %s at %d QPS", *target, *qps)

	for range ticker.C {
		go func() {
			resp, err := http.Get(*target)
			if err != nil {
				log.Printf("Error sending request: %v", err)
				return
			}
			resp.Body.Close()
		}()
	}
}
