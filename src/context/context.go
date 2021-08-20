package context

import (
	"errors"
	"internal/reflectlite"
	"sync"
	"sync/atomic"
	"time"
)

/*
	1. go 语言中 context 用来设置截止日期、同步信号，传递请求相关的结构体，主要用于对信号进行同步以减少计算资源的浪费
	2. Context.Background() 用于创建初始 context
	3. context 对象提供四个方法：
			- Deadline: 返回取消时间
			- Done: 返回 一个channel，该 channel 在当前工作被完成或被取消后关闭
			- Err: 返回 context 结束原因，Done 返回的 channel 被关闭时，err 才为非 nil
			- Value: 返回 context 中存储的值，会递归向上查找
	3. 三个主要方法：
			- context.WithCancel() 返回 cancelCtx 结构体实例, 和取消函数
			- context.WithDeadline() 返回 timerCtx 结构体实例，, 和取消函数，timerCtx 在 cancelCtx 结构体上添加了计时器
			- context.WithTimeout() 返回 timerCtx 结构体实例，, 和取消函数，内部调用 WithDeadline 方法，与 WithDeadline 流程一样
	4. cancelCtx 将子 context 保存在一个 map 中，当自身被取消时，遍历调用子 context 的 cancel 方法
	5. context 实现取消原理：
			- Done 方法会返回一个空 channel，即 Done 方法会阻塞
			- 取消函数会 close 掉 Done 返回的 channel，当取消函数被执行时，Done 方法会取消阻塞，等同于收到信号，达到通信的目的，用户执行相关操作
			- context 是一个"以通信的方式来共享内存"的代表，通过并发原语 channel 来实现通信

*/
// Context 接口定义了四个方法
type Context interface {
	Deadline() (deadline time.Time, ok bool) // 返回 context 被取消的时间

	Done() <-chan struct{} // 返回一个 channel，该 channel 会在当前工作完成或者上下文被取消后关闭，多次调用返回相同的 Channel
	// 由于 Done 方法多次调用返回相同的 channel，因此可以在多处(多个 goroutine) 中调用，均会返回 channel

	Err() error // 返回 context 结束的原因，当 Done() 方法返回的 channel 被关闭时，该方法返回值才为非 nil

	Value(key interface{}) interface{} // 返回存储在 context 中的值，值不存在返回 nil，相同的 key 返回相同的值，该方法可以用来传递特定的数据
}

// Canceled is the error returned by Context.Err when the context is canceled.
var Canceled = errors.New("context canceled")

// DeadlineExceeded is the error returned by Context.Err when the context's
// deadline passes.
var DeadlineExceeded error = deadlineExceededError{}

type deadlineExceededError struct{}

func (deadlineExceededError) Error() string   { return "context deadline exceeded" }
func (deadlineExceededError) Timeout() bool   { return true }
func (deadlineExceededError) Temporary() bool { return true }

type emptyCtx int // 空 context，默认的 contxt

func (*emptyCtx) Deadline() (deadline time.Time, ok bool) {
	return
}

func (*emptyCtx) Done() <-chan struct{} {
	return nil
}

func (*emptyCtx) Err() error {
	return nil
}

func (*emptyCtx) Value(key interface{}) interface{} {
	return nil
}

func (e *emptyCtx) String() string {
	switch e {
	case background:
		return "context.Background"
	case todo:
		return "context.TODO"
	}
	return "unknown empty Context"
}

var (
	background = new(emptyCtx)
	todo       = new(emptyCtx)
)

func Background() Context { // 返回一个空 contxt，Background 方法通常用于创建一个顶级 context
	return background
}

// 返回一个空 contxt，Background 方法通常在不知道使用什么类型 context 时使用，TODO 与 Background() 返回的空 context 一样
func TODO() Context {
	return todo
}

// cancelFunc 函数用于放弃当前工作，能够被多次调用，只有首次生效，会中断进行中的工作
type CancelFunc func()

// 根据传入的 contxt 衍生出一个新的 context，同时返回一个用于取消该 contxt 的函数
// 执行该取消函数，当前 context 及其子 contxt 都会被取消，所有涉及到的 goroutine会被同时通知到
func WithCancel(parent Context) (ctx Context, cancel CancelFunc) {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	c := newCancelCtx(parent)
	propagateCancel(parent, &c)
	return &c, func() { c.cancel(true, Canceled) }
}

