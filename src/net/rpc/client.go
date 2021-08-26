// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rpc

import (
	"bufio"
	"encoding/gob"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
)

// ServerError represents an error that has been returned from
// the remote side of the RPC connection.
type ServerError string

func (e ServerError) Error() string {
	return string(e)
}

var ErrShutdown = errors.New("connection is shut down")

// Call 结构体表示一个活动状态的 RPC
type Call struct {
	ServiceMethod string      // 调用的服务端方法名
	Args          interface{} // 请求参数
	Reply         interface{} // 响应参数
	Error         error       // 调用是否出错
	Done          chan *Call  // Receives *Call when Go is complete.
}

// Client 表示一个 RPC 客户端，一个客户端可能有多个未完成的调用，切可能同时被多个 goroutine 使用
type Client struct {
	codec ClientCodec

	reqMutex sync.Mutex // 防止并发发送 rpc 数据
	request  Request

	mutex    sync.Mutex // 防止并发生成发送序列号
	seq      uint64
	pending  map[uint64]*Call // 存储发送中的 rpc 请求，key 为 rpc 发送序列号，是一个递增的数字
	closing  bool             // Close 方法是否被调用
	shutdown bool             // 服务端是否停止
}

// 表示 rpc client 读写数据
type ClientCodec interface {
	WriteRequest(*Request, interface{}) error
	ReadResponseHeader(*Response) error
	ReadResponseBody(interface{}) error

	Close() error
}

// rpc 请求发送函数
//	1.注册发送的 rpc
//	2.生成发送头部
//	3.发送请求
//	4.校验结果
func (client *Client) send(call *Call) {
	client.reqMutex.Lock() // 加锁，防止并发发送，造成数据乱序，解析失败
	defer client.reqMutex.Unlock()

	// Register this call.
	client.mutex.Lock()
	if client.shutdown || client.closing {
		client.mutex.Unlock()
		call.Error = ErrShutdown
		call.done()
		return
	}
	seq := client.seq // 1.获取本次 rpc 请求发送序列号
	client.seq++
	client.pending[seq] = call
	client.mutex.Unlock()

	// Encode and send the request.
	client.request.Seq = seq
	client.request.ServiceMethod = call.ServiceMethod
	err := client.codec.WriteRequest(&client.request, call.Args) // 2.3.使用指定编码方式编码发送数据，默认 codec 为 gobClientCodec
	if err != nil {
		client.mutex.Lock()
		call = client.pending[seq]
		delete(client.pending, seq)
		client.mutex.Unlock()
		if call != nil {
			call.Error = err // 调用错误，将错误封装进当前 rpc 对象
			call.done()
		}
	}
}

func (client *Client) input() {
	var err error
	var response Response
	for err == nil {
		response = Response{}
		err = client.codec.ReadResponseHeader(&response) // 读取响应头
		if err != nil {
			break
		}
		seq := response.Seq
		client.mutex.Lock() // 根据响应序列号获取 rpc 请求，同时将此次请求从进行中 map 删除
		call := client.pending[seq]
		delete(client.pending, seq)
		client.mutex.Unlock()

		switch {
		case call == nil:
			// 根据请求序列号查找不到 rpc 请求，意味着此次请求发送部分失败，并且被删除
			err = client.codec.ReadResponseBody(nil)
			if err != nil {
				err = errors.New("reading error body: " + err.Error())
			}
		case response.Error != "": // 请求错误，服务端返回错误
			call.Error = ServerError(response.Error)
			err = client.codec.ReadResponseBody(nil)
			if err != nil {
				err = errors.New("reading error body: " + err.Error())
			}
			call.done() // 调用 rpc 请求的 done 方法，发送请求结束响应
		default: // 请求正常
			err = client.codec.ReadResponseBody(call.Reply)
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			call.done() // 调用 rpc 请求的 done 方法，发送请求结束响应
		}
	}
	// 代码运行到此处，表示读取响应操作出错，中断发送中的 rpc，关闭服务
	client.reqMutex.Lock()
	client.mutex.Lock()
	client.shutdown = true
	closing := client.closing
	if err == io.EOF {
		if closing {
			err = ErrShutdown
		} else {
			err = io.ErrUnexpectedEOF
		}
	}
	for _, call := range client.pending {
		call.Error = err
		call.done()
	}
	client.mutex.Unlock()
	client.reqMutex.Unlock()
	if debugLog && err != io.EOF && !closing {
		log.Println("rpc: client protocol error:", err)
	}
}

