package qrpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const (
	// DefaultMaxFrameSize is the max size for each request frame
	DefaultMaxFrameSize = 10 * 1024 * 1024
)

// A serveconn represents the server side of an qrpc connection.
// all fields (except untrack) are immutable, mutables are in ConnectionInfo
type serveconn struct {
	// server is the server on which the connection arrived.
	server *Server

	// cancelCtx cancels the connection-level context.
	cancelCtx context.CancelFunc
	// ctx is the corresponding context for cancelCtx
	ctx context.Context
	wg  sync.WaitGroup // wait group for goroutines

	idx int

	cs *ConnStreams

	// rwc is the underlying network connection.
	// This is never wrapped by other types and is the value given out
	// to CloseNotifier callers. It is usually of type *net.TCPConn
	rwc         net.Conn
	wlock       sync.Mutex
	bytesWriter *Writer

	reader      *defaultFrameReader  // used in conn.readFrames
	writer      FrameWriter          // used by handlers
	readFrameCh chan readFrameResult // written by conn.readFrames

	// modified by Server
	untrack     uint32 // ony the first call to untrack actually do it, subsequent calls should wait for untrackedCh
	untrackedCh chan struct{}
}

// ConnectionInfoKey is context key for ConnectionInfo
// used to store custom information
var ConnectionInfoKey = &contextKey{"qrpc-connection"}

// ConnectionInfo for store info on connection
type ConnectionInfo struct {
	l           sync.Mutex
	closed      bool
	id          string
	closeNotify []func()
	SC          *serveconn
	anything    interface{}
	respes      map[uint64]*response
}

// GetAnything returns anything
func (ci *ConnectionInfo) GetAnything() interface{} {
	ci.l.Lock()
	defer ci.l.Unlock()
	return ci.anything
}

// SetAnything sets anything
func (ci *ConnectionInfo) SetAnything(anything interface{}) {
	ci.l.Lock()
	ci.anything = anything
	ci.l.Unlock()
}

// GetID returns the ID
func (ci *ConnectionInfo) GetID() string {
	ci.l.Lock()
	defer ci.l.Unlock()
	return ci.id
}

// NotifyWhenClose ensures f is called when connection is closed
func (ci *ConnectionInfo) NotifyWhenClose(f func()) {
	ci.l.Lock()

	if ci.closed {
		ci.l.Unlock()
		f()
		return
	}

	ci.closeNotify = append(ci.closeNotify, f)
	ci.l.Unlock()
}

// Server returns the server
func (sc *serveconn) Server() *Server {
	return sc.server
}

// ReaderConfig for change timeout
type ReaderConfig interface {
	SetReadTimeout(timeout int)
}

// Reader returns the ReaderConfig
func (sc *serveconn) Reader() ReaderConfig {
	return sc.reader
}

func (sc *serveconn) RemoteAddr() string {
	return sc.rwc.RemoteAddr().String()
}

// Serve a new connection.
func (sc *serveconn) serve() {

	idx := sc.idx

	defer func() {
		// connection level panic
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			LogError("connection panic", sc.rwc.RemoteAddr().String(), err, buf)
		}
		sc.Close()
		sc.wg.Wait()
		sc.cs.Release()
	}()

	binding := sc.server.bindings[idx]
	var maxFrameSize int
	if binding.MaxFrameSize > 0 {
		maxFrameSize = binding.MaxFrameSize
	} else {
		maxFrameSize = DefaultMaxFrameSize
	}
	ctx := sc.ctx
	sc.reader = newFrameReaderWithMFS(ctx, sc.rwc, binding.DefaultReadTimeout, maxFrameSize)
	sc.writer = newFrameWriter(sc) // only used by blocking mode
	sc.bytesWriter = NewWriterWithTimeout(ctx, sc.rwc, binding.DefaultWriteTimeout)

	GoFunc(&sc.wg, func() {
		sc.readFrames()
	})

	handler := binding.Handler

	for {
		select {
		case <-ctx.Done():
			return
		case res := <-sc.readFrameCh:

			if res.readMore != nil {
				func() {
					defer sc.handleRequestPanic(res.f, time.Now())
					handler.ServeQRPC(sc.writer, res.f)
				}()
				res.readMore()
			} else {
				GoFunc(&sc.wg, func() {
					defer sc.handleRequestPanic(res.f, time.Now())
					handler.ServeQRPC(sc.GetWriter(), res.f)
				})
			}
		}

	}

}

