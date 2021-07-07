// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// MIT License
// Copyright (c) 2020 Dmitriy Titov (Дмитрий Титов)
//
// Code retrieved from https://github.com/DmitriyVTitov/size.

// Package size implements run-time calculation of size of the variable. Source
// code is based on "binary.Size()" function from Go standard library. size.Of()
// omits size of slices, arrays and maps containers itself (24, 24 and 8 bytes).
// When counting maps separate calculations are done for keys and values.
package size

import "reflect"

// TODO(mgartner): ...

// Of returns the size of 'v' in bytes.
// If there is an error during calculation, Of returns -1.
func Of(v interface{}) int64 {
	cache := make(map[uintptr]bool) // cache with every visited Pointer for recursion detection
	return sizeOf(reflect.Indirect(reflect.ValueOf(v)), cache)
}

// sizeOf returns the number of bytes the actual data represented by v occupies in memory.
// If there is an error, sizeOf returns -1.
func sizeOf(v reflect.Value, cache map[uintptr]bool) int64 {

	switch v.Kind() {

	case reflect.Array:
		fallthrough
	case reflect.Slice:
		// return 0 if this node has been visited already (infinite recursion)
		v.UnsafeAddr()
		if v.Kind() != reflect.Array && cache[v.Pointer()] {
			return 0
		}
		if v.Kind() != reflect.Array {
			cache[v.Pointer()] = true
		}
		var sum int64
		for i := 0; i < v.Len(); i++ {
			s := sizeOf(v.Index(i), cache)
			// if s < 0 {
			// 	return -1
			// }
			sum += s
		}
		return sum + int64(v.Type().Size())

	case reflect.Struct:
		var sum int64
		t := v.Type()
		for i, n := 0, v.NumField(); i < n; i++ {
			if t.Field(i).Tag.Get("size") == "ignore" {
				continue
			}
			s := sizeOf(v.Field(i), cache)
			// if s < 0 {
			// 	return -1
			// }
			sum += s
		}
		return sum

	case reflect.String:
		return int64(len(v.String())) + int64(v.Type().Size())

	case reflect.Ptr:
		// return Ptr size if this node has been visited already (infinite recursion)
		if cache[v.Pointer()] {
			return int64(v.Type().Size())
		}
		cache[v.Pointer()] = true
		if v.IsNil() {
			return int64(reflect.New(v.Type()).Type().Size())
		}
		s := sizeOf(reflect.Indirect(v), cache)
		// if s < 0 {
		// 	return -1
		// }
		return s + int64(v.Type().Size())

	case reflect.Bool,
		reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Int,
		reflect.Chan,
		reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return int64(v.Type().Size())

	case reflect.Map:
		// return 0 if this node has been visited already (infinite recursion)
		if cache[v.Pointer()] {
			return 0
		}
		cache[v.Pointer()] = true
		var sum int64
		keys := v.MapKeys()
		for i := range keys {
			val := v.MapIndex(keys[i])
			// calculate size of key and value separately
			sv := sizeOf(val, cache)
			// if sv < 0 {
			// 	return -1
			// }
			sum += sv
			sk := sizeOf(keys[i], cache)
			// if sk < 0 {
			// 	return -1
			// }
			sum += sk
		}
		// Include overhead due to unused map buckets.  10.79 comes
		// from https://golang.org/src/runtime/map.go.
		return sum + int64(v.Type().Size()) + int64(float64(len(keys))*10.79)

	case reflect.Interface:
		return sizeOf(v.Elem(), cache) + int64(v.Type().Size())

	case reflect.Func:
		// TODO(mgartner): There's probably some bytes taken up by a Func.
		return 0

	case reflect.Invalid:
		return 0
	}

	return -1
}
