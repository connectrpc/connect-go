// Copyright 2021-2023 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connect

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bufbuild/connect-go/internal/assert"
)

// Tests are based on io.Pipe tests.
// See: https://github.com/golang/go/blob/master/src/io/pipe_test.go

// Test a single read/write pair.
func TestPipe1(t *testing.T) {
	t.Parallel()
	done := make(chan int)
	pipe := &messagePipe{}
	var buf = make([]byte, 64)
	go func() {
		data := []byte("hello, world")
		n, err := pipe.Write(data)
		assert.Nil(t, err)
		assert.Equal(t, n, len(data))
		done <- 0
	}()
	n, err := pipe.Read(buf)
	assert.Nil(t, err)
	if n != 12 || string(buf[0:12]) != "hello, world" {
		t.Errorf("bad read: got %d %q", n, string(buf[0:n]))
	}
	<-done
	_ = pipe.Close()
}

// Test a sequence of read/write pairs.
func TestPipe2(t *testing.T) {
	t.Parallel()
	cBytes := make(chan int)
	pipe := &messagePipe{}
	go func() {
		var buf = make([]byte, 64)
		for {
			rbytes, err := pipe.Read(buf)
			if errors.Is(err, io.EOF) {
				cBytes <- 0
				break
			}
			if err != nil {
				t.Errorf("read: %v", err)
			}
			cBytes <- rbytes
		}
	}()
	var buf = make([]byte, 64)
	for i := 0; i < 5; i++ {
		pbuf := buf[0 : 5+i*10]
		wbytes, err := pipe.Write(pbuf)
		if wbytes != len(pbuf) {
			t.Errorf("wrote %d, got %d", len(pbuf), wbytes)
		}
		if err != nil {
			t.Errorf("write: %v", err)
		}
		rbytes := <-cBytes
		if rbytes != wbytes {
			t.Errorf("wrote %d, read got %d", wbytes, rbytes)
		}
	}
	_ = pipe.Close()
	nn := <-cBytes
	if nn != 0 {
		t.Errorf("final read got %d", nn)
	}
}

// Test a large write that requires multiple reads to satisfy.
func TestPipe3(t *testing.T) {
	t.Parallel()
	type pipeReturn struct {
		n   int
		err error
	}
	cReturns := make(chan pipeReturn)
	pipe := &messagePipe{}
	var wdat = make([]byte, 128)
	for i := 0; i < len(wdat); i++ {
		wdat[i] = byte(i)
	}
	go func() {
		n, err := pipe.Write(wdat)
		pipe.Close()
		cReturns <- pipeReturn{n, err}
	}()
	var rdat = make([]byte, 1024)
	tot := 0
	for count := 1; count <= 256; count *= 2 {
		bytesRead, err := pipe.Read(rdat[tot : tot+count])
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("read: %v", err)
		}

		// only final two reads should be short - 1 byte, then 0
		expect := count
		switch count {
		case 128:
			expect = 1
		case 256:
			expect = 0
			if !errors.Is(err, io.EOF) {
				t.Fatalf("read at end: %v", err)
			}
		}
		if bytesRead != expect {
			t.Fatalf("read %d, expected %d, got %d", count, expect, bytesRead)
		}
		tot += bytesRead
	}
	pr := <-cReturns
	if pr.n != 128 || pr.err != nil {
		t.Fatalf("write 128: %d, %v", pr.n, pr.err)
	}
	if tot != 128 {
		t.Fatalf("total read %d != 128", tot)
	}
	for i := 0; i < 128; i++ {
		if rdat[i] != byte(i) {
			t.Fatalf("rdat[%d] = %d", i, rdat[i])
		}
	}
}

// Test read after/before writer close.
func TestPipeReadClose(t *testing.T) {
	t.Parallel()
	type pipeTest struct {
		async          bool
		err            error
		closeWithError bool
	}

	var pipeTests = []pipeTest{
		{true, nil, false},
		{true, nil, true},
		{true, io.ErrShortWrite, true},
		{false, nil, false},
		{false, nil, true},
		{false, io.ErrShortWrite, true},
	}
	delayClose := func(t *testing.T, closer func(err error) error, done chan int, tt pipeTest) {
		t.Helper()
		time.Sleep(1 * time.Millisecond)
		var err error
		if tt.closeWithError {
			err = closer(tt.err)
		} else {
			err = closer(io.EOF)
		}
		assert.Nil(t, err) // delayClose
		done <- 0
	}
	for _, testcase := range pipeTests {
		testcase := testcase
		t.Run(fmt.Sprintf(
			"async=%v err=%v closeWithError=%v",
			testcase.async, testcase.err, testcase.closeWithError,
		), func(t *testing.T) {
			t.Parallel()
			done := make(chan int, 1)
			pipe := &messagePipe{}
			closer := func(err error) error {
				pipe.CloseWithErr(err)
				return nil
			}
			if testcase.async {
				go delayClose(t, closer, done, testcase)
			} else {
				delayClose(t, closer, done, testcase)
			}
			var buf = make([]byte, 64)
			rbytes, err := pipe.Read(buf)
			<-done
			want := testcase.err
			if want == nil {
				want = io.EOF
			}
			assert.ErrorIs(t, err, want) // pipe.Read
			assert.Equal(t, rbytes, 0)   // pipe.Read
			assert.Nil(t, pipe.Close())
		})
	}
}

