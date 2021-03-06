package queue

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/stephane-martin/skewer/utils"
)

type intNode struct {
	next *intNode
	uid  int32
}

type IntQueue struct {
	_padding0 [8]uint64
	head      *intNode
	_padding1 [8]uint64
	tail      *intNode
	_padding2 [8]uint64
	disposed  int32
	_padding3 [8]uint64
	pool      *sync.Pool
}

func NewIntQueue() *IntQueue {
	stub := &intNode{}
	q := &IntQueue{head: stub, tail: stub, disposed: 0, pool: &sync.Pool{New: func() interface{} {
		return &intNode{}
	}}}
	return q
}

func (q *IntQueue) Disposed() bool {
	return atomic.LoadInt32(&q.disposed) == 1
}

func (q *IntQueue) Dispose() {
	atomic.StoreInt32(&q.disposed, 1)
}

func (q *IntQueue) Get() (int32, error) {
	if q.Disposed() {
		return -1, utils.ErrDisposed
	}
	tail := q.tail
	next := tail.next
	if next != nil {
		(*intNode)(atomic.SwapPointer((*unsafe.Pointer)(unsafe.Pointer(&q.tail)), unsafe.Pointer(next))).uid = next.uid
		q.pool.Put(tail)
		return next.uid, nil
	}
	return -1, nil
}

func (q *IntQueue) Peek() (int32, error) {
	if q.Disposed() {
		return -1, utils.ErrDisposed
	}
	next := q.tail.next
	if next != nil {
		return next.uid, nil
	}
	return -1, nil
}

func (q *IntQueue) Put(uid int32) error {
	if q.Disposed() {
		return utils.ErrDisposed
	}
	n := q.pool.Get().(*intNode)
	n.uid = uid
	n.next = nil
	(*intNode)(atomic.SwapPointer((*unsafe.Pointer)(unsafe.Pointer(&q.head)), unsafe.Pointer(n))).next = n
	return nil
}

func (q *IntQueue) Has() bool {
	return q.tail.next != nil
}

func (q *IntQueue) Wait() bool {
	var w utils.ExpWait
	for {
		if q.Has() {
			return true
		}
		if q.Disposed() {
			return false
		}
		w.Wait()
	}
}

func WaitOne(q1 *IntQueue, q2 *IntQueue) bool {
	var nb uint64
	for {
		if q1.Disposed() || q2.Disposed() {
			return false
		}
		if q1.Has() || q2.Has() {
			return true
		}
		if nb < 22 {
			runtime.Gosched()
		} else if nb < 24 {
			time.Sleep(time.Millisecond)
		} else if nb < 26 {
			time.Sleep(10 * time.Millisecond)
		} else {
			time.Sleep(100 * time.Millisecond)
		}
		nb++
	}
}
