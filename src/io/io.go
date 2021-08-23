// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package io

import (
	"errors"
	"sync"
)

/*
	io 包定义了 I/O 原语的基本接口和一些常用的对象及方法

	接口
		- Reader					: 定义读 I/O 方法
		- Writer					: 定义写 I/O 方法
		- Closer					: 定义关闭 I/O 方法
		- Seeker					: 定义 设置下一次读取或写入的偏移量，返回相对于文件开头的新偏移量 方法
		- ReadWriter			: 包含 Reader 和 Writer 接口
		- ReadCloser			: 包含 Reader 和 Closer 接口
		- WriteCloser			: 包含 Writer 和 Closer 接口
		- ReadWriteCloser	: 包含 Reader、Writer 、Closer 接口
		- ReadSeeker			: 包含 Reader、Seeker 接口
		- ReadSeekCloser	: 包含 Reader、Seeker、Closer 接口
		- WriteSeeker			: 包含 Writer、Seeker 接口
		- ReadWriteSeeker	: 包含 Reader、Writer、Seeker 接口
		- ReaderFrom			: 定义从指定对象读取数据的方法
		- WriterTo				: 定义向指定写对象写入数据的方法
		- ReaderAt				: 定义从指定偏移量开始写入数据
		- WriterAt				: 定义从指定偏移量开始读取数据
		- ByteReader			: 定义返回读取的字节方法
		- ByteScanner			: 定义前后两次的 ByteReader 调用返回相同符文方法
		- ByteWriter			: 定义写字节方法
		- RuneReader			: 定义读取取单个 UFT-8 编码的 Unicode 字符并返回符文及其字节大小函数
		- RuneScanner			: 定义前后两次的 RuneReader 调用返回相同符文方法
		- StringWriter		: 定义将字符串写入写入对象，返回写入字节大小和可能发生的错误方法

	对象
		- LimitReader			: 返回一个只读取 n 字节 Reader
		- NewSectionReader: 返回一个区间读取 Reader，从 off 开始读取 n 字节
		- TeeReader				: 返回一个 teeReader，该 reader 无内部缓冲，写入必须在读取完成之前
		- NopCloser				: 返回一个无操作 Close 方法的 ReadCloser

	方法
		- WriteString : 将字符串写入写入对象，返回写入字节大小和可能发生的错误
		- ReadAtLeast : 从 r 中至少读取 min 字节到 buf
		- ReadFull		: 从 r 中准确读取 len(buf) 字节到 buf
		- CopyN				: 从 src 复制 n 个字节到 dst
		- Copy				: 分配缓冲区来进行复制
		- CopyBuffer	: 提供缓冲区来进行复制，而不是分配一个缓冲区
		- ReadAll			: 从 reader 中读取数据，返回读取的数据及可能发生的错误
*/

// Seek whence values.
const (
	SeekStart   = 0 // seek relative to the origin of the file
	SeekCurrent = 1 // seek relative to the current offset
	SeekEnd     = 2 // seek relative to the end
)

// ErrShortWrite means that a write accepted fewer bytes than requested
// but failed to return an explicit error.
var ErrShortWrite = errors.New("short write")

// errInvalidWrite means that a write returned an impossible count.
var errInvalidWrite = errors.New("invalid write result")

// ErrShortBuffer means that a read required a longer buffer than was provided.
var ErrShortBuffer = errors.New("short buffer")

// EOF is the error returned by Read when no more input is available.
// (Read must return EOF itself, not an error wrapping EOF,
// because callers will test for EOF using ==.)
// Functions should return EOF only to signal a graceful end of input.
// If the EOF occurs unexpectedly in a structured data stream,
// the appropriate error is either ErrUnexpectedEOF or some other error
// giving more detail.
var EOF = errors.New("EOF")

// ErrUnexpectedEOF means that EOF was encountered in the
// middle of reading a fixed-size block or data structure.
var ErrUnexpectedEOF = errors.New("unexpected EOF")

