package queue

import (
	"runtime"
	"sync/atomic"
	"time"

	"github.com/stephane-martin/skewer/model"
)

type rawtcpnode struct {
	position uint64
	data     *model.RawTcpMessage
}

type rawtcpnodes []*rawtcpnode

// RawTCPRing is a MPMC buffer that achieves threadsafety with CAS operations
// only.  A put on full or get on empty call will block until an item
// is put or retrieved.  Calling Dispose on the RawTCPRing will unblock
// any blocked threads with an error.  This buffer is similar to the buffer
// described here: http://www.1024cores.net/home/lock-free-algorithms/queues/bounded-mpmc-queue
// with some minor additions.
type RawTCPRing struct {
	_padding0      [8]uint64
	queue          uint64
	_padding1      [8]uint64
	dequeue        uint64
	_padding2      [8]uint64
	mask, disposed uint64
	_padding3      [8]uint64
	nodes          rawtcpnodes
}

func (rb *RawTCPRing) init(size uint64) {
	size = roundUp(size)
	rb.nodes = make(rawtcpnodes, size)
	for i := uint64(0); i < size; i++ {
		rb.nodes[i] = &rawtcpnode{position: i}
	}
	rb.mask = size - 1 // so we don't have to do this with every put/get operation
}

// Put adds the provided item to the queue.  If the queue is full, this
// call will block until an item is added to the queue or Dispose is called
// on the queue.  An error will be returned if the queue is disposed.
func (rb *RawTCPRing) Put(item *model.RawTcpMessage) error {
	_, err := rb.put(item, false)
	return err
}

// Offer adds the provided item to the queue if there is space.  If the queue
// is full, this call will return false.  An error will be returned if the
// queue is disposed.
func (rb *RawTCPRing) Offer(item *model.RawTcpMessage) (bool, error) {
	return rb.put(item, true)
}

func (rb *RawTCPRing) put(item *model.RawTcpMessage, offer bool) (bool, error) {
	var n *rawtcpnode
	var nb uint64
	pos := atomic.LoadUint64(&rb.queue)
L:
	for {
		if atomic.LoadUint64(&rb.disposed) == 1 {
			return false, ErrDisposed
		}

		n = rb.nodes[pos&rb.mask]
		seq := atomic.LoadUint64(&n.position)
		switch dif := seq - pos; {
		case dif == 0:
			if atomic.CompareAndSwapUint64(&rb.queue, pos, pos+1) {
				break L
			}
		case dif < 0:
			panic(`Ring buffer in a compromised state during a put operation.`)
		default:
			pos = atomic.LoadUint64(&rb.queue)
		}

		if offer {
			return false, nil
		}

		if nb < 22 {
			runtime.Gosched()
		} else if nb < 24 {
			time.Sleep(1000000)
		} else if nb < 26 {
			time.Sleep(10000000)
		} else {
			time.Sleep(100000000)
		}
		nb++
	}

	n.data = item
	atomic.StoreUint64(&n.position, pos+1)
	return true, nil
}

// Get will return the next item in the queue.  This call will block
// if the queue is empty.  This call will unblock when an item is added
// to the queue or Dispose is called on the queue.  An error will be returned
// if the queue is disposed.
func (rb *RawTCPRing) Get() (*model.RawTcpMessage, error) {
	return rb.Poll(0)
}

// Poll will return the next item in the queue.  This call will block
// if the queue is empty.  This call will unblock when an item is added
// to the queue, Dispose is called on the queue, or the timeout is reached. An
// error will be returned if the queue is disposed or a timeout occurs. A
// non-positive timeout will block indefinitely.
func (rb *RawTCPRing) Poll(timeout time.Duration) (*model.RawTcpMessage, error) {
	var (
		n     *rawtcpnode
		pos   = atomic.LoadUint64(&rb.dequeue)
		start time.Time
		nb    uint64
	)
	if timeout > 0 {
		start = time.Now()
	}
L:
	for {
		n = rb.nodes[pos&rb.mask]
		seq := atomic.LoadUint64(&n.position)
		switch dif := seq - (pos + 1); {
		case dif == 0:
			if atomic.CompareAndSwapUint64(&rb.dequeue, pos, pos+1) {
				break L
			}
		case dif < 0:
			panic(`Ring buffer in compromised state during a get operation.`)
		default:
			pos = atomic.LoadUint64(&rb.dequeue)
		}

		if timeout > 0 && time.Since(start) >= timeout {
			return nil, ErrTimeout
		}
		if atomic.LoadUint64(&rb.disposed) == 1 {
			return nil, ErrDisposed
		}

		if nb < 22 {
			runtime.Gosched()
		} else if nb < 24 {
			time.Sleep(1000000)
		} else if nb < 26 {
			time.Sleep(10000000)
		} else {
			time.Sleep(100000000)
		}
		nb++
	}
	data := n.data
	n.data = nil
	atomic.StoreUint64(&n.position, pos+rb.mask+1)
	return data, nil
}

// Len returns the number of items in the queue.
func (rb *RawTCPRing) Len() uint64 {
	return atomic.LoadUint64(&rb.queue) - atomic.LoadUint64(&rb.dequeue)
}

// Cap returns the capacity of this ring buffer.
func (rb *RawTCPRing) Cap() uint64 {
	return uint64(len(rb.nodes))
}

// Dispose will dispose of this queue and free any blocked threads
// in the Put and/or Get methods.  Calling those methods on a disposed
// queue will return an error.
func (rb *RawTCPRing) Dispose() {
	atomic.CompareAndSwapUint64(&rb.disposed, 0, 1)
}

// IsDisposed will return a bool indicating if this queue has been
// disposed.
func (rb *RawTCPRing) IsDisposed() bool {
	return atomic.LoadUint64(&rb.disposed) == 1
}

// NewRingBuffer will allocate, initialize, and return a ring buffer
// with the specified size.
func NewRawTCPRing(size uint64) *RawTCPRing {
	rb := &RawTCPRing{}
	rb.init(size)
	return rb
}