// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// 互斥锁，不应该被复制
type Mutex struct {
	state int32
	sema  uint32
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked      = 1 << iota // 锁定状态
	mutexWoken                   // 被唤醒
	mutexStarving                // 饥饿状态
	mutexWaiterShift = iota      // 等待该锁的 goroutine 数量

	/*
		关于互斥锁公平:
			互斥锁有 2 中工作模式：正常模式 和 饥饿模式
			- 正常模式下，锁等待者是一个先进先出队列，当一个被唤醒的锁等待者与一个新到达的 goroutine 争夺锁时，
			  新到达的锁获取者具有优势，因为新到达的获取者已经在 CPU 上运行(再一次证明多个 gorouine 执行顺序不确定)，
				被唤醒的锁等待着可能会输，当新到达的 goroutine 很多时，会使得被唤醒的锁等待者长时间获取不到锁，造成饥饿。如果一个锁等待者等待时间超过 1ms，
				互斥锁进入饥饿模式
			- 饥饿模式下，互斥锁解锁的 goroutine 直接将锁移交给等待队列最前面的等待着，新到达的锁获取者不会尝试获取锁，
			  直接进入队列尾部等待
			-	如果一个锁等待者发现自己是最后一个等待者，或者获取锁时间小于 1ms，将互斥锁转换为正常模式
			- 正常模式性能更好，即使有阻塞等待，goroutine 也会参试多次获取锁，而饥饿模式对于防止尾部延迟很重要，饥饿模式促进了互斥锁的公平
	*/
	starvationThresholdNs = 1e6
)

// 获取互斥锁，如果互斥锁处于不可用(锁定)状态，调用方将阻塞，知道锁可用
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) { // 互斥锁处于可用状态，拿到锁
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	m.lockSlow() // 互斥锁不可用，进入自旋状态
}

/*
	1. 判断是否进入自旋
	2. 自旋等待互斥锁释放
	3. 判断互斥锁状态
	4. 更新互斥锁状态，获取锁
*/
func (m *Mutex) lockSlow() {
	var waitStartTime int64
	starving := false // 饥饿模式
	awoke := false    // 唤醒状态
	iter := 0         // 记录自旋次数
	old := m.state    // 进入自旋前锁状态
	for {
		// 进入自旋条件：
		//	1. 非饥饿模式
		//	2. runtime_canSpin 返回 true
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			runtime_doSpin()
			iter++
			old = m.state
			continue
		}
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		if starving && old&mutexLocked != 0 { // 如果饥饿标记为真且锁处于锁定状态，修改锁进入饥饿状态
			new |= mutexStarving
		}
		if awoke {
			if new&mutexWoken == 0 { // 锁被唤醒
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken
		}
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				break
			}
			awoke = true
			iter = 0
		} else {
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 { // 如果没有等待队列不为空，锁状态不为空，开始慢速解锁
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	if (new+mutexLocked)&mutexLocked == 0 { // 判断互斥锁状态，处于非锁定状态报错
		throw("sync: unlock of unlocked mutex")
	}
	if new&mutexStarving == 0 { // 非饥饿模式
		old := new
		for {
			// 如果锁等待队列为空，或者锁状态已经为锁定，或者等待者被唤醒，或者处于饥饿模式，直接返回
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) { // 将锁移交给下一个等待者
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else { // 饥饿模式
		runtime_Semrelease(&m.sema, true, 1) // 将互斥锁移交给队列中等待者，互斥锁状态为未锁定，等待着被唤醒后会修改互斥锁状态为锁定
	}
}
