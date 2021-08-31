// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
package encoding

/*
	encoding 包定了一些接口，这些接口被 encoding 中的子包实现。encoding 中的子包在字节和文本之间转换数据，
	子包包含 encoding/gob, encoding/json, encoding/xml.
	接口实现成对出现，用于编码和解码
*/

// 将接收到的数据转换为字节，序列化数据
type BinaryMarshaler interface {
	MarshalBinary() (data []byte, err error)
}

// UnmarshalBinary 用户反序列化数据，该方法解码数据后会丢失原始数据，如果希望在返回数据后保留数据，需要复制数据
type BinaryUnmarshaler interface {
	UnmarshalBinary(data []byte) error
}

// 将接收到的数据编码为 UTF-8 形式的文本
type TextMarshaler interface {
	MarshalText() (text []byte, err error)
}

// 将 UTF-8 格式的文本转码为指定格式,该方法解码数据后会丢失原始数据，如果希望在返回数据后保留数据，需要复制数据
type TextUnmarshaler interface {
	UnmarshalText(text []byte) error
}
