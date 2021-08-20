// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"sync/atomic"
)

// Once 保证某个函数只被执行一次
type Once struct {
	done uint32 // 表示函数是否被执行
	m    Mutex  // 同步控制
}

// 执行函数 f， f不能调用 Do 方法，否则出现死锁，即使 f 出现 panic ，Do 方法也会任务 f 被执行了，下次调用不会被执行
func (o *Once) Do(f func()) {
	// 当 Do 方法被并发调用时，cas 获胜者执行 f，后面的调用直接返回，不会等待 f 执行完成，因此
	// 此处需要退化到互斥锁，使得未获取到的 cas 的调用方返回时 f 已经执行完。这个思路非常严谨!
	if atomic.LoadUint32(&o.done) == 0 {
		o.doSlow(f) // 执行 f
	}
}

func (o *Once) doSlow(f func()) {
	o.m.Lock()
	defer o.m.Unlock()
	if o.done == 0 {
		defer atomic.StoreUint32(&o.done, 1)
		f()
	}
}
