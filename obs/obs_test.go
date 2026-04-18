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

package obs

import (
	"testing"
)

type User struct {
	Name string
}

func UserAttribute(u *User) Attribute {
	return String("user.name", u.Name)
}

func TestObs(t *testing.T) {
	ctx := t.Context()

	// Test FromContext
	log := FromContext(ctx)
	log.Info("hello from FromContext")

	// Test Start without attributes
	ctx2, span1 := Start(ctx, "test1")
	defer span1.End()
	span1.Info("hello from span1")

	// Test Start with attributes
	u := &User{Name: "alice"}
	ctx3, span2 := Start(ctx2, "test2", UserAttribute(u))
	defer span2.End()
	span2.Info("hello from span2")

	// Test extracting the logger with attributes
	log3 := FromContext(ctx3)
	log3.Info("hello from log3, should have user.name=alice")
}
