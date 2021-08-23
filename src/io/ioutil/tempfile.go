// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ioutil

import (
	"os"
)

func TempFile(dir, pattern string) (f *os.File, err error) {
	return os.CreateTemp(dir, pattern)
}

func TempDir(dir, pattern string) (name string, err error) {
	return os.MkdirTemp(dir, pattern)
}
