// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#include "textflag.h"

// NaCl entry has:
//	0(FP) - 0
//	4(FP) - cleanup function pointer, always 0
//	8(FP) - envc
//	12(FP) - argc
//	16(FP) - argv, then 0, then envv, then 0, then auxv
TEXT _rt0_arm_nacl(SB),NOSPLIT|NOFRAME,$0
	MOVW	8(R13), R0
	MOVW	$12(R13), R1
	B	runtime·rt0_go(SB)
