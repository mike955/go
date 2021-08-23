// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

Package ioutil implements some I/O utility functions.

As of Go 1.16, the same functionality is now provided
by package io or package os, and those implementations
should be preferred in new code.
See the specific function documentation for details.
package ioutil

import (
	"io"
	"io/fs"
	"os"
	"sort"
)

/*
	ioutil 包提供了一些 I/O 实用函数, 主要是对其它包的简单封装
		- ReadAll: 调用 io 包的 ReadAll 方法读取数据
		- ReadFile: 调用 os 包的 ReadFile 方法读取整个文件内容，成功 error 为 nil， 而不是 EOF
		- WriteFile: 调用 os 包的 WriteFile 方法将数据写入指定文件，如果文件不存在，根据 perm 参数创建文件
		- ReadDir: 读取目录指定文件，返回读取的文件名
			* 1.16+ 开始，os.ReadDir 函数是一个更佳选择，os.ReadDir 返回 fs.DirEntry 类型 
		- NopCloser 调用 io 包的 NopCloser 返回一个无操作 Close 方法的 ReadCloser

		- TempFile: 调用 os.CreateTemp 在指定目录中创建一个新的临时文件，打开该文件进行读写
		- MkdirTemp: 调用 os.MkdirTemp 创建一个临时目录

*/


func ReadAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

func ReadFile(filename string) ([]byte, error) {
	return os.ReadFile(filename)
}

func WriteFile(filename string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(filename, data, perm)
}

func ReadDir(dirname string) ([]fs.FileInfo, error) {
	f, err := os.Open(dirname)
	if err != nil {
		return nil, err
	}
	list, err := f.Readdir(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name() < list[j].Name() })
	return list, nil
}


func NopCloser(r io.Reader) io.ReadCloser {
	return io.NopCloser(r)
}

var Discard io.Writer = io.Discard
