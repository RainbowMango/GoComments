// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// There is a modified copy of this file in runtime/rwmutex.go.
// If you make any changes here, see if you should make them there.

// A RWMutex is a reader/writer mutual exclusion lock.
// The lock can be held by an arbitrary number of readers or a single writer.
// The zero value for a RWMutex is an unlocked mutex.
//
// A RWMutex must not be copied after first use.
//
// If a goroutine holds a RWMutex for reading and another goroutine might
// call Lock, no goroutine should expect to be able to acquire a read lock
// until the initial read lock is released. In particular, this prohibits
// recursive read locking. This is to ensure that the lock eventually becomes
// available; a blocked Lock call excludes new readers from acquiring the
// lock.
type RWMutex struct {
	w           Mutex  // held if there are pending writers //用于控制多个写锁，获得写锁首先要获取该锁，如果有一个写锁在进行，那么再到来的写锁将会阻塞于此
	writerSem   uint32 // semaphore for writers to wait for completing readers //写阻塞等待的信号量，最后一个读者释放锁时会释放信号量
	readerSem   uint32 // semaphore for readers to wait for completing writers //读阻塞的协程等待的信号量，持有写锁的协程释放锁后会释放readerCount个信号量
	readerCount int32  // number of pending readers //记录读者个数,获取读锁时简单+1,获取写锁时将其置为负值,用于标志已有协程持有了写锁
	readerWait  int32  // number of departing readers //获取写锁时，如果有读者，则将读者数记录于此，读者解锁时据此值来决定是否释放信号量，只有最后一个读者才需要释放信号量。
}

const rwmutexMaxReaders = 1 << 30

// RLock locks rw for reading.
//
// It should not be used for recursive read locking; a blocked Lock
// call excludes new readers from acquiring the lock. See the
// documentation on the RWMutex type.
func (rw *RWMutex) RLock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	if atomic.AddInt32(&rw.readerCount, 1) < 0 { //读者数量简单+1，如果readerCount为负值，说明有协程持有了写锁，需要等待协程解除写锁后释放信号量解锁
		// A writer is pending, wait for it.
		runtime_SemacquireMutex(&rw.readerSem, false)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
	}
}

// RUnlock undoes a single RLock call;
// it does not affect other simultaneous readers.
// It is a run-time error if rw is not locked for reading
// on entry to RUnlock.
func (rw *RWMutex) RUnlock() {
	if race.Enabled {
		_ = rw.w.state
		race.ReleaseMerge(unsafe.Pointer(&rw.writerSem))
		race.Disable()
	}
	if r := atomic.AddInt32(&rw.readerCount, -1); r < 0 { //每个读者解锁时，首先将readerCount -1，如果readerCount为负值，说明有协程在等待写锁
		if r+1 == 0 || r+1 == -rwmutexMaxReaders {
			race.Enable()
			throw("sync: RUnlock of unlocked RWMutex")
		}
		// A writer is pending.
		if atomic.AddInt32(&rw.readerWait, -1) == 0 { //将readerWait -1, 并且最后一个读者负责释放一个信号量，来唤醒等待写锁的协程
			// The last reader unblocks the writer.
			runtime_Semrelease(&rw.writerSem, false)
		}
	}
	if race.Enabled {
		race.Enable()
	}
}

// Lock locks rw for writing.
// If the lock is already locked for reading or writing,
// Lock blocks until the lock is available.
func (rw *RWMutex) Lock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// First, resolve competition with other writers.
	rw.w.Lock() //想要获取写锁,首先要获取互斥锁,下面再等待所有读者释放锁
	// Announce to readers there is a pending writer.
	r := atomic.AddInt32(&rw.readerCount, -rwmutexMaxReaders) + rwmutexMaxReaders //使用原子操作减去rwmutexMaxReaders将readerCount置为负值,目的是阻止读锁。再加上rwmutexMaxReaders又可以获取原来的读者数。非常精妙
	// Wait for active readers.
	if r != 0 && atomic.AddInt32(&rw.readerWait, r) != 0 { //如果读者数是0，那么直接获取写锁，不需要等待信号量。 因为写锁获取成功，所以此处简单的加上读者数量即可。（加上读者数量应该不会出现0的情况）
		runtime_SemacquireMutex(&rw.writerSem, false) // 续：此处将读者数写入readerWait实际上是用于排队，即当前为止的读者释放后轮到写操作，避免写锁被饿死
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
}

// Unlock unlocks rw for writing. It is a run-time error if rw is
// not locked for writing on entry to Unlock.
//
// As with Mutexes, a locked RWMutex is not associated with a particular
// goroutine. One goroutine may RLock (Lock) a RWMutex and then
// arrange for another goroutine to RUnlock (Unlock) it.
func (rw *RWMutex) Unlock() {
	if race.Enabled {
		_ = rw.w.state
		race.Release(unsafe.Pointer(&rw.readerSem))
		race.Disable()
	}

	// Announce to readers there is no active writer.
	r := atomic.AddInt32(&rw.readerCount, rwmutexMaxReaders) //因为持有写锁期间,读者数量有可能增加,此处将读者数量加上rwmutexMaxReaders,将读者数量转为正值。
	if r >= rwmutexMaxReaders {
		race.Enable()
		throw("sync: Unlock of unlocked RWMutex")
	}
	// Unblock blocked readers, if any.
	for i := 0; i < int(r); i++ { //持有锁期间，读者可能继续到来并阻塞起来，所以这里有多少个读者，释放多少个信号量
		runtime_Semrelease(&rw.readerSem, false)
	}
	// Allow other writers to proceed.
	rw.w.Unlock()
	if race.Enabled {
		race.Enable()
	}
}

// RLocker returns a Locker interface that implements
// the Lock and Unlock methods by calling rw.RLock and rw.RUnlock.
func (rw *RWMutex) RLocker() Locker {
	return (*rlocker)(rw)
}

type rlocker RWMutex

func (r *rlocker) Lock()   { (*RWMutex)(r).RLock() }
func (r *rlocker) Unlock() { (*RWMutex)(r).RUnlock() }
