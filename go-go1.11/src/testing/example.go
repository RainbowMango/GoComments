// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package testing

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

type InternalExample struct {
	Name      string    // 测试名称
	F         func()   // 测试函数
	Output    string    // 期望字符串
	Unordered bool      // 输出是否是无序的
}

// An internal function but exported because it is cross-package; part of the implementation
// of the "go test" command.
func RunExamples(matchString func(pat, str string) (bool, error), examples []InternalExample) (ok bool) {
	_, ok = runExamples(matchString, examples)
	return ok
}

func runExamples(matchString func(pat, str string) (bool, error), examples []InternalExample) (ran, ok bool) {
	ok = true

	var eg InternalExample

	for _, eg = range examples {
		matched, err := matchString(*match, eg.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testing: invalid regexp for -test.run: %s\n", err)
			os.Exit(1)
		}
		if !matched {
			continue
		}
		ran = true
		if !runExample(eg) {
			ok = false
		}
	}

	return ran, ok
}

func sortLines(output string) string {
	lines := strings.Split(output, "\n")
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func runExample(eg InternalExample) (ok bool) {
	if *chatty {
		fmt.Printf("=== RUN   %s\n", eg.Name)
	}

	// Capture stdout.
	stdout := os.Stdout      // 备份标输出文件
	r, w, err := os.Pipe()   // 创建一个管道
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Stdout = w           // 标准输出文件暂时修改为管道的入口，即所有的标准输出实际上都会进入管道
	outC := make(chan string)
	go func() {
		var buf strings.Builder
		_, err := io.Copy(&buf, r)  // 从管道中读出数据
		r.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "testing: copying pipe: %v\n", err)
			os.Exit(1)
		}
		outC <- buf.String()  // 管道中读出的数据写入channel中
	}()

	start := time.Now()
	ok = true

	// Clean up in a deferred call so we can recover if the example panics.
	defer func() {
		dstr := fmtDuration(time.Since(start))         // 计时结束，记录测试用时

		// Close pipe, restore stdout, get output.
		w.Close()                // 关闭管道
		os.Stdout = stdout       // 恢复原标准输出
		out := <-outC            // 从channel中取出数据

		var fail string
		err := recover()
		got := strings.TrimSpace(out)        // 实际得到的打印字符串
		want := strings.TrimSpace(eg.Output) // 期望的字符串
		if eg.Unordered { // 如果输出是无序的，则把输出字符串和期望字符串排序后比较
			if sortLines(got) != sortLines(want) && err == nil {
				fail = fmt.Sprintf("got:\n%s\nwant (unordered):\n%s\n", out, eg.Output)
			}
		} else { // 如果输出是有序的，则直接比较输出字符串和期望字符串
			if got != want && err == nil {
				fail = fmt.Sprintf("got:\n%s\nwant:\n%s\n", got, want)
			}
		}
		if fail != "" || err != nil {
			fmt.Printf("--- FAIL: %s (%s)\n%s", eg.Name, dstr, fail)
			ok = false
		} else if *chatty {
			fmt.Printf("--- PASS: %s (%s)\n", eg.Name, dstr)
		}
		if err != nil {
			panic(err)
		}
	}()

	// Run example.
	eg.F()
	return
}
