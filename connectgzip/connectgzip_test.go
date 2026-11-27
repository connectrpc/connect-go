// Copyright 2021-2026 The Connect Authors
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

package connectgzip_test

import (
	"bytes"
	"io"
	"testing"

	"connectrpc.com/connect/v2/connectgzip"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	comp := connectgzip.New()
	src := bytes.Repeat([]byte("connectrpc"), 4096)

	got := decompress(t, comp, compress(t, comp, src))
	if !bytes.Equal(got, src) {
		t.Fatalf("round trip mismatch: got %d bytes, want %d", len(got), len(src))
	}
}

// TestPooledReuse exercises the recycle path: closed writers and readers
// return to the pool and must reset cleanly for the next payload.
func TestPooledReuse(t *testing.T) {
	t.Parallel()
	comp := connectgzip.New()
	for i := range 3 {
		src := bytes.Repeat([]byte{byte('a' + i)}, 1024*(i+1))
		got := decompress(t, comp, compress(t, comp, src))
		if !bytes.Equal(got, src) {
			t.Fatalf("round trip %d mismatch: got %d bytes, want %d", i, len(got), len(src))
		}
	}
}

// TestCloseHalfConsumed verifies a reader closed before EOF recycles
// safely: the next Decompress must reset the pooled state completely.
func TestCloseHalfConsumed(t *testing.T) {
	t.Parallel()
	comp := connectgzip.New()
	src := bytes.Repeat([]byte("connectrpc"), 4096)
	compressed := compress(t, comp, src)

	reader, err := comp.Decompress(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	got := decompress(t, comp, compressed)
	if !bytes.Equal(got, src) {
		t.Fatal("round trip after half-consumed close mismatch")
	}
}

// TestUseAfterClose verifies closed writers and readers reject further
// use instead of corrupting pooled state.
func TestUseAfterClose(t *testing.T) {
	t.Parallel()
	comp := connectgzip.New()

	var buf bytes.Buffer
	writer, err := comp.Compress(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err == nil {
		t.Fatal("Write after Close succeeded")
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("second Close returned %v", err)
	}

	reader, err := comp.Decompress(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read after Close succeeded")
	}
}

// TestUseAfterCloseAfterPoolReuse verifies that stale handles stay closed
// after the underlying gzip writer or reader is recycled to another caller.
func TestUseAfterCloseAfterPoolReuse(t *testing.T) {
	t.Parallel()
	t.Run("writer", func(t *testing.T) {
		t.Parallel()
		comp := connectgzip.New()
		for range 8 {
			var buf1, buf2 bytes.Buffer
			writer1, err := comp.Compress(&buf1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer1.Write([]byte("first")); err != nil {
				t.Fatal(err)
			}
			if err := writer1.Close(); err != nil {
				t.Fatal(err)
			}

			writer2, err := comp.Compress(&buf2)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer2.Write([]byte("second")); err != nil {
				t.Fatal(err)
			}

			if _, err := writer1.Write([]byte("stale")); err == nil {
				t.Fatal("stale writer Write succeeded after pool reuse")
			}
			flusher1, ok := writer1.(interface{ Flush() error })
			if !ok {
				t.Fatal("writer does not implement Flush")
			}
			if err := flusher1.Flush(); err == nil {
				t.Fatal("stale writer Flush succeeded after pool reuse")
			}
			if err := writer1.Close(); err != nil {
				t.Fatalf("stale writer second Close returned %v", err)
			}

			if _, err := writer2.Write([]byte("-tail")); err != nil {
				t.Fatalf("active writer Write failed after stale Close: %v", err)
			}
			if err := writer2.Close(); err != nil {
				t.Fatalf("active writer Close failed: %v", err)
			}
			if got := decompress(t, comp, buf2.Bytes()); string(got) != "second-tail" {
				t.Fatalf("active writer output = %q, want %q", got, "second-tail")
			}
		}
	})

	t.Run("reader", func(t *testing.T) {
		t.Parallel()
		comp := connectgzip.New()
		compressed1 := compress(t, comp, []byte("alpha"))
		compressed2 := compress(t, comp, []byte("bravo-payload"))
		for range 8 {
			reader1, err := comp.Decompress(bytes.NewReader(compressed1))
			if err != nil {
				t.Fatal(err)
			}
			if err := reader1.Close(); err != nil {
				t.Fatal(err)
			}

			reader2, err := comp.Decompress(bytes.NewReader(compressed2))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reader1.Read(make([]byte, 4)); err == nil {
				t.Fatal("stale reader Read succeeded after pool reuse")
			}
			if err := reader1.Close(); err != nil {
				t.Fatalf("stale reader second Close returned %v", err)
			}
			got, err := io.ReadAll(reader2)
			if err != nil {
				t.Fatalf("active reader ReadAll failed after stale Close: %v", err)
			}
			if string(got) != "bravo-payload" {
				t.Fatalf("active reader output = %q, want %q", got, "bravo-payload")
			}
			if err := reader2.Close(); err != nil {
				t.Fatalf("active reader Close failed: %v", err)
			}
		}
	})
}

// TestStreamingDecompressStopsEarly verifies that a bounded consumer can
// abandon a huge payload without materializing it: only the bytes read
// are decompressed.
func TestStreamingDecompressStopsEarly(t *testing.T) {
	t.Parallel()
	comp := connectgzip.New()
	// 16 MiB of zeros compresses to a few KiB but expands enormously.
	const decompressedSize = 16 << 20
	bomb := compress(t, comp, make([]byte, decompressedSize))

	reader, err := comp.Decompress(bytes.NewReader(bomb))
	if err != nil {
		t.Fatal(err)
	}
	const maxBytes = 1 << 10
	n, err := io.Copy(io.Discard, io.LimitReader(reader, maxBytes))
	if err != nil {
		t.Fatal(err)
	}
	if n != maxBytes {
		t.Fatalf("read %d bytes, want %d", n, maxBytes)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkDecompress(b *testing.B) {
	comp := connectgzip.New()
	src := bytes.Repeat([]byte("connectrpc the rpc framework "), 8192) // ~232 KiB
	compressed := compress(b, comp, src)
	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		reader, err := comp.Decompress(bytes.NewReader(compressed))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			b.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func compress(tb testing.TB, comp *connectgzip.Compressor, src []byte) []byte {
	tb.Helper()
	var buf bytes.Buffer
	writer, err := comp.Compress(&buf)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := writer.Write(src); err != nil {
		tb.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

func decompress(tb testing.TB, comp *connectgzip.Compressor, src []byte) []byte {
	tb.Helper()
	reader, err := comp.Decompress(bytes.NewReader(src))
	if err != nil {
		tb.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		tb.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		tb.Fatal(err)
	}
	return got
}
