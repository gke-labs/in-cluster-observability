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

package pb

import (
	"bytes"
	"encoding/binary"
)

type ObjectType struct {
	TypeCode uint32
	TypeName string
}

func (x *ObjectType) Marshal() []byte {
	// Tag 1 (type_code): 0x08 (field 1, type varint)
	// Tag 2 (type_name): 0x12 (field 2, type length-delimited)
	var buf bytes.Buffer

	// Write type_code
	buf.WriteByte(0x08)
	var tmp [10]byte
	n := binary.PutUvarint(tmp[:], uint64(x.TypeCode))
	buf.Write(tmp[:n])

	// Write type_name
	buf.WriteByte(0x12)
	n = binary.PutUvarint(tmp[:], uint64(len(x.TypeName)))
	buf.Write(tmp[:n])
	buf.WriteString(x.TypeName)

	return buf.Bytes()
}
