// Copyright 2013 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

// The MIT License (MIT)
//
// Copyright (c) 2014 wandoulabs
// Copyright (c) 2014 siddontang
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package driver

import (
	"encoding/binary"
	"io"
	"math"
	"time"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/terror"
	"github.com/pingcap/tidb/util/arena"
	"github.com/pingcap/tidb/util/hack"
	"github.com/pingcap/tidb/util/types"
	"strconv"
)

const (
	codeInvalidType = 4
)

var errInvalidType = terror.ClassServer.New(codeInvalidType, "invalid type")

func ParseLengthEncodedInt(b []byte) (num uint64, isNull bool, n int) {
	switch b[0] {
	// 251: NULL
	case 0xfb:
		n = 1
		isNull = true
		return

	// 252: value of following 2
	case 0xfc:
		num = uint64(b[1]) | uint64(b[2])<<8
		n = 3
		return

	// 253: value of following 3
	case 0xfd:
		num = uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16
		n = 4
		return

	// 254: value of following 8
	case 0xfe:
		num = uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16 |
			uint64(b[4])<<24 | uint64(b[5])<<32 | uint64(b[6])<<40 |
			uint64(b[7])<<48 | uint64(b[8])<<56
		n = 9
		return
	}

	// 0-250: value of first byte
	num = uint64(b[0])
	n = 1
	return
}

func DumpLengthEncodedInt(n uint64) []byte {
	switch {
	case n <= 250:
		return tinyIntCache[n]

	case n <= 0xffff:
		return []byte{0xfc, byte(n), byte(n >> 8)}

	case n <= 0xffffff:
		return []byte{0xfd, byte(n), byte(n >> 8), byte(n >> 16)}

	case n <= 0xffffffffffffffff:
		return []byte{0xfe, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24),
			byte(n >> 32), byte(n >> 40), byte(n >> 48), byte(n >> 56)}
	}

	return nil
}

func ParseLengthEncodedBytes(b []byte) ([]byte, bool, int, error) {
	// Get length
	num, isNull, n := ParseLengthEncodedInt(b)
	if num < 1 {
		return nil, isNull, n, nil
	}

	n += int(num)

	// Check data length
	if len(b) >= n {
		return b[n-int(num) : n], false, n, nil
	}

	return nil, false, n, io.EOF
}

func DumpEncodedString(b []byte, alloc arena.Allocator, isXProtocol bool) []byte {
	if isXProtocol {
		return Dump0EndEncodedString(b, alloc)
	}
	return DumpLengthEncodedString(b, alloc)
}

func Dump0EndEncodedString(b []byte, alloc arena.Allocator) []byte {
	data := alloc.Alloc(len(b) + 1)
	data = append(data, b...)
	data = append(data, byte(0))
	return data
}

func DumpLengthEncodedString(b []byte, alloc arena.Allocator) []byte {
	data := alloc.Alloc(len(b) + 9)
	data = append(data, DumpLengthEncodedInt(uint64(len(b)))...)
	data = append(data, b...)
	return data
}

func DumpUint16(n uint16) []byte {
	return []byte{
		byte(n),
		byte(n >> 8),
	}
}

func DumpUint32(n uint32) []byte {
	return []byte{
		byte(n),
		byte(n >> 8),
		byte(n >> 16),
		byte(n >> 24),
	}
}

func DumpUint64(n uint64) []byte {
	return []byte{
		byte(n),
		byte(n >> 8),
		byte(n >> 16),
		byte(n >> 24),
		byte(n >> 32),
		byte(n >> 40),
		byte(n >> 48),
		byte(n >> 56),
	}
}

var tinyIntCache [251][]byte

func init() {
	for i := 0; i < len(tinyIntCache); i++ {
		tinyIntCache[i] = []byte{byte(i)}
	}
}