func (sc *serveconn) instrument(frame *RequestFrame, begin time.Time, err interface{}) {
	binding := sc.server.bindings[sc.idx]

	if binding.CounterMetric == nil && binding.LatencyMetric == nil {
		return
	}

	errStr := fmt.Sprintf("%v", err)
	if binding.CounterMetric != nil {
		countlvs := []string{"method", strconv.Itoa(int(frame.Cmd)), "error", errStr}
		binding.CounterMetric.With(countlvs...).Add(1)
	}

	if binding.LatencyMetric == nil {
		return
	}

	lvs := []string{"method", strconv.Itoa(int(frame.Cmd)), "error", errStr}

	binding.LatencyMetric.With(lvs...).Observe(time.Since(begin).Seconds())

}

func (sc *serveconn) handleRequestPanic(frame *RequestFrame, begin time.Time) {
	err := recover()
	sc.instrument(frame, begin, err)

	if err != nil {

		const size = 64 << 10
		buf := make([]byte, size)
		buf = buf[:runtime.Stack(buf, false)]
		LogError("handleRequestPanic", sc.rwc.RemoteAddr().String(), err, string(buf))

	}

	s := frame.Stream
	if !s.IsSelfClosed() {
		// send error frame
		writer := sc.GetWriter()
		writer.StartWrite(frame.RequestID, 0, StreamRstFlag)
		err := writer.EndWrite()
		if err != nil {
			LogDebug("send error frame", err, sc.rwc.RemoteAddr().String(), frame)
		}
	}

}

// SetID sets id for serveconn
func (sc *serveconn) SetID(id string) (bool, uint64) {
	if id == "" {
		panic("empty id not allowed")
	}
	ci := sc.ctx.Value(ConnectionInfoKey).(*ConnectionInfo)
	ci.l.Lock()
	if ci.id != "" {
		ci.l.Unlock()
		panic("SetID called twice")
	}
	ci.id = id
	ci.l.Unlock()

	return sc.server.bindID(sc, id)
}

func (sc *serveconn) GetID() string {
	ci := sc.ctx.Value(ConnectionInfoKey).(*ConnectionInfo)
	return ci.GetID()
}

// GetWriter generate a FrameWriter for the connection
func (sc *serveconn) GetWriter() FrameWriter {

	return newFrameWriter(sc)
}

var (
	// ErrInvalidPacket when packet invalid
	ErrInvalidPacket = errors.New("invalid packet")
)

type readFrameResult struct {
	f *RequestFrame // valid until readMore is called

	// readMore should be called once the consumer no longer needs or
	// retains f. After readMore, f is invalid and more frames can be
	// read.
	readMore func()
}

// A gate lets two goroutines coordinate their activities.
type gate chan struct{}

const (
	errStrReadFramesForOverlayNetwork  = "readFrames err for OverlayNetwork"
	errStrWriteFramesForOverlayNetwork = "writeFrames err for OverlayNetwork"
)

func (g gate) Done() { g <- struct{}{} }

