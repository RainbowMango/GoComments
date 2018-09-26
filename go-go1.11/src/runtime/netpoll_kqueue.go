// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin dragonfly freebsd netbsd openbsd

package runtime

// Integrated network poller (kqueue-based implementation).

import "unsafe"

var (
	kq int32 = -1
)

func netpollinit() {
	kq = kqueue()
	if kq < 0 {
		println("runtime: kqueue failed with", -kq)
		throw("runtime: netpollinit failed")
	}
	closeonexec(kq)
}

func netpolldescriptor() uintptr {
	return uintptr(kq)
}

func netpollopen(fd uintptr, pd *pollDesc) int32 {
	// Arm both EVFILT_READ and EVFILT_WRITE in edge-triggered mode (EV_CLEAR)
	// for the whole fd lifetime. The notifications are automatically unregistered
	// when fd is closed.
	var ev [2]keventt
	*(*uintptr)(unsafe.Pointer(&ev[0].ident)) = fd
	ev[0].filter = _EVFILT_READ
	ev[0].flags = _EV_ADD | _EV_CLEAR
	ev[0].fflags = 0
	ev[0].data = 0
	ev[0].udata = (*byte)(unsafe.Pointer(pd))
	ev[1] = ev[0]
	ev[1].filter = _EVFILT_WRITE
	n := kevent(kq, &ev[0], 2, nil, 0, nil)
	if n < 0 {
		return -n
	}
	return 0
}

func netpollclose(fd uintptr) int32 {
	// Don't need to unregister because calling close()
	// on fd will remove any kevents that reference the descriptor.
	return 0
}

func netpollarm(pd *pollDesc, mode int) {
	throw("runtime: unused")
}

// Polls for ready network connections.
// Returns list of goroutines that become runnable.
func netpoll(block bool) *g {
	if kq == -1 {
		return nil
	}
	var tp *timespec
	var ts timespec
	if !block {
		tp = &ts
	}
	var events [64]keventt
retry:
	n := kevent(kq, nil, 0, &events[0], int32(len(events)), tp)
	if n < 0 {
		if n != -_EINTR {
			println("runtime: kevent on fd", kq, "failed with", -n)
			throw("runtime: netpoll failed")
		}
		goto retry
	}
	var gp guintptr
	for i := 0; i < int(n); i++ {
		ev := &events[i]
		var mode int32
		switch ev.filter {
		case _EVFILT_READ:
			mode += 'r'

			// On some systems when the read end of a pipe
			// is closed the write end will not get a
			// _EVFILT_WRITE event, but will get a
			// _EVFILT_READ event with EV_EOF set.
			// Note that setting 'w' here just means that we
			// will wake up a goroutine waiting to write;
			// that goroutine will try the write again,
			// and the appropriate thing will happen based
			// on what that write returns (success, EPIPE, EAGAIN).
			if ev.flags&_EV_EOF != 0 {
				mode += 'w'
			}
		case _EVFILT_WRITE:
			mode += 'w'
		}
		if mode != 0 {
			netpollready(&gp, (*pollDesc)(unsafe.Pointer(ev.udata)), mode)
		}
	}
	if block && gp == 0 {
		goto retry
	}
	return gp.ptr()
}