func DumpBinaryTime(dur time.Duration) (data []byte) {
	if dur == 0 {
		data = tinyIntCache[0]
		return
	}
	data = make([]byte, 13)
	data[0] = 12
	if dur < 0 {
		data[1] = 1
		dur = -dur
	}
	days := dur / (24 * time.Hour)
	dur -= days * 24 * time.Hour
	data[2] = byte(days)
	hours := dur / time.Hour
	dur -= hours * time.Hour
	data[6] = byte(hours)
	minutes := dur / time.Minute
	dur -= minutes * time.Minute
	data[7] = byte(minutes)
	seconds := dur / time.Second
	dur -= seconds * time.Second
	data[8] = byte(seconds)
	if dur == 0 {
		data[0] = 8
		return data[:9]
	}
	binary.LittleEndian.PutUint32(data[9:13], uint32(dur/time.Microsecond))
	return
}

func DumpBinaryDateTime(t types.Time, loc *time.Location) (data []byte, err error) {
	if t.Type == mysql.TypeTimestamp && loc != nil {
		// TODO: Consider time_zone variable.
		t1, err := t.Time.GoTime(time.Local)
		if err != nil {
			return nil, errors.Errorf("FATAL: convert timestamp %v go time return error!", t.Time)
		}
		t.Time = types.FromGoTime(t1.In(loc))
	}

	year, mon, day := t.Time.Year(), t.Time.Month(), t.Time.Day()
	if t.IsZero() {
		year, mon, day = 1, int(time.January), 1
	}
	switch t.Type {
	case mysql.TypeTimestamp, mysql.TypeDatetime:
		data = append(data, 11)
		data = append(data, DumpUint16(uint16(year))...)
		data = append(data, byte(mon), byte(day), byte(t.Time.Hour()), byte(t.Time.Minute()), byte(t.Time.Second()))
		data = append(data, DumpUint32(uint32(t.Time.Microsecond()))...)
	case mysql.TypeDate, mysql.TypeNewDate:
		data = append(data, 4)
		data = append(data, DumpUint16(uint16(year))...) //year
		data = append(data, byte(mon), byte(day))
	}
	return
}

func UniformValue(value interface{}) interface{} {
	switch v := value.(type) {
	case int8:
		return int64(v)
	case int16:
		return int64(v)
	case int32:
		return int64(v)
	case int64:
		return int64(v)
	case uint8:
		return uint64(v)
	case uint16:
		return uint64(v)
	case uint32:
		return uint64(v)
	case uint64:
		return uint64(v)
	default:
		return value
	}
}

func DumpRowValuesBinary(alloc arena.Allocator, columns []*ColumnInfo, row []types.Datum) (data []byte, err error) {
	if len(columns) != len(row) {
		err = mysql.ErrMalformPacket
		return
	}
	data = append(data, mysql.OKHeader)
	nullsLen := ((len(columns) + 7 + 2) / 8)
	nulls := make([]byte, nullsLen)
	for i, val := range row {
		if val.IsNull() {
			bytePos := (i + 2) / 8
			bitPos := byte((i + 2) % 8)
			nulls[bytePos] |= 1 << bitPos
		}
	}
	data = append(data, nulls...)
	for i, val := range row {
		datum, err := DumpDatumToBinary(alloc, columns[i], val, false)
		if err != nil {
			return nil, errors.Trace(err)
		}
		data = append(data, datum...)
	}
	return
}

