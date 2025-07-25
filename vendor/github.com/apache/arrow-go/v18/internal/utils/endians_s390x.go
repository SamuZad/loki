// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package utils

import (
	"math"
	"math/bits"

	"golang.org/x/exp/constraints"
)

var (
	ToLEInt16   = func(x int16) int16 { return int16(bits.ReverseBytes16(uint16(x))) }
	ToLEUint16  = bits.ReverseBytes16
	ToLEUint32  = bits.ReverseBytes32
	ToLEUint64  = bits.ReverseBytes64
	ToLEInt32   = func(x int32) int32 { return int32(bits.ReverseBytes32(uint32(x))) }
	ToLEInt64   = func(x int64) int64 { return int64(bits.ReverseBytes64(uint64(x))) }
	ToLEFloat32 = func(x float32) float32 { return math.Float32frombits(bits.ReverseBytes32(math.Float32bits(x))) }
	ToLEFloat64 = func(x float64) float64 { return math.Float64frombits(bits.ReverseBytes64(math.Float64bits(x))) }
)

func ToLE[T constraints.Integer | constraints.Float](x T) T {
	switch v := any(x).(type) {
	case int8:
		return T(v)
	case uint8:
		return T(v)
	case int16:
		return T(bits.ReverseBytes16(uint16(v)))
	case uint16:
		return T(bits.ReverseBytes16(v))
	case int32:
		return T(bits.ReverseBytes32(uint32(v)))
	case uint32:
		return T(bits.ReverseBytes32(v))
	case int64:
		return T(bits.ReverseBytes64(uint64(v)))
	case uint64:
		return T(bits.ReverseBytes64(v))
	case float32:
		return T(math.Float32frombits(bits.ReverseBytes32(math.Float32bits(v))))
	case float64:
		return T(math.Float64frombits(bits.ReverseBytes64(math.Float64bits(v))))
	default:
		panic("unsupported type for ToLE conversion")
	}
}