func (call *Call) done() {
	select {
	case call.Done <- call:
		// ok
	default:
		// We don't want to block here. It is the caller's responsibility to make
		// sure the channel has enough buffer space. See comment in Go().
		if debugLog {
			log.Println("rpc: discarding Call reply due to insufficient Done chan capacity")
		}
	}
}

func NewClient(conn io.ReadWriteCloser) *Client {
	encBuf := bufio.NewWriter(conn)
	client := &gobClientCodec{conn, gob.NewDecoder(conn), gob.NewEncoder(encBuf), encBuf}
	return NewClientWithCodec(client)
}

// NewClientWithCodec is like NewClient but uses the specified
// codec to encode requests and decode responses.
func NewClientWithCodec(codec ClientCodec) *Client {
	client := &Client{
		codec:   codec,
		pending: make(map[uint64]*Call),
	}
	go client.input()
	return client
}

type gobClientCodec struct {
	rwc    io.ReadWriteCloser
	dec    *gob.Decoder
	enc    *gob.Encoder
	encBuf *bufio.Writer
}

// 发送数据，发送的数据分为两部分，头部 + 请求体，头部为: 请求方法名+请求序列号, 请求体为: 请求参数
func (c *gobClientCodec) WriteRequest(r *Request, body interface{}) (err error) {
	if err = c.enc.Encode(r); err != nil { // 编码请求头
		return
	}
	if err = c.enc.Encode(body); err != nil { // 编码请求体
		return
	}
	return c.encBuf.Flush() // 发送数据
}

func (c *gobClientCodec) ReadResponseHeader(r *Response) error {
	return c.dec.Decode(r)
}

func (c *gobClientCodec) ReadResponseBody(body interface{}) error {
	return c.dec.Decode(body)
}

func (c *gobClientCodec) Close() error {
	return c.rwc.Close()
}

func DialHTTP(network, address string) (*Client, error) {
	return DialHTTPPath(network, address, DefaultRPCPath)
}

func DialHTTPPath(network, address, path string) (*Client, error) {
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	io.WriteString(conn, "CONNECT "+path+" HTTP/1.0\n\n")

	// Require successful HTTP response
	// before switching to RPC protocol.
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err == nil && resp.Status == connected {
		return NewClient(conn), nil
	}
	if err == nil {
		err = errors.New("unexpected HTTP response: " + resp.Status)
	}
	conn.Close()
	return nil, &net.OpError{
		Op:   "dial-http",
		Net:  network + " " + address,
		Addr: nil,
		Err:  err,
	}
}

// Dial connects to an RPC server at the specified network address.
func Dial(network, address string) (*Client, error) {
	conn, err := net.Dial(network, address)
	if err != nil {
		return nil, err
	}
	return NewClient(conn), nil
}

// 关闭连接
func (client *Client) Close() error {
	client.mutex.Lock()
	if client.closing {
		client.mutex.Unlock()
		return ErrShutdown
	}
	client.closing = true
	client.mutex.Unlock()
	return client.codec.Close()
}

// Go 方法是一个异步调用方法
// 1.根据请求参数封装一个 rpc 请求 Call
// 2.检查 done 参数
//		- 如果 done 为空，创建缓冲长度为 10 的 *Call 通道
//		- done 不为空，但缓冲长度为 0，抛出错误，该方法会将创建的本次 rpc 请求方法 done 通道，当请求结束时，
//      调用 rpc 请求的 Done 方法取出 rpc 请求，检查结果，当缓冲长度为 0 时，无法缓存本次 rpc 请求(*Call)，抛出错误
// 3.调用 client 的 send 方法发送请求
func (client *Client) Go(serviceMethod string, args interface{}, reply interface{}, done chan *Call) *Call {
	call := new(Call)
	call.ServiceMethod = serviceMethod // 方法
	call.Args = args                   // 参数
	call.Reply = reply                 // 响应
	if done == nil {
		done = make(chan *Call, 10) // buffered.
	} else {
		if cap(done) == 0 {
			log.Panic("rpc: done channel is unbuffered")
		}
	}
	call.Done = done
	client.send(call)
	return call
}

// Call 方法是一个同步调用方法，内部调用了 Go 方法，创建缓冲通道长度为 1 的 *Call 通道
func (client *Client) Call(serviceMethod string, args interface{}, reply interface{}) error {
	call := <-client.Go(serviceMethod, args, reply, make(chan *Call, 1)).Done
	return call.Error
}
