// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Time-related runtime and pieces of package time.

package runtime

import (
	"runtime/internal/sys"
	"unsafe"
)

// Package time knows the layout of this structure.
// If this struct changes, adjust ../time/sleep.go:/runtimeTimer.
// For GOOS=nacl, package syscall knows the layout of this structure.
// If this struct changes, adjust ../syscall/net_nacl.go:/runtimeTimer.
type timer struct {
	tb *timersBucket // the bucket the timer lives in   // 当前定时器寄存于系统timer堆的地址
	i  int           // heap index                      // 当前定时器寄存于系统timer堆的下标

	// Timer wakes up at when, and then at when+period, ... (period > 0 only)
	// each time calling f(arg, now) in the timer goroutine, so f must be
	// a well-behaved function and not block.
	when   int64                                        // 当前定时器下次触发时间
	period int64                                        // 当前定时器周期触发间隔（如果是Timer，间隔为0，表示不重复触发）
	f      func(interface{}, uintptr)                 // 定时器触发时执行的函数
	arg    interface{}                                // 定时器触发时执行函数传递的参数一
	seq    uintptr                                     // 定时器触发时执行函数传递的参数二(该参数只在网络收发场景下使用)
}

// timersLen is the length of timers array.
//
// Ideally, this would be set to GOMAXPROCS, but that would require
// dynamic reallocation
//
// The current value is a compromise between memory usage and performance
// that should cover the majority of GOMAXPROCS values used in the wild.
const timersLen = 64

// timers contains "per-P" timer heaps.
//
// Timers are queued into timersBucket associated with the current P,
// so each P may work with its own timers independently of other P instances.
//
// Each timersBucket may be associated with multiple P
// if GOMAXPROCS > timersLen.
var timers [timersLen]struct {
	timersBucket

	// The padding should eliminate false sharing
	// between timersBucket values.
	pad [sys.CacheLineSize - unsafe.Sizeof(timersBucket{})%sys.CacheLineSize]byte
}

func (t *timer) assignBucket() *timersBucket {
	id := uint8(getg().m.p.ptr().id) % timersLen
	t.tb = &timers[id].timersBucket
	return t.tb
}

//go:notinheap
type timersBucket struct {
	lock         mutex
	gp           *g          // 处理堆中事件的协程
	created      bool        // 事件处理协程是否已创建，默认为false，添加首个定时器时置为true
	sleeping     bool        // 事件处理协程（gp）是否在睡眠(如果t中有定时器，还未到触发的时间，那么gp会投入睡眠)
	rescheduling bool        // 事件处理协程（gp）是否已暂停（如果t中定时器均已删除，那么gp会暂停）
	sleepUntil   int64       // 事件处理协程睡眠时间
	waitnote     note        // 事件处理协程睡眠事件（据此唤醒协程）
	t            []*timer    // 定时器切片
}

// nacl fake time support - time in nanoseconds since 1970
var faketime int64

// Package time APIs.
// Godoc uses the comments in package time, not these.

// time.now is implemented in assembly.

// timeSleep puts the current goroutine to sleep for at least ns nanoseconds.
//go:linkname timeSleep time.Sleep
func timeSleep(ns int64) {
	if ns <= 0 {
		return
	}

	gp := getg()
	t := gp.timer
	if t == nil {
		t = new(timer)
		gp.timer = t
	}
	*t = timer{}
	t.when = nanotime() + ns
	t.f = goroutineReady
	t.arg = gp
	tb := t.assignBucket()
	lock(&tb.lock)
	if !tb.addtimerLocked(t) {
		unlock(&tb.lock)
		badTimer()
	}
	goparkunlock(&tb.lock, waitReasonSleep, traceEvGoSleep, 2)
}

// startTimer adds t to the timer heap.
//go:linkname startTimer time.startTimer
func startTimer(t *timer) {
	if raceenabled {
		racerelease(unsafe.Pointer(t))
	}
	addtimer(t)
}

// stopTimer removes t from the timer heap if it is there.
// It returns true if t was removed, false if t wasn't even there.
//go:linkname stopTimer time.stopTimer
func stopTimer(t *timer) bool {
	return deltimer(t)
}

// Go runtime.

// Ready the goroutine arg.
func goroutineReady(arg interface{}, seq uintptr) {
	goready(arg.(*g), 0)
}

func addtimer(t *timer) {
	tb := t.assignBucket()
	lock(&tb.lock)
	ok := tb.addtimerLocked(t)
	unlock(&tb.lock)
	if !ok {
		badTimer()
	}
}