func DumpDatumToBinary(alloc arena.Allocator, column *ColumnInfo, val types.Datum, isXProtocol bool) ([]byte, error) {
	var data []byte
	switch val.Kind() {
	case types.KindInt64:
		v := val.GetInt64()
		switch column.Type {
		case mysql.TypeTiny:
			data = append(data, byte(v))
		case mysql.TypeShort, mysql.TypeYear:
			data = append(data, DumpUint16(uint16(v))...)
		case mysql.TypeInt24, mysql.TypeLong:
			data = append(data, DumpUint32(uint32(v))...)
		case mysql.TypeLonglong:
			data = append(data, DumpUint64(uint64(v))...)
		}
	case types.KindUint64:
		v := val.GetUint64()
		switch column.Type {
		case mysql.TypeTiny:
			data = append(data, byte(v))
		case mysql.TypeShort, mysql.TypeYear:
			data = append(data, DumpUint16(uint16(v))...)
		case mysql.TypeInt24, mysql.TypeLong:
			data = append(data, DumpUint32(uint32(v))...)
		case mysql.TypeLonglong:
			data = append(data, DumpUint64(uint64(v))...)
		}
	case types.KindFloat32:
		floatBits := math.Float32bits(val.GetFloat32())
		data = append(data, DumpUint32(floatBits)...)
	case types.KindFloat64:
		floatBits := math.Float64bits(val.GetFloat64())
		data = append(data, DumpUint64(floatBits)...)
	case types.KindString, types.KindBytes:
		data = append(data, DumpEncodedString(val.GetBytes(), alloc, isXProtocol)...)
	case types.KindMysqlDecimal:
		data = append(data, DumpEncodedString(hack.Slice(val.GetMysqlDecimal().String()), alloc, isXProtocol)...)
	case types.KindMysqlTime:
		tmp, err := DumpBinaryDateTime(val.GetMysqlTime(), nil)
		if err != nil {
			return data, errors.Trace(err)
		}
		data = append(data, tmp...)
	case types.KindMysqlDuration:
		data = append(data, DumpBinaryTime(val.GetMysqlDuration().Duration)...)
	case types.KindMysqlSet:
		data = append(data, DumpEncodedString(hack.Slice(val.GetMysqlSet().String()), alloc, isXProtocol)...)
	case types.KindMysqlEnum:
		data = append(data, DumpEncodedString(hack.Slice(val.GetMysqlEnum().String()), alloc, isXProtocol)...)
	case types.KindMysqlBit:
		data = append(data, DumpEncodedString(hack.Slice(val.GetMysqlBit().ToString()), alloc, isXProtocol)...)
	}
	return data, nil
}

func DumpTextValue(colInfo *ColumnInfo, value types.Datum) ([]byte, error) {
	switch value.Kind() {
	case types.KindInt64:
		return strconv.AppendInt(nil, value.GetInt64(), 10), nil
	case types.KindUint64:
		return strconv.AppendUint(nil, value.GetUint64(), 10), nil
	case types.KindFloat32:
		prec := -1
		if colInfo.Decimal > 0 && int(colInfo.Decimal) != mysql.NotFixedDec {
			prec = int(colInfo.Decimal)
		}
		return strconv.AppendFloat(nil, value.GetFloat64(), 'f', prec, 32), nil
	case types.KindFloat64:
		prec := -1
		if colInfo.Decimal > 0 && int(colInfo.Decimal) != mysql.NotFixedDec {
			prec = int(colInfo.Decimal)
		}
		return strconv.AppendFloat(nil, value.GetFloat64(), 'f', prec, 64), nil
	case types.KindString, types.KindBytes:
		return value.GetBytes(), nil
	case types.KindMysqlTime:
		return hack.Slice(value.GetMysqlTime().String()), nil
	case types.KindMysqlDuration:
		return hack.Slice(value.GetMysqlDuration().String()), nil
	case types.KindMysqlDecimal:
		return hack.Slice(value.GetMysqlDecimal().String()), nil
	case types.KindMysqlEnum:
		return hack.Slice(value.GetMysqlEnum().String()), nil
	case types.KindMysqlSet:
		return hack.Slice(value.GetMysqlSet().String()), nil
	case types.KindMysqlJSON:
		return hack.Slice(value.GetMysqlJSON().String()), nil
	case types.KindBinaryLiteral, types.KindMysqlBit:
		return hack.Slice(value.GetBinaryLiteral().ToString()), nil
	default:
		return nil, errInvalidType.Gen("invalid type %v", value.Kind())
	}
}