func (sc *serveconn) readFrames() (err error) {

	ci := sc.ctx.Value(ConnectionInfoKey).(*ConnectionInfo)

	ctx := sc.ctx
	binding := sc.server.bindings[sc.idx]
	if binding.ReadFrameChSize > 0 {
		runtime.LockOSThread()
	}

	defer func() {

		if err == ErrFrameTooLarge {
			LogError("ErrFrameTooLarge", "ip", sc.RemoteAddr())
		}

		if binding.CounterMetric != nil {
			errStr := fmt.Sprintf("%v", err)
			if err != nil {
				if binding.OverlayNetwork != nil {
					LogError("readFrames err", err, reflect.TypeOf(err))
					errStr = errStrReadFramesForOverlayNetwork
				}
			}

			countlvs := []string{"method", "readFrames", "error", errStr}
			binding.CounterMetric.With(countlvs...).Add(1)
		}
		if binding.ReadFrameChSize > 0 {
			runtime.UnlockOSThread()
		}
	}()
	gate := make(gate, 1)
	gateDone := gate.Done

	for {
		req, err := sc.reader.ReadFrame(sc.cs)
		if err != nil {
			sc.Close()
			sc.reader.Finalize()
			if opErr, ok := err.(*net.OpError); ok {
				return opErr.Err
			}
			return err
		}
		if req.FromServer() {
			ci.l.Lock()
			if ci.respes != nil {
				resp, ok := ci.respes[req.RequestID]
				if ok {
					delete(ci.respes, req.RequestID)
				}
				ci.l.Unlock()
				if ok {
					resp.SetResponse(req)
					continue
				}
			} else {
				ci.l.Unlock()
			}
		}

		if req.Flags.IsNonBlock() {
			select {
			case sc.readFrameCh <- readFrameResult{f: (*RequestFrame)(req)}:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		} else {
			select {
			case sc.readFrameCh <- readFrameResult{f: (*RequestFrame)(req), readMore: gateDone}:
			case <-ctx.Done():
				return ctx.Err()
			}

			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		sc.server.waitThrottle(sc.idx, ctx.Done())
	}

}

func (sc *serveconn) writeFrame(dfw *defaultFrameWriter) (err error) {
	select {
	case <-sc.ctx.Done():
		return sc.ctx.Err()
	default:
	}

	sc.wlock.Lock()

	defer sc.wlock.Unlock()

	flags := dfw.Flags()
	requestID := dfw.RequestID()

	if flags.IsRst() {
		s := sc.cs.GetStream(requestID, flags)
		if s == nil {
			err = ErrRstNonExistingStream
			return
		}
		// for rst frame, AddOutFrame returns false when no need to send the frame
		if !s.AddOutFrame(requestID, flags) {
			return
		}
	} else if !flags.IsPush() { // skip stream logic if PushFlag set
		s, loaded := sc.cs.CreateOrGetStream(sc.ctx, requestID, flags)
		if !loaded {
			LogDebug(unsafe.Pointer(sc.cs), "serveconn new stream", requestID, flags, dfw.Cmd())
		}
		if !s.AddOutFrame(requestID, flags) {
			err = ErrWriteAfterCloseSelf
			return
		}
	}

	_, err = sc.bytesWriter.Write(dfw.GetWbuf())
	if err != nil {
		LogDebug(unsafe.Pointer(sc), "serveconn Write", err)
		sc.Close()

		if opErr, ok := err.(*net.OpError); ok {
			err = opErr.Err
		}

		binding := sc.server.bindings[sc.idx]
		if binding.CounterMetric != nil {
			errStr := fmt.Sprintf("%v", err)
			if err != nil && binding.OverlayNetwork != nil {
				errStr = errStrWriteFramesForOverlayNetwork
			}
			countlvs := []string{"method", "writeFrame", "error", errStr}
			binding.CounterMetric.With(countlvs...).Add(1)
		}
		return
	}

	return
}

// Request clientconn from serveconn
func (sc *serveconn) Request(cmd Cmd, flags FrameFlag, payload []byte) (uint64, Response, error) {
	flags = flags | NBFlag
	requestID, resp, _, err := sc.writeFirstFrame(cmd, flags, payload)

	return requestID, resp, err
}

func (sc *serveconn) writeFirstFrame(cmd Cmd, flags FrameFlag, payload []byte) (uint64, Response, *defaultFrameWriter, error) {
	var (
		requestID uint64
		suc       bool
	)

	if atomic.LoadUint32(&sc.untrack) != 0 {
		return 0, nil, nil, ErrConnAlreadyClosed
	}

	requestID = PoorManUUID(false)
	ci := sc.ctx.Value(ConnectionInfoKey).(*ConnectionInfo)
	ci.l.Lock()
	if ci.respes == nil {
		ci.respes = make(map[uint64]*response)
	}
	i := 0
	for {
		_, ok := ci.respes[requestID]
		if !ok {
			suc = true
			break
		}

		i++
		if i >= 3 {
			break
		}
		requestID = PoorManUUID(false)
	}

	if !suc {
		ci.l.Unlock()
		return 0, nil, nil, ErrNoNewUUID
	}
	resp := &response{Frame: make(chan *Frame, 1)}
	ci.respes[requestID] = resp
	ci.l.Unlock()

	writer := newFrameWriter(sc)
	writer.StartWrite(requestID, cmd, flags)
	writer.WriteBytes(payload)
	err := writer.EndWrite()

	if err != nil {
		ci.l.Lock()
		resp, ok := ci.respes[requestID]
		if ok {
			resp.Close()
			delete(ci.respes, requestID)
		}
		ci.l.Unlock()
		return 0, nil, nil, err
	}

	return requestID, resp, writer, nil
}

// Close the connection.
func (sc *serveconn) Close() error {

	if limiter := sc.server.closeRateLimiter[sc.idx]; limiter != nil {
		limiter.Take()
	}

	ok, ch := sc.server.untrack(sc, false)
	if !ok {
		<-ch
	}
	return sc.closeUntracked()

}

func (sc *serveconn) closeUntracked() error {

	err := sc.rwc.Close()
	if err != nil {
		return err
	}
	sc.cancelCtx()

	ci := sc.ctx.Value(ConnectionInfoKey).(*ConnectionInfo)
	ci.l.Lock()
	ci.closed = true
	closeNotify := ci.closeNotify
	ci.closeNotify = nil
	ci.l.Unlock()

	for _, f := range closeNotify {
		f()
	}

	return nil
}