// Add a timer to the heap and start or kick timerproc if the new timer is
// earlier than any of the others.                                           // 添加定时器到堆中，如果定时器比堆中所有定时器都早，则立即唤醒timerproc
// Timers are locked.
// Returns whether all is well: false if the data structure is corrupt
// due to user-level races.
func (tb *timersBucket) addtimerLocked(t *timer) bool {
	// when must never be negative; otherwise timerproc will overflow
	// during its delta calculation and never expire other runtime timers.
	if t.when < 0 {
		t.when = 1<<63 - 1
	}
	t.i = len(tb.t)                 // 先把定时器插入到堆尾
	tb.t = append(tb.t, t)          // 保存定时器
	if !siftupTimer(tb.t, t.i) {    // 堆中插入数据，触发堆重新排序
		return false
	}
	if t.i == 0 { // 堆排序后，发现新插入的定时器跑到了栈顶，需要唤醒协程来处理
		// siftup moved to top: new earliest deadline.
		if tb.sleeping {                 // 协程在睡眠，唤醒协程来处理新加入的定时器
			tb.sleeping = false
			notewakeup(&tb.waitnote)
		}
		if tb.rescheduling {             // 协程已暂停，唤醒协程来处理新加入的定时器
			tb.rescheduling = false
			goready(tb.gp, 0)
		}
	}
	if !tb.created {       // 如果是系统首个定时器，则启动协程处理堆中的定时器
		tb.created = true
		go timerproc(tb)
	}
	return true
}

// Delete timer t from the heap.
// Do not need to update the timerproc: if it wakes up early, no big deal.
func deltimer(t *timer) bool {
	if t.tb == nil {
		// t.tb can be nil if the user created a timer
		// directly, without invoking startTimer e.g
		//    time.Ticker{C: c}
		// In this case, return early without any deletion.
		// See Issue 21874.
		return false
	}

	tb := t.tb

	lock(&tb.lock)
	// t may not be registered anymore and may have
	// a bogus i (typically 0, if generated by Go).
	// Verify it before proceeding.
	i := t.i
	last := len(tb.t) - 1
	if i < 0 || i > last || tb.t[i] != t {
		unlock(&tb.lock)
		return false
	}
	if i != last {
		tb.t[i] = tb.t[last]
		tb.t[i].i = i
	}
	tb.t[last] = nil
	tb.t = tb.t[:last]
	ok := true
	if i != last {
		if !siftupTimer(tb.t, i) {
			ok = false
		}
		if !siftdownTimer(tb.t, i) {
			ok = false
		}
	}
	unlock(&tb.lock)
	if !ok {
		badTimer()
	}
	return true
}

// Timerproc runs the time-driven events.
// It sleeps until the next event in the tb heap.
// If addtimer inserts a new earlier event, it wakes timerproc early.
func timerproc(tb *timersBucket) { // 执行由时间驱动的事件，持续睡眠到位于堆顶的下一个事件到来（如果有新的事件加入，且新事件会最早触发，则立即唤醒）。
	tb.gp = getg()  // 保存执行堆事件的协程
	for {  // 协程一旦启动就是无限循环，没有退出条件
		lock(&tb.lock)
		tb.sleeping = false
		now := nanotime()
		delta := int64(-1)
		for {  // 开始遍历堆来处理事件
			if len(tb.t) == 0 {     // 堆为空，说明没有定时器，停止遍历
				delta = -1
				break
			}
			t := tb.t[0]            // 取栈顶timer
			delta = t.when - now
			if delta > 0 {         // 如果delta > 0 说明堆顶事件时间还没到，直接退出，进入睡眠
				break
			}
			ok := true
			if t.period > 0 {      // 如果period > 0 说明这个是周期性的定时器（tiker），调整下次执行时间，放重新放回堆中排序
				// leave in heap but adjust next time to fire
				t.when += t.period * (1 + -delta/t.period)
				if !siftdownTimer(tb.t, 0) {
					ok = false
				}
			} else {              // 如果period <= 0（实际上是0），说明是一次性的定时器，从堆项元素移除，并调整堆
				// remove from heap
				last := len(tb.t) - 1
				if last > 0 {
					tb.t[0] = tb.t[last]
					tb.t[0].i = 0
				}
				tb.t[last] = nil
				tb.t = tb.t[:last]
				if last > 0 {
					if !siftdownTimer(tb.t, 0) {
						ok = false
					}
				}
				t.i = -1 // mark as removed
			}
			f := t.f
			arg := t.arg
			seq := t.seq
			unlock(&tb.lock)
			if !ok {
				badTimer()
			}
			if raceenabled {
				raceacquire(unsafe.Pointer(t))
			}
			f(arg, seq)          // 执行定时器既定的方法，此处没有启动协程，因为此处调用的方法必须不会阻塞
			lock(&tb.lock)
		}
		if delta < 0 || faketime > 0 {   // 堆中没有定时器，进入不定期睡眠，需要添加定时器时goready才可以唤醒
			// No timers left - put goroutine to sleep.
			tb.rescheduling = true
			goparkunlock(&tb.lock, waitReasonTimerGoroutineIdle, traceEvGoBlock, 1)
			continue
		}
		// At least one timer pending. Sleep until then.
		tb.sleeping = true
		tb.sleepUntil = now + delta
		noteclear(&tb.waitnote)
		unlock(&tb.lock)
		notetsleepg(&tb.waitnote, delta)
	}
}