// ErrNoProgress is returned by some clients of an Reader when
// many calls to Read have failed to return any data or error,
// usually the sign of a broken Reader implementation.
var ErrNoProgress = errors.New("multiple Read calls return no data or error")

// Reader 接口表示读取，包含一个 Read 方法，用于 I/O 读取
// 1.Read 方法接收一个字节数据，返回读取的字节数和可能遇到的错误
// 2.读取结束应该返回一个 EOF 错误，而不是 0
type Reader interface {
	Read(p []byte) (n int, err error)
}

// Writer 接口表示写，包含一个 Write 方法，用于写 I/O
// 1.Write 方法接收一个字节数据，将接收的字节数据写入底层数据流，返回写入的字节数和可能遇到的错误
// 2.返回的写入字节数应该与传入的写入字节数长度相等
type Writer interface {
	Write(p []byte) (n int, err error)
}

// Closer 接口表示关闭，包含一个 Close 方法，用于关闭 I/O
// 1.Close 结构未明确定义相关行文，实现者可以根据需要定制相关行为
type Closer interface {
	Close() error
}

// Seeker 接口
// 1.Seek 方法设置下一次读取或写入的偏移量，返回相对于文件开头的新偏移量和错误
// 2.whence 函数可选类型：
// 		- SeekStart: 偏移量相对于文件开头
//		- SeekCurrent: 偏移量相对于当前偏移量
//		- SeekEnd: 偏移量相对于结尾
type Seeker interface {
	Seek(offset int64, whence int) (int64, error)
}

// ReadWriter 接口包含 Reader 和 Writer 接口，表示支持读写操作
type ReadWriter interface {
	Reader
	Writer
}

// ReadCloser 接口包含 Reader 和 Closer 接口，表示支持读和关闭操作
type ReadCloser interface {
	Reader
	Closer
}

// WriteCloser 接口包含 Writer 和 Closer 接口，表示支持写和关闭操作
type WriteCloser interface {
	Writer
	Closer
}

// ReadWriteCloser 接口包含 Reader、Writer 、Closer 接口，表示支持读、写和关闭操作
type ReadWriteCloser interface {
	Reader
	Writer
	Closer
}

// ReadSeeker 接口包含 Reader、Seeker 接口，表示支持读、修改偏移量操作
type ReadSeeker interface {
	Reader
	Seeker
}

// ReadSeekCloser 接口包含 Reader、Seeker、Closer 接口，表示支持读、修改偏移量、关闭操作
type ReadSeekCloser interface {
	Reader
	Seeker
	Closer
}

// WriteSeeker 接口包含 Writer、Seeker 接口，表示支持写、修改偏移量操作
type WriteSeeker interface {
	Writer
	Seeker
}

// ReadWriteSeeker 接口包含 Reader、Writer、Seeker 接口，表示支持读、写、修改偏移量操作
type ReadWriteSeeker interface {
	Reader
	Writer
	Seeker
}

// ReaderFrom 接口定义从指定对象读入数据
// 1.ReadFrom 方法接收一个读取对象，返回从该对象的读取的字节数和可能遇到的错误
type ReaderFrom interface {
	ReadFrom(r Reader) (n int64, err error)
}

// WriterTo 接口定义向指定对象写入数据
// 1.WriteTo 方法接收一个写入对象，返回写入到该对象的字节数和可能遇到的错误
type WriterTo interface {
	WriteTo(w Writer) (n int64, err error)
}

type ReaderAt interface {
	ReadAt(p []byte, off int64) (n int, err error)
}

// WriterAt 接口定义从指定偏移量开始读取数据
// 1.WriteAt 方法从 off 偏移俩个开始读取数据，返回读取的字节数和可能遇到的错误
type WriterAt interface {
	WriteAt(p []byte, off int64) (n int, err error)
}

// ByteReader 接口定义读取返回字节
// 1.ReadByte 方法从输入中读取并返回写一个字节，或者返回遇到的错误
type ByteReader interface {
	ReadByte() (byte, error)
}

