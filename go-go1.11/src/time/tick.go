// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package time

import "errors"

// A Ticker holds a channel that delivers `ticks' of a clock
// at intervals.
type Ticker struct {
	C <-chan Time // The channel on which the ticks are delivered.
	r runtimeTimer
}

// NewTicker returns a new Ticker containing a channel that will send the
// time with a period specified by the duration argument.
// It adjusts the intervals or drops ticks to make up for slow receivers.
// The duration d must be greater than zero; if not, NewTicker will panic.
// Stop the ticker to release associated resources.
func NewTicker(d Duration) *Ticker {
	if d <= 0 {
		panic(errors.New("non-positive interval for NewTicker"))
	}
	// Give the channel a 1-element time buffer.
	// If the client falls behind while reading, we drop ticks
	// on the floor until the client catches up.
	c := make(chan Time, 1)
	t := &Ticker{
		C: c,
		r: runtimeTimer{
			when:   when(d),
			period: int64(d), // Ticker跟Timer的重要区就是提供了period这个参数，据此决定timer是一次性的，还是周期性的
			f:      sendTime,
			arg:    c,
		},
	}
	startTimer(&t.r)
	return t
}

// Stop turns off a ticker. After Stop, no more ticks will be sent.
// Stop does not close the channel, to prevent a concurrent goroutine
// reading from the channel from seeing an erroneous "tick".
func (t *Ticker) Stop() {
	stopTimer(&t.r)
}

// Tick is a convenience wrapper for NewTicker providing access to the ticking    // Tick()是对NewTicker()的封装，只提供ticker的管道
// channel only. While Tick is useful for clients that have no need to shut down  // Tick()在不需要关闭的场景下非常有用，但必须明确因为没有手段关闭底层的Ticker，所以会有"泄露"的风险
// the Ticker, be aware that without a way to shut it down the underlying
// Ticker cannot be recovered by the garbage collector; it "leaks".
// Unlike NewTicker, Tick will return nil if d <= 0.                              // Tick()不像NewTicker()，当d <= 0时它返回nil
func Tick(d Duration) <-chan Time {
	if d <= 0 {
		return nil
	}
	return NewTicker(d).C
}