// newCancelCtx returns an initialized cancelCtx.
func newCancelCtx(parent Context) cancelCtx {
	return cancelCtx{Context: parent}
}

// goroutines counts the number of goroutines ever created; for testing.
var goroutines int32

// 当父 context 被取消时，通知子 context 取消进行中的操作
func propagateCancel(parent Context, child canceler) {
	done := parent.Done()
	if done == nil {
		return // 当父 context Done 方法为空时，不会触发取消事件，直接返回
	}

	select {
	case <-done: // 父 context 被取消，立即取消子 context
		child.cancel(false, parent.Err())
		return
	default:
	}
	// 父 context 当前没有被取消，将子 context 加入到父 context 的 children 列表中，
	// 当父 context 被取消后，从列表中取出子 context 进行取消
	if p, ok := parentCancelCtx(parent); ok {
		p.mu.Lock()
		if p.err != nil { // 父 context 被关闭，关闭子 context
			child.cancel(false, p.err)
		} else { // 父 context 未被关闭，将子 context 放入列表
			if p.children == nil {
				p.children = make(map[canceler]struct{})
			}
			p.children[child] = struct{}{}
		}
		p.mu.Unlock()
	} else {
		atomic.AddInt32(&goroutines, +1)
		go func() {
			select {
			case <-parent.Done():
				child.cancel(false, parent.Err())
			case <-child.Done():
			}
		}()
	}
}

// &cancelCtxKey is the key that a cancelCtx returns itself for.
var cancelCtxKey int

func parentCancelCtx(parent Context) (*cancelCtx, bool) {
	done := parent.Done()                  //
	if done == closedchan || done == nil { // 父 context 未被取消，done 表示不会取消事件，closedchan 表示 Done 返回的 channel 未被关闭，即还未调用父 contxt 的 cancel 方法
		return nil, false
	}
	p, ok := parent.Value(&cancelCtxKey).(*cancelCtx) // 当父 context 调用函数时 cancelCtxKey 会赋值，不为空，为空表示父 context 未被取消
	if !ok {
		return nil, false
	}
	pdone, _ := p.done.Load().(chan struct{})
	if pdone != done {
		return nil, false
	}
	return p, true
}

// removeChild removes a context from its parent.
func removeChild(parent Context, child canceler) {
	p, ok := parentCancelCtx(parent)
	if !ok {
		return
	}
	p.mu.Lock()
	if p.children != nil {
		delete(p.children, child)
	}
	p.mu.Unlock()
}

// A canceler is a context type that can be canceled directly. The
// implementations are *cancelCtx and *timerCtx.
type canceler interface {
	cancel(removeFromParent bool, err error)
	Done() <-chan struct{}
}

// closedchan is a reusable closed channel.
var closedchan = make(chan struct{})

func init() {
	close(closedchan)
}

// A cancelCtx can be canceled. When canceled, it also cancels any children
// that implement canceler.
type cancelCtx struct {
	Context

	mu       sync.Mutex            // protects following fields
	done     atomic.Value          // of chan struct{}, created lazily, closed by first cancel call
	children map[canceler]struct{} // set to nil by the first cancel call
	err      error                 // set to non-nil by the first cancel call
}

func (c *cancelCtx) Value(key interface{}) interface{} {
	if key == &cancelCtxKey {
		return c
	}
	return c.Context.Value(key)
}