func timejump() *g {
	if faketime == 0 {
		return nil
	}

	for i := range timers {
		lock(&timers[i].lock)
	}
	gp := timejumpLocked()
	for i := range timers {
		unlock(&timers[i].lock)
	}

	return gp
}

func timejumpLocked() *g {
	// Determine a timer bucket with minimum when.
	var minT *timer
	for i := range timers {
		tb := &timers[i]
		if !tb.created || len(tb.t) == 0 {
			continue
		}
		t := tb.t[0]
		if minT == nil || t.when < minT.when {
			minT = t
		}
	}
	if minT == nil || minT.when <= faketime {
		return nil
	}

	faketime = minT.when
	tb := minT.tb
	if !tb.rescheduling {
		return nil
	}
	tb.rescheduling = false
	return tb.gp
}

func timeSleepUntil() int64 {
	next := int64(1<<63 - 1)

	// Determine minimum sleepUntil across all the timer buckets.
	//
	// The function can not return a precise answer,
	// as another timer may pop in as soon as timers have been unlocked.
	// So lock the timers one by one instead of all at once.
	for i := range timers {
		tb := &timers[i]

		lock(&tb.lock)
		if tb.sleeping && tb.sleepUntil < next {
			next = tb.sleepUntil
		}
		unlock(&tb.lock)
	}

	return next
}

// Heap maintenance algorithms.
// These algorithms check for slice index errors manually.
// Slice index error can happen if the program is using racy
// access to timers. We don't want to panic here, because
// it will cause the program to crash with a mysterious
// "panic holding locks" message. Instead, we panic while not
// holding a lock.
// The races can occur despite the bucket locks because assignBucket
// itself is called without locks, so racy calls can cause a timer to
// change buckets while executing these functions.

func siftupTimer(t []*timer, i int) bool { // 添加元素后向上调整堆
	if i >= len(t) {
		return false
	}
	when := t[i].when
	tmp := t[i]
	for i > 0 {
		p := (i - 1) / 4 // parent // 注意，这里不是二叉堆，而是4叉堆，查找父节点算法略有区别
		if when >= t[p].when {    // timer的触发时间比父节点晚，则退出循环
			break
		}
		t[i] = t[p]               // (timer的触发时间比父节点早) 则与父节点交换
		t[i].i = i                // (因为把父节点换位置了)修改父节点内部记录的位置
		i = p                     // 下一次比较从父节点开始
	}
	if tmp != t[i] {             // 如果发生过调整（此时i值已变成目标位置），则把目标timer放到最终位置
		t[i] = tmp               // 把目标timer放到最终位置
		t[i].i = i               // 修改目标timer内部记录的位置
	}
	return true
}

func siftdownTimer(t []*timer, i int) bool { // 删除堆顶元素后向下调整堆
	n := len(t)
	if i >= n {
		return false
	}
	when := t[i].when
	tmp := t[i]
	for {
		c := i*4 + 1 // left child
		c3 := c + 2  // mid child
		if c >= n {
			break
		}
		w := t[c].when
		if c+1 < n && t[c+1].when < w {
			w = t[c+1].when
			c++
		}
		if c3 < n {
			w3 := t[c3].when
			if c3+1 < n && t[c3+1].when < w3 {
				w3 = t[c3+1].when
				c3++
			}
			if w3 < w {
				w = w3
				c = c3
			}
		}
		if w >= when {
			break
		}
		t[i] = t[c]
		t[i].i = i
		i = c
	}
	if tmp != t[i] {
		t[i] = tmp
		t[i].i = i
	}
	return true
}

// badTimer is called if the timer data structures have been corrupted,
// presumably due to racy use by the program. We panic here rather than
// panicing due to invalid slice access while holding locks.
// See issue #25686.
func badTimer() {
	panic(errorString("racy use of timers"))
}

// Entry points for net, time to call nanotime.

//go:linkname poll_runtimeNano internal/poll.runtimeNano
func poll_runtimeNano() int64 {
	return nanotime()
}

//go:linkname time_runtimeNano time.runtimeNano
func time_runtimeNano() int64 {
	return nanotime()
}

// Monotonic times are reported as offsets from startNano.
// We initialize startNano to nanotime() - 1 so that on systems where
// monotonic time resolution is fairly low (e.g. Windows 2008
// which appears to have a default resolution of 15ms),
// we avoid ever reporting a nanotime of 0.
// (Callers may want to use 0 as "time not set".)
var startNano int64 = nanotime() - 1