// ByteScanner 接口将 UnreadByte 方法添加到 ByteReader 接口
// 1.UnreadByte 方法使得该方法前后两次的 ByteReader 调用返回相同字节
type ByteScanner interface {
	ByteReader
	UnreadByte() error
}

// ByteWriter 接口定义写字节方法
type ByteWriter interface {
	WriteByte(c byte) error
}

// ByteWriter 接口定义 RuneReader 方法
// RuneReader 方法读取单个 UFT-8 编码的 Unicode 字符并返回符文及其字节大小
type RuneReader interface {
	ReadRune() (r rune, size int, err error)
}

// RuneScanner 接口将 UnreadRune 方法添加到 RuneReader 接口
// 1.UnreadRune 方法使得该方法前后两次的 RuneReader 调用返回相同符文
type RuneScanner interface {
	RuneReader
	UnreadRune() error
}

// StringWriter 接口定义 WriteString 方法
type StringWriter interface {
	WriteString(s string) (n int, err error)
}

// 将字符串写入写入对象，返回写入字节大小和可能发生的错误
func WriteString(w Writer, s string) (n int, err error) {
	if sw, ok := w.(StringWriter); ok {
		return sw.WriteString(s)
	}
	return w.Write([]byte(s))
}

// ReadAtLeast 从 r 中至少读取 min 字节到 buf
func ReadAtLeast(r Reader, buf []byte, min int) (n int, err error) {
	if len(buf) < min {
		return 0, ErrShortBuffer
	}
	for n < min && err == nil {
		var nn int
		nn, err = r.Read(buf[n:])
		n += nn
	}
	if n >= min {
		err = nil
	} else if n > 0 && err == EOF {
		err = ErrUnexpectedEOF
	}
	return
}

// ReadAtLeast 从 r 中准确读取 len(buf) 字节到 buf
func ReadFull(r Reader, buf []byte) (n int, err error) {
	return ReadAtLeast(r, buf, len(buf))
}

// 从 src 复制 n 个字节到 dst
func CopyN(dst Writer, src Reader, n int64) (written int64, err error) {
	written, err = Copy(dst, LimitReader(src, n))
	if written == n {
		return n, nil
	}
	if written < n && err == nil {
		// src stopped early; must have been EOF.
		err = EOF
	}
	return
}

// 将副本从 src 复制掉 dst，直到在 src 上达到 EOF 或发生错误，返回复制的字节数
// 复制成功返回 err == nil, 而不是 err == EOF
func Copy(dst Writer, src Reader) (written int64, err error) {
	return copyBuffer(dst, src, nil)
}

// 提供缓冲区来进行复制，而不是分配一个缓冲区
func CopyBuffer(dst Writer, src Reader, buf []byte) (written int64, err error) {
	if buf != nil && len(buf) == 0 {
		panic("empty buffer in CopyBuffer")
	}
	return copyBuffer(dst, src, buf)
}

// 实际复制函数，优先使用 src 的 WriteTo 方法和 dst 的 ReadFrom 方法
func copyBuffer(dst Writer, src Reader, buf []byte) (written int64, err error) {
	// If the reader has a WriteTo method, use it to do the copy.
	// Avoids an allocation and a copy.
	if wt, ok := src.(WriterTo); ok {
		return wt.WriteTo(dst)
	}
	// Similarly, if the writer has a ReadFrom method, use it to do the copy.
	if rt, ok := dst.(ReaderFrom); ok {
		return rt.ReadFrom(src)
	}
	if buf == nil {
		size := 32 * 1024
		if l, ok := src.(*LimitedReader); ok && int64(size) > l.N {
			if l.N < 1 {
				size = 1
			} else {
				size = int(l.N)
			}
		}
		buf = make([]byte, size)
	}
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = errInvalidWrite
				}
			}
			written += int64(nw)
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != EOF {
				err = er
			}
			break
		}
	}
	return written, err
}

// 返回一个只读取 n 字节 Reader
func LimitReader(r Reader, n int64) Reader { return &LimitedReader{r, n} }

