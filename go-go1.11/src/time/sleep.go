// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package time

// Sleep pauses the current goroutine for at least the duration d.
// A negative or zero duration causes Sleep to return immediately.
func Sleep(d Duration)

// runtimeNano returns the current value of the runtime clock in nanoseconds.
func runtimeNano() int64

// Interface to timers implemented in package runtime.
// Must be in sync with ../runtime/time.go:/^type timer
type runtimeTimer struct {
	tb uintptr
	i  int

	when   int64
	period int64
	f      func(interface{}, uintptr) // NOTE: must not be closure
	arg    interface{}
	seq    uintptr
}

// when is a helper function for setting the 'when' field of a runtimeTimer.
// It returns what the time will be, in nanoseconds, Duration d in the future.
// If d is negative, it is ignored. If the returned value would be less than
// zero because of an overflow, MaxInt64 is returned.
func when(d Duration) int64 {
	if d <= 0 {
		return runtimeNano()
	}
	t := runtimeNano() + int64(d)
	if t < 0 {
		t = 1<<63 - 1 // math.MaxInt64
	}
	return t
}

func startTimer(*runtimeTimer)
func stopTimer(*runtimeTimer) bool

// The Timer type represents a single event.
// When the Timer expires, the current time will be sent on C,
// unless the Timer was created by AfterFunc.
// A Timer must be created with NewTimer or AfterFunc.
type Timer struct { // Timer代表一次定时，时间到来后仅发生一个事件。
	C <-chan Time
	r runtimeTimer
}

// Stop prevents the Timer from firing.
// It returns true if the call stops the timer, false if the timer has already
// expired or been stopped.
// Stop does not close the channel, to prevent a read from the channel succeeding
// incorrectly.
//
// To prevent a timer created with NewTimer from firing after a call to Stop,
// check the return value and drain the channel.
// For example, assuming the program has not received from t.C already:
//
// 	if !t.Stop() {
// 		<-t.C
// 	}
//
// This cannot be done concurrent to other receives from the Timer's
// channel.
//
// For a timer created with AfterFunc(d, f), if t.Stop returns false, then the timer
// has already expired and the function f has been started in its own goroutine;
// Stop does not wait for f to complete before returning.
// If the caller needs to know whether f is completed, it must coordinate
// with f explicitly.
func (t *Timer) Stop() bool {
	if t.r.f == nil {
		panic("time: Stop called on uninitialized Timer")
	}
	return stopTimer(&t.r) // 此处停止定时器，是把Timer.runtimeTimer从系统维护的timer堆中删除，否则系统会持续维护timer，从而造成资源泄露
}

// NewTimer creates a new Timer that will send
// the current time on its channel after at least duration d.
func NewTimer(d Duration) *Timer {
	c := make(chan Time, 1)  // 创建一个管道
	t := &Timer{ // 构造timer数据结构
		C: c,               // 新创建的管道
		r: runtimeTimer{
			when: when(d),  // 时间
			f:    sendTime, // 时间到来后执行函数sendTime
			arg:  c,        // 时间到来后执行函数sendTime时附带的参数
		},
	}
	startTimer(&t.r) // 此处启动定时器，只是把runtimeTimer放到系统维护的timer堆中，由系统维护timer
	return t
}

// Reset changes the timer to expire after duration d.
// It returns true if the timer had been active, false if the timer had
// expired or been stopped.
//
// Resetting a timer must take care not to race with the send into t.C
// that happens when the current timer expires.
// If a program has already received a value from t.C, the timer is known
// to have expired, and t.Reset can be used directly.
// If a program has not yet received a value from t.C, however,
// the timer must be stopped and—if Stop reports that the timer expired
// before being stopped—the channel explicitly drained:
//
// 	if !t.Stop() {
// 		<-t.C
// 	}
// 	t.Reset(d)
//
// This should not be done concurrent to other receives from the Timer's
// channel.
//
// Note that it is not possible to use Reset's return value correctly, as there
// is a race condition between draining the channel and the new timer expiring.
// Reset should always be invoked on stopped or expired channels, as described above.
// The return value exists to preserve compatibility with existing programs.
func (t *Timer) Reset(d Duration) bool { // 重启定时器意味着停止先前的定时器并重新启动新的
	if t.r.f == nil {
		panic("time: Reset called on uninitialized Timer")
	}
	w := when(d)
	active := stopTimer(&t.r)
	t.r.when = w
	startTimer(&t.r)
	return active
}

func sendTime(c interface{}, seq uintptr) { // NewTimer 和 NewTicker共用的发送时间到管道的方法
	// Non-blocking send of time on c.
	// Used in NewTimer, it cannot block anyway (buffer). // NewTimer的管道含有一个缓存,所以绝不会阻塞
	// Used in NewTicker, dropping sends on the floor is  // NewTicker场景下如果阻塞说明接收者处理的慢，就直接放弃本次发送，放弃也没有问题，因为发送是周期的。
	// the desired behavior when the reader gets behind,
	// because the sends are periodic.
	select {
	case c.(chan Time) <- Now():
	default: // 因为有default，所以如果管道中满了的情况下，会直接通过default结束函数
	}
}

// After waits for the duration to elapse and then sends the current time
// on the returned channel.
// It is equivalent to NewTimer(d).C.
// The underlying Timer is not recovered by the garbage collector
// until the timer fires. If efficiency is a concern, use NewTimer
// instead and call Timer.Stop if the timer is no longer needed.
func After(d Duration) <-chan Time {
	return NewTimer(d).C
}

// AfterFunc waits for the duration to elapse and then calls f
// in its own goroutine. It returns a Timer that can
// be used to cancel the call using its Stop method.
func AfterFunc(d Duration, f func()) *Timer {
	t := &Timer{
		r: runtimeTimer{
			when: when(d),
			f:    goFunc,
			arg:  f,
		},
	}
	startTimer(&t.r)
	return t
}

func goFunc(arg interface{}, seq uintptr) {
	go arg.(func())()
}