// Test close on Read side during Read.
func TestPipeReadClose2(t *testing.T) {
	t.Parallel()
	done := make(chan int, 1)
	pipe := &messagePipe{}
	go func() {
		time.Sleep(1 * time.Millisecond)
		pipe.CloseWithErr(io.EOF) // delayClose
		done <- 0
	}()
	n, err := pipe.Read(make([]byte, 64))
	<-done
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("read from closed pipe: %v, %v want %v, %v", n, err, 0, io.ErrClosedPipe)
	}
}

// Test close on Write side during Write.
func TestPipeWriteClose2(t *testing.T) {
	t.Parallel()
	done := make(chan int, 1)
	pipe := &messagePipe{}
	go func() {
		time.Sleep(1 * time.Millisecond)
		_ = pipe.Close() // delayClose
		done <- 0
	}()
	n, err := pipe.Write(make([]byte, 64))
	<-done
	if n != 0 || errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("write to closed pipe: %v, %v want %v, %v", n, err, 0, io.ErrClosedPipe)
	}
}

func TestPipeWriteEmpty(t *testing.T) {
	t.Parallel()
	pipe := &messagePipe{}
	go func() {
		_, _ = pipe.Write([]byte{})
		_ = pipe.Close()
	}()
	var b [2]byte
	_, _ = io.ReadFull(pipe, b[0:2])
	_ = pipe.Close()
}

func TestPipeWriteNil(t *testing.T) {
	t.Parallel()
	pipe := &messagePipe{}
	go func() {
		_, _ = pipe.Write(nil)
		_ = pipe.Close()
	}()
	var b [2]byte
	_, _ = io.ReadFull(pipe, b[0:2])
	pipe.CloseWithErr(io.ErrClosedPipe)
}

func TestPipeWriteAfterWriterClose(t *testing.T) {
	t.Parallel()
	pipe := &messagePipe{}

	done := make(chan bool)
	var writeErr error
	go func() {
		_, err := pipe.Write([]byte("hello"))
		if err != nil {
			t.Errorf("got error: %q; expected none", err)
		}
		pipe.Close()
		_, writeErr = pipe.Write([]byte("world"))
		done <- true
	}()

	buf := make([]byte, 100)
	var result string
	n, err := io.ReadFull(pipe, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got: %q; want: %q", err, io.ErrUnexpectedEOF)
	}
	result = string(buf[0:n])
	<-done

	if result != "hello" {
		t.Errorf("got: %q; want: %q", result, "hello")
	}
	if errors.Is(writeErr, io.ErrClosedPipe) {
		t.Errorf("got: %q; want: %q", writeErr, io.ErrClosedPipe)
	}
}

func TestPipeConcurrent(t *testing.T) {
	t.Parallel()
	const (
		input    = "0123456789abcdef"
		count    = 8
		readSize = 2
	)
	t.Run("Write", func(t *testing.T) {
		t.Parallel()
		pipe := &messagePipe{}

		for i := 0; i < count; i++ {
			go func() {
				time.Sleep(time.Millisecond) // Increase probability of race
				if n, err := pipe.Write([]byte(input)); n != len(input) || err != nil {
					t.Errorf("Write() = (%d, %v); want (%d, nil)", n, err, len(input))
				}
			}()
		}

		buf := make([]byte, count*len(input))
		for i := 0; i < len(buf); i += readSize {
			if n, err := pipe.Read(buf[i : i+readSize]); n != readSize || err != nil {
				t.Errorf("Read() = (%d, %v); want (%d, nil)", n, err, readSize)
			}
		}

		// Since each Write is fully gated, if multiple Read calls were needed,
		// the contents of Write should still appear together in the output.
		got := string(buf)
		want := strings.Repeat(input, count)
		if got != want {
			t.Errorf("got: %q; want: %q", got, want)
		}
	})
	t.Run("Read", func(t *testing.T) {
		t.Parallel()
		pipe := &messagePipe{}

		cbytes := make(chan []byte, count*len(input)/readSize)
		for i := 0; i < cap(cbytes); i++ {
			go func() {
				time.Sleep(time.Millisecond) // Increase probability of race
				buf := make([]byte, readSize)
				if n, err := pipe.Read(buf); n != readSize || err != nil {
					t.Errorf("Read() = (%d, %v); want (%d, nil)", n, err, readSize)
				}
				cbytes <- buf
			}()
		}

		for i := 0; i < count; i++ {
			if n, err := pipe.Write([]byte(input)); n != len(input) || err != nil {
				t.Errorf("Write() = (%d, %v); want (%d, nil)", n, err, len(input))
			}
		}

		// Since each read is independent, the only guarantee about the output
		// is that it is a permutation of the input in readSized groups.
		got := make([]byte, 0, count*len(input))
		for i := 0; i < cap(cbytes); i++ {
			got = append(got, (<-cbytes)...)
		}
		got = sortBytesInGroups(got, readSize)
		want := bytes.Repeat([]byte(input), count)
		want = sortBytesInGroups(want, readSize)
		if string(got) != string(want) {
			t.Errorf("got: %q; want: %q", got, want)
		}
	})
}

