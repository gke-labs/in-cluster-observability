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

package scrapeauth

import (
	"net/http"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// StreamInterceptor applies the same TokenReview + SAR decision to
// gRPC streams. The SAR nonResourceURL is the full method path
// (e.g. /ollie.stream.v1.StreamService/SubscribeSpans) and the verb
// is `post` — factually what gRPC is on the wire — so RBAC grants
// use nonResourceURLs like "/ollie.stream.v1.StreamService/*" with
// verbs: ["post"]. Loopback peers are exempt when configured
// (port-forward debugging, same trust boundary as HTTP).
func (m *Middleware) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := ss.Context()
		if m.cfg.ExemptLoopback {
			if p, ok := peer.FromContext(ctx); ok && p.Addr != nil && isLoopback(p.Addr.String()) {
				return handler(srv, ss)
			}
		}
		md, _ := metadata.FromIncomingContext(ctx)
		token := bearerFromMD(md)
		if token == "" {
			return status.Error(codes.Unauthenticated, "bearer token required")
		}
		switch m.Authorize(ctx, token, info.FullMethod, "post") {
		case http.StatusOK:
			return handler(srv, ss)
		case http.StatusUnauthorized:
			return status.Error(codes.Unauthenticated, "token not authenticated")
		case http.StatusForbidden:
			return status.Error(codes.PermissionDenied, "not authorized for "+info.FullMethod)
		default:
			return status.Error(codes.Unavailable, "authentication unavailable")
		}
	}
}

func bearerFromMD(md metadata.MD) string {
	for _, v := range md.Get("authorization") {
		const prefix = "Bearer "
		if len(v) > len(prefix) && strings.EqualFold(v[:len(prefix)], prefix) {
			return strings.TrimSpace(v[len(prefix):])
		}
	}
	return ""
}