func (c *cancelCtx) Done() <-chan struct{} { // 返回 channel
	d := c.done.Load()
	if d != nil {
		return d.(chan struct{})
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	d = c.done.Load()
	if d == nil {
		d = make(chan struct{})
		c.done.Store(d)
	}
	return d.(chan struct{})
}

func (c *cancelCtx) Err() error { // 返回 contxt 取消原因
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	return err
}

type stringer interface {
	String() string
}

func contextName(c Context) string {
	if s, ok := c.(stringer); ok {
		return s.String()
	}
	return reflectlite.TypeOf(c).String()
}

func (c *cancelCtx) String() string {
	return contextName(c.Context) + ".WithCancel"
}

// context 取消方法，1:关闭 c.done channel，取消子 context
func (c *cancelCtx) cancel(removeFromParent bool, err error) {
	if err == nil {
		panic("context: internal error: missing cancel error")
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return // already canceled
	}
	c.err = err
	d, _ := c.done.Load().(chan struct{})
	if d == nil {
		c.done.Store(closedchan)
	} else {
		close(d)
	}
	for child := range c.children {
		// NOTE: acquiring the child's lock while holding parent's lock.
		child.cancel(false, err)
	}
	c.children = nil
	c.mu.Unlock()

	if removeFromParent {
		removeChild(c.Context, c)
	}
}

// 根据传入的 contxt 和截至时间 衍生出一个新的 context，同时返回一个用于取消该 contxt 的函数
// 执行该取消函数，当前 context 及其子 contxt 都会被取消，所有涉及到的 goroutine会被同时通知到
// 当截止时间到了，会自动取消 context
func WithDeadline(parent Context, d time.Time) (Context, CancelFunc) {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	if cur, ok := parent.Deadline(); ok && cur.Before(d) {
		return WithCancel(parent) // 使用最近的截止时间
	}
	c := &timerCtx{
		cancelCtx: newCancelCtx(parent),
		deadline:  d,
	}
	propagateCancel(parent, c) // 判断父 context 是否被取消，将子 context 放入父列表
	dur := time.Until(d)
	if dur <= 0 { // 截止时间到
		c.cancel(true, DeadlineExceeded)
		return c, func() { c.cancel(false, Canceled) } //截止时间小于当前时间，只取消当前 context，不取消子 context
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil { // 父 context 未被取消
		c.timer = time.AfterFunc(dur, func() { // 创建定时器执行取消
			c.cancel(true, DeadlineExceeded)
		})
	}
	return c, func() { c.cancel(true, Canceled) } // 父 context 未被取消
}

// A timerCtx carries a timer and a deadline. It embeds a cancelCtx to
// implement Done and Err. It implements cancel by stopping its timer then
// delegating to cancelCtx.cancel.
type timerCtx struct {
	cancelCtx
	timer *time.Timer // Under cancelCtx.mu.

	deadline time.Time
}

func (c *timerCtx) Deadline() (deadline time.Time, ok bool) {
	return c.deadline, true
}

func (c *timerCtx) String() string {
	return contextName(c.cancelCtx.Context) + ".WithDeadline(" +
		c.deadline.String() + " [" +
		time.Until(c.deadline).String() + "])"
}

func (c *timerCtx) cancel(removeFromParent bool, err error) {
	c.cancelCtx.cancel(false, err)
	if removeFromParent {
		// Remove this timerCtx from its parent cancelCtx's children.
		removeChild(c.cancelCtx.Context, c)
	}
	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.mu.Unlock()
}

// 根据传入的 contxt 和截至时间 衍生出一个新的 context，同时返回一个用于取消该 contxt 的函数
// 执行该取消函数，当前 context 及其子 contxt 都会被取消，所有涉及到的 goroutine会被同时通知到
// 当截止时间到了，会自动取消 context
func WithTimeout(parent Context, timeout time.Duration) (Context, CancelFunc) {
	return WithDeadline(parent, time.Now().Add(timeout))
}

func WithValue(parent Context, key, val interface{}) Context {
	if parent == nil {
		panic("cannot create context from nil parent")
	}
	if key == nil {
		panic("nil key")
	}
	if !reflectlite.TypeOf(key).Comparable() {
		panic("key is not comparable")
	}
	return &valueCtx{parent, key, val}
}

// A valueCtx carries a key-value pair. It implements Value for that key and
// delegates all other calls to the embedded Context.
type valueCtx struct {
	Context
	key, val interface{}
}

// stringify tries a bit to stringify v, without using fmt, since we don't
// want context depending on the unicode tables. This is only used by
// *valueCtx.String().
func stringify(v interface{}) string {
	switch s := v.(type) {
	case stringer:
		return s.String()
	case string:
		return s
	}
	return "<not Stringer>"
}

func (c *valueCtx) String() string {
	return contextName(c.Context) + ".WithValue(type " +
		reflectlite.TypeOf(c.key).String() +
		", val " + stringify(c.val) + ")"
}

func (c *valueCtx) Value(key interface{}) interface{} {
	if c.key == key {
		return c.val
	}
	return c.Context.Value(key)
}