func sortBytesInGroups(b []byte, n int) []byte {
	var groups [][]byte
	for len(b) > 0 {
		groups = append(groups, b[:n])
		b = b[n:]
	}
	sort.Slice(groups, func(i, j int) bool { return bytes.Compare(groups[i], groups[j]) < 0 })
	return bytes.Join(groups, nil)
}

func BenchmarkPipe(b *testing.B) {
	for _, size := range []int{1, 512, 1024, 1024 * 1024} {
		size := size
		buf := make([]byte, size)
		b.Run(fmt.Sprintf("io.Pipe:size=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			pipeReader, pipeWriter := io.Pipe()
			go func() {
				for i := 0; i < b.N; i++ {
					_, _ = pipeWriter.Write(buf)
				}
				_ = pipeWriter.Close()
			}()
			buf, err := io.ReadAll(pipeReader)
			assert.Nil(b, err)
			assert.Equal(b, size*b.N, len(buf))
		})
		b.Run(fmt.Sprintf("messagePipe:size=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			pipe := &messagePipe{}
			go func() {
				for i := 0; i < b.N; i++ {
					_, _ = pipe.Write(buf)
				}
				_ = pipe.Close()
			}()
			buf, err := io.ReadAll(pipe)
			assert.Nil(b, err)
			assert.Equal(b, size*b.N, len(buf))
		})
	}
}

func TestPipeBuffer(t *testing.T) {
	t.Parallel()
	t.Run("SingleWrite", func(t *testing.T) {
		t.Parallel()
		pipe := &messagePipe{}
		pipe.buffer = &bytes.Buffer{}
		pipe.limit = maxRPCClientBufferSize

		go func() {
			_, _ = pipe.Write([]byte("hello"))
			_ = pipe.Close()
		}()

		var buf [5]byte
		_, err := pipe.Read(buf[:])
		assert.Nil(t, err)
		assert.Equal(t, string(buf[:]), "hello")

		assert.True(t, pipe.Rewind())

		buf = [5]byte{}
		_, err = pipe.Read(buf[:])
		assert.Nil(t, err)
		assert.Equal(t, string(buf[:]), "hello")

		n, err := pipe.Read(buf[:])
		assert.Equal(t, n, 0)
		t.Log(err)
		assert.ErrorIs(t, err, io.EOF)
	})
	t.Run("MultipleWrite", func(t *testing.T) {
		t.Parallel()
		pipe := &messagePipe{}
		pipe.buffer = &bytes.Buffer{}
		pipe.limit = maxRPCClientBufferSize

		go func() {
			_, _ = pipe.Write([]byte("hello"))
			_, _ = pipe.Write([]byte("world"))
			_ = pipe.Close()
		}()

		var buf [6]byte
		rbytes, err := pipe.Read(buf[:])
		assert.Nil(t, err)
		nn, err := pipe.Read(buf[rbytes:])
		assert.Nil(t, err)
		assert.Equal(t, string(buf[:nn+rbytes]), "hellow")

		assert.True(t, pipe.Rewind())

		var buf2 [5]byte
		rbytes, err = pipe.Read(buf2[:])
		assert.Nil(t, err)
		assert.Equal(t, string(buf2[:rbytes]), "hello")
		rbytes, err = pipe.Read(buf2[:])
		assert.Nil(t, err)
		assert.Equal(t, string(buf2[:rbytes]), "world")

		_, err = pipe.Read(buf[:])
		assert.ErrorIs(t, err, io.EOF)
	})
}