// 限制性读取 Reader
type LimitedReader struct {
	R Reader // underlying reader
	N int64  // max bytes remaining
}

func (l *LimitedReader) Read(p []byte) (n int, err error) {
	if l.N <= 0 {
		return 0, EOF
	}
	if int64(len(p)) > l.N {
		p = p[0:l.N]
	}
	n, err = l.R.Read(p)
	l.N -= int64(n)
	return
}

// 返回一个区间读取 Reader，从 off 开始读取 n 字节
func NewSectionReader(r ReaderAt, off int64, n int64) *SectionReader {
	return &SectionReader{r, off, off, off + n}
}

// 区间读取 Reader
type SectionReader struct {
	r     ReaderAt // 底层 ReaderAt
	base  int64    // 起始位置
	off   int64    // 偏移量
	limit int64    // 读取字节数限制
}

func (s *SectionReader) Read(p []byte) (n int, err error) {
	if s.off >= s.limit {
		return 0, EOF
	}
	if max := s.limit - s.off; int64(len(p)) > max {
		p = p[0:max]
	}
	n, err = s.r.ReadAt(p, s.off)
	s.off += int64(n)
	return
}

var errWhence = errors.New("Seek: invalid whence")
var errOffset = errors.New("Seek: invalid offset")

func (s *SectionReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	default:
		return 0, errWhence
	case SeekStart:
		offset += s.base
	case SeekCurrent:
		offset += s.off
	case SeekEnd:
		offset += s.limit
	}
	if offset < s.base {
		return 0, errOffset
	}
	s.off = offset
	return offset - s.base, nil
}

func (s *SectionReader) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= s.limit-s.base {
		return 0, EOF
	}
	off += s.base
	if max := s.limit - off; int64(len(p)) > max {
		p = p[0:max]
		n, err = s.r.ReadAt(p, off)
		if err == nil {
			err = EOF
		}
		return n, err
	}
	return s.r.ReadAt(p, off)
}

// Size returns the size of the section in bytes.
func (s *SectionReader) Size() int64 { return s.limit - s.base }

// 从指定输入中读取数据并写入指定输出，返回的 teeReader 没有内部缓冲，因此写入必须在读取完成之前完成
func TeeReader(r Reader, w Writer) Reader {
	return &teeReader{r, w}
}

type teeReader struct {
	r Reader
	w Writer
}

func (t *teeReader) Read(p []byte) (n int, err error) {
	n, err = t.r.Read(p)
	if n > 0 {
		if n, err := t.w.Write(p[:n]); err != nil {
			return n, err
		}
	}
	return
}

var Discard Writer = discard{}

type discard struct{}

// discard implements ReaderFrom as an optimization so Copy to
// io.Discard can avoid doing unnecessary work.
var _ ReaderFrom = discard{}

func (discard) Write(p []byte) (int, error) {
	return len(p), nil
}

func (discard) WriteString(s string) (int, error) {
	return len(s), nil
}

var blackHolePool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 8192)
		return &b
	},
}

func (discard) ReadFrom(r Reader) (n int64, err error) {
	bufp := blackHolePool.Get().(*[]byte)
	readSize := 0
	for {
		readSize, err = r.Read(*bufp)
		n += int64(readSize)
		if err != nil {
			blackHolePool.Put(bufp)
			if err == EOF {
				return n, nil
			}
			return
		}
	}
}

// 返回一个无操作 Close 方法的 ReadCloser
func NopCloser(r Reader) ReadCloser {
	return nopCloser{r}
}

type nopCloser struct {
	Reader
}

func (nopCloser) Close() error { return nil }

// 从 r 中读取数据，并返回读取的数据和可能遇到的错误
func ReadAll(r Reader) ([]byte, error) {
	b := make([]byte, 0, 512)
	for {
		if len(b) == cap(b) {
			// Add more capacity (let append pick how much).
			b = append(b, 0)[:len(b)]
		}
		n, err := r.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if err != nil {
			if err == EOF {
				err = nil
			}
			return b, err
		}
	}
}
