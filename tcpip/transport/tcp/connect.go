// Copyright 2016 The Netstack Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tcp

import (
	"crypto/rand"
	"time"

	"github.com/google/netstack/sleep"
	"github.com/google/netstack/tcpip"
	"github.com/google/netstack/tcpip/buffer"
	"github.com/google/netstack/tcpip/header"
	"github.com/google/netstack/tcpip/seqnum"
	"github.com/google/netstack/tcpip/stack"
	"github.com/google/netstack/waiter"
)

// maxSegmentsPerWake is the maximum number of segments to process in the main
// protocol goroutine per wake-up. Yielding [after this number of segments are
// processed] allows other events to be processed as well (e.g., timeouts,
// resets, etc.).
const maxSegmentsPerWake = 100

type handshakeState int

// The following are the possible states of the TCP connection during a 3-way
// handshake. A depiction of the states and transitions can be found in RFC 793,
// page 23.
const (
	handshakeSynSent handshakeState = iota
	handshakeSynRcvd
	handshakeCompleted
)

// The following are used to set up sleepers.
const (
	wakerForNotification = iota
	wakerForNewSegment
	wakerForResend
)

const (
	// maxWndScale is maximum allowed window scaling, as described in
	// RFC 1323, section 2.3, page 11.
	maxWndScale = 14
)

// handshake holds the state used during a TCP 3-way handshake.
type handshake struct {
	ep     *endpoint
	state  handshakeState
	active bool
	flags  uint8
	ackNum seqnum.Value

	// iss is the initial send sequence number, as defined in RFC 793.
	iss seqnum.Value

	// rcvWnd is the receive window, as defined in RFC 793.
	rcvWnd seqnum.Size

	// sndWnd is the send window, as defined in RFC 793.
	sndWnd seqnum.Size

	// mss is the maximum segment size received from the peer.
	mss uint16

	// sndWndScale is the send window scale, as defined in RFC 1323. A
	// negative value means no scaling is supported by the peer.
	sndWndScale int

	// rcvWndScale is the receive window scale, as defined in RFC 1323.
	rcvWndScale int
}

func newHandshake(ep *endpoint, rcvWnd seqnum.Size) (handshake, error) {
	h := handshake{
		ep:          ep,
		active:      true,
		rcvWnd:      rcvWnd,
		rcvWndScale: findWndScale(rcvWnd),
	}
	if err := h.resetState(); err != nil {
		return handshake{}, err
	}

	return h, nil
}

// findWndScale determines the window scale to use for the given maximum window
// size.
func findWndScale(wnd seqnum.Size) int {
	if wnd < 0x10000 {
		return 0
	}

	max := seqnum.Size(0xffff)
	s := 0
	for wnd > max && s < maxWndScale {
		s++
		max <<= 1
	}

	return s
}

// resetState resets the state of the handshake object such that it becomes
// ready for a new 3-way handshake.
func (h *handshake) resetState() error {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return err
	}

	h.state = handshakeSynSent
	h.flags = flagSyn
	h.ackNum = 0
	h.mss = 0
	h.iss = seqnum.Value(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24)

	return nil
}

// effectiveRcvWndScale returns the effective receive window scale to be used.
// If the peer doesn't support window scaling, the effective rcv wnd scale is
// zero; otherwise it's the value calculated based on the initial rcv wnd.
func (h *handshake) effectiveRcvWndScale() uint8 {
	if h.sndWndScale < 0 {
		return 0
	}
	return uint8(h.rcvWndScale)
}

// resetToSynRcvd resets the state of the handshake object to the SYN-RCVD
// state.
func (h *handshake) resetToSynRcvd(iss seqnum.Value, irs seqnum.Value, mss uint16, sndWndScale int) {
	h.active = false
	h.state = handshakeSynRcvd
	h.flags = flagSyn | flagAck
	h.iss = iss
	h.ackNum = irs + 1
	h.mss = mss
	h.sndWndScale = sndWndScale
}

// checkAck checks if the ACK number, if present, of a segment received during
// a TCP 3-way handshake is valid. If it's not, a RST segment is sent back in
// response.
func (h *handshake) checkAck(s *segment) bool {
	if s.flagIsSet(flagAck) && s.ackNumber != h.iss+1 {
		// RFC 793, page 36, states that a reset must be generated when
		// the connection is in any non-synchronized state and an
		// incoming segment acknowledges something not yet sent. The
		// connection remains in the same state.
		ack := s.sequenceNumber.Add(s.logicalLen())
		h.ep.sendRaw(nil, flagRst|flagAck, s.ackNumber, ack, 0)
		return false
	}

	return true
}

// synSentState handles a segment received when the TCP 3-way handshake is in
// the SYN-SENT state.
func (h *handshake) synSentState(s *segment) error {
	// RFC 793, page 37, states that in the SYN-SENT state, a reset is
	// acceptable if the ack field acknowledges the SYN.
	if s.flagIsSet(flagRst) {
		if s.flagIsSet(flagAck) && s.ackNumber == h.iss+1 {
			return tcpip.ErrConnectionRefused
		}
		return nil
	}

	if !h.checkAck(s) {
		return nil
	}

	// We are in the SYN-SENT state. We only care about segments that have
	// the SYN flag.
	if !s.flagIsSet(flagSyn) {
		return nil
	}

	// Parse the SYN options. Ignore the segment if it's invalid.
	mss, sws, ok := parseSynOptions(s)
	if !ok {
		return nil
	}

	// Remember the sequence we'll ack from now on.
	h.ackNum = s.sequenceNumber + 1
	h.flags |= flagAck
	h.mss = mss
	h.sndWndScale = sws

	// If this is a SYN ACK response, we only need to acknowledge the SYN
	// and the handshake is completed.
	if s.flagIsSet(flagAck) {
		h.state = handshakeCompleted
		h.ep.sendRaw(nil, flagAck, h.iss+1, h.ackNum, h.rcvWnd>>h.effectiveRcvWndScale())
		return nil
	}

	// A SYN segment was received, but no ACK in it. We acknowledge the SYN
	// but resend our own SYN and wait for it to be acknowledged in the
	// SYN-RCVD state.
	h.state = handshakeSynRcvd
	sendSynTCP(&s.route, h.ep.id, h.flags, h.iss, h.ackNum, h.rcvWnd, h.rcvWndScale)

	return nil
}

// synRcvdState handles a segment received when the TCP 3-way handshake is in
// the SYN-RCVD state.
func (h *handshake) synRcvdState(s *segment) error {
	if s.flagIsSet(flagRst) {
		// RFC 793, page 37, states that in the SYN-RCVD state, a reset
		// is acceptable if the sequence number is in the window.
		if s.sequenceNumber.InWindow(h.ackNum, h.rcvWnd) {
			return tcpip.ErrConnectionRefused
		}
		return nil
	}

	if !h.checkAck(s) {
		return nil
	}

	if s.flagIsSet(flagSyn) && s.sequenceNumber != h.ackNum-1 {
		// We received two SYN segments with different sequence
		// numbers, so we reset this and restart the whole
		// process, except that we don't reset the timer.
		ack := s.sequenceNumber.Add(s.logicalLen())
		seq := seqnum.Value(0)
		if s.flagIsSet(flagAck) {
			seq = s.ackNumber
		}
		h.ep.sendRaw(nil, flagRst|flagAck, seq, ack, 0)

		if !h.active {
			return tcpip.ErrInvalidEndpointState
		}

		if err := h.resetState(); err != nil {
			return err
		}

		sendSynTCP(&s.route, h.ep.id, h.flags, h.iss, h.ackNum, h.rcvWnd, h.rcvWndScale)
		return nil
	}

	// We have previously received (and acknowledged) the peer's SYN. If the
	// peer acknowledges our SYN, the handshake is completed.
	if s.flagIsSet(flagAck) {
		h.state = handshakeCompleted
		return nil
	}

	return nil
}

// processSegments goes through the segment queue and processes up to
// maxSegmentsPerWake (if they're available).
func (h *handshake) processSegments() error {
	for i := 0; i < maxSegmentsPerWake; i++ {
		s := h.ep.segmentQueue.dequeue()
		if s == nil {
			return nil
		}

		h.sndWnd = s.window
		if !s.flagIsSet(flagSyn) && h.sndWndScale > 0 {
			h.sndWnd <<= uint8(h.sndWndScale)
		}

		var err error
		switch h.state {
		case handshakeSynRcvd:
			err = h.synRcvdState(s)
		case handshakeSynSent:
			err = h.synSentState(s)
		}
		s.decRef()
		if err != nil {
			return err
		}

		// We stop processing packets once the handshake is completed,
		// otherwise we may process packets meant to be processed by
		// the main protocol goroutine.
		if h.state == handshakeCompleted {
			break
		}
	}

	// If the queue is not empty, make sure we'll wake up in the next
	// iteration.
	if !h.ep.segmentQueue.empty() {
		h.ep.newSegmentWaker.Assert()
	}

	return nil
}

// execute executes the TCP 3-way handshake.
func (h *handshake) execute() error {
	// Initialize the resend timer.
	resendWaker := sleep.Waker{}
	timeOut := time.Duration(time.Second)
	rt := time.AfterFunc(timeOut, func() {
		resendWaker.Assert()
	})
	defer rt.Stop()

	// Set up the wakers.
	s := sleep.Sleeper{}
	s.AddWaker(&resendWaker, wakerForResend)
	s.AddWaker(&h.ep.notificationWaker, wakerForNotification)
	s.AddWaker(&h.ep.newSegmentWaker, wakerForNewSegment)
	defer s.Done()

	// Send the initial SYN segment and loop until the handshake is
	// completed.
	sendSynTCP(&h.ep.route, h.ep.id, h.flags, h.iss, h.ackNum, h.rcvWnd, h.rcvWndScale)
	for h.state != handshakeCompleted {
		switch index, _ := s.Fetch(true); index {
		case wakerForResend:
			timeOut *= 2
			if timeOut > 60*time.Second {
				return tcpip.ErrTimeout
			}
			rt.Reset(timeOut)
			sendSynTCP(&h.ep.route, h.ep.id, h.flags, h.iss, h.ackNum, h.rcvWnd, h.rcvWndScale)

		case wakerForNotification:
			n := h.ep.fetchNotifications()
			if n&notifyClose != 0 {
				return tcpip.ErrAborted
			}

		case wakerForNewSegment:
			if err := h.processSegments(); err != nil {
				return err
			}
		}
	}

	return nil
}

// parseSynOptions parses the options received in a syn segment and returns the
// relevant ones. If no window scale option is specified, ws is returned as -1;
// this is because the absence of the option indicates that the we cannot use
// window scaling on the receive end either.
func parseSynOptions(s *segment) (mss uint16, ws int, ok bool) {
	// Per RFC 1122, page 85: "If an MSS option is not received at
	// connection setup, TCP MUST assume a default send MSS of 536."
	mss = 536
	ws = -1
	opts := s.options
	limit := len(opts)
	for i := 0; i < limit; {
		switch opts[i] {
		case header.TCPOptionEOL:
			i = limit
		case header.TCPOptionNOP:
			i++
		case header.TCPOptionMSS:
			if i+4 > limit || opts[i+1] != 4 {
				return 0, -1, false
			}
			mss = uint16(opts[i+2])<<8 | uint16(opts[i+3])
			if mss == 0 {
				return 0, -1, false
			}
			i += 4

		case header.TCPOptionWS:
			if i+3 > limit || opts[i+1] != 3 {
				return 0, -1, false
			}
			ws = int(opts[i+2])
			if ws > maxWndScale {
				ws = maxWndScale
			}
			i += 3

		default:
			// We don't recognize this option, just skip over it.
			if i+2 > limit {
				return 0, -1, false
			}
			l := int(opts[i+1])
			if i < 2 || i+l > limit {
				return 0, -1, false
			}
			i += l
		}
	}

	return mss, ws, true
}

func sendSynTCP(r *stack.Route, id stack.TransportEndpointID, flags byte, seq, ack seqnum.Value, rcvWnd seqnum.Size, rcvWndScale int) error {
	// Initialize the options.
	mss := r.MTU() - header.TCPMinimumSize
	options := []byte{
		// Initialize the MSS option.
		header.TCPOptionMSS, 4, byte(mss >> 8), byte(mss),

		// Initialize the WS option. It must be the last one so that it
		// can be removed if rcvWndScale is negative (disabled).
		header.TCPOptionWS, 3, uint8(rcvWndScale), header.TCPOptionNOP,
	}

	if rcvWndScale < 0 {
		options = options[:len(options)-4]
	}

	return sendTCPWithOptions(r, id, nil, flags, seq, ack, rcvWnd, options)
}

// sendTCPWithOptions sends a TCP segment with the provided options via the
// provided network endpoint and under the provided identity.
func sendTCPWithOptions(r *stack.Route, id stack.TransportEndpointID, data buffer.View, flags byte, seq, ack seqnum.Value, rcvWnd seqnum.Size, opts []byte) error {
	// Allocate a buffer for the TCP header.
	hdr := buffer.NewPrependable(header.TCPMinimumSize + int(r.MaxHeaderLength()) + len(opts))

	if rcvWnd > 0xffff {
		rcvWnd = 0xffff
	}

	// Initialize the header.
	tcp := header.TCP(hdr.Prepend(header.TCPMinimumSize + len(opts)))
	tcp.Encode(&header.TCPFields{
		SrcPort:    id.LocalPort,
		DstPort:    id.RemotePort,
		SeqNum:     uint32(seq),
		AckNum:     uint32(ack),
		DataOffset: uint8(header.TCPMinimumSize + len(opts)),
		Flags:      flags,
		WindowSize: uint16(rcvWnd),
	})
	copy(tcp[header.TCPMinimumSize:], opts)

	length := uint16(hdr.UsedLength())
	xsum := r.PseudoHeaderChecksum(ProtocolNumber)
	if data != nil {
		length += uint16(len(data))
		xsum = header.Checksum(data, xsum)
	}

	tcp.SetChecksum(^tcp.CalculateChecksum(xsum, length))

	return r.WritePacket(&hdr, data, ProtocolNumber)
}

// sendTCP sends a TCP segment via the provided network endpoint and under the
// provided identity.
func sendTCP(r *stack.Route, id stack.TransportEndpointID, data buffer.View, flags byte, seq, ack seqnum.Value, rcvWnd seqnum.Size) error {
	// Allocate a buffer for the TCP header.
	hdr := buffer.NewPrependable(header.TCPMinimumSize + int(r.MaxHeaderLength()))

	if rcvWnd > 0xffff {
		rcvWnd = 0xffff
	}

	// Initialize the header.
	tcp := header.TCP(hdr.Prepend(header.TCPMinimumSize))
	tcp.Encode(&header.TCPFields{
		SrcPort:    id.LocalPort,
		DstPort:    id.RemotePort,
		SeqNum:     uint32(seq),
		AckNum:     uint32(ack),
		DataOffset: header.TCPMinimumSize,
		Flags:      flags,
		WindowSize: uint16(rcvWnd),
	})

	length := uint16(hdr.UsedLength())
	xsum := r.PseudoHeaderChecksum(ProtocolNumber)
	if data != nil {
		length += uint16(len(data))
		xsum = header.Checksum(data, xsum)
	}

	tcp.SetChecksum(^tcp.CalculateChecksum(xsum, length))

	return r.WritePacket(&hdr, data, ProtocolNumber)
}

// sendRaw sends a TCP segment to the endpoint's peer.
func (e *endpoint) sendRaw(data buffer.View, flags byte, seq, ack seqnum.Value, rcvWnd seqnum.Size) error {
	return sendTCP(&e.route, e.id, data, flags, seq, ack, rcvWnd)
}

func (e *endpoint) handleWrite() bool {
	// Nothing to do if already closed.
	if e.snd.closed {
		return true
	}

	// Move packets from send queue to send list. The queue is accessible
	// from other goroutines and protected by the send mutex, while the send
	// list is only accessible from the handler goroutine, so it needs no
	// mutexes.
	e.sndBufMu.Lock()

	first := e.sndQueue.Front()
	if first != nil {
		e.snd.writeList.PushBackList(&e.sndQueue)
		e.snd.sndNxtList.UpdateForward(e.sndBufInQueue)
		e.sndBufInQueue = 0
	}

	e.sndBufMu.Unlock()

	// Initialize the next segment to write if it's currently nil.
	if e.snd.writeNext == nil {
		e.snd.writeNext = first
	}

	// Push out any new packets.
	e.snd.sendData()

	return true
}

func (e *endpoint) handleClose() bool {
	// Nothing to do if already closed.
	if e.snd.closed {
		return true
	}

	// Drain the send queue.
	e.handleWrite()

	// Queue the FIN packet and mark send side as closed.
	e.snd.closed = true
	e.snd.sndNxtList++

	// Push out the FIN packet.
	e.snd.sendData()

	return true
}

// resetConnection sends a RST segment and puts the endpoint in an error state
// with the given error code.
// This method must only be called from the protocol goroutine.
func (e *endpoint) resetConnection(err error) {
	e.sendRaw(nil, flagAck|flagRst, e.snd.sndUna, e.rcv.rcvNxt, 0)

	e.mu.Lock()
	e.state = stateError
	e.hardError = err
	e.mu.Unlock()
}

// completeWorker is called by the worker goroutine when it's about to exit. It
// marks the worker as completed and performs cleanup work if requested by
// Close().
func (e *endpoint) completeWorker() {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.workerRunning = false
	if e.workerCleanup {
		e.cleanup()
	}
}

// handleSegments pulls segments from the queue and processes them. It returns
// true if the protocol loop should continue, false otherwise.
func (e *endpoint) handleSegments() bool {
	checkRequeue := true
	for i := 0; i < maxSegmentsPerWake; i++ {
		s := e.segmentQueue.dequeue()
		if s == nil {
			checkRequeue = false
			break
		}

		if s.flagIsSet(flagRst) {
			if e.rcv.acceptable(s.sequenceNumber, 0) {
				// RFC 793, page 37 states that "in all states
				// except SYN-SENT, all reset (RST) segments are
				// validated by checking their SEQ-fields." So
				// we only process it if it's acceptable.
				s.decRef()
				e.mu.Lock()
				e.state = stateError
				e.hardError = tcpip.ErrConnectionReset
				e.mu.Unlock()
				return false
			}
		} else if s.flagIsSet(flagAck) {
			// Patch the window size in the segment according to the
			// send window scale.
			s.window <<= e.snd.sndWndScale

			// RFC 793, page 41 states that "once in the ESTABLISHED
			// state all segments must carry current acknowledgment
			// information."
			e.rcv.handleRcvdSegment(s)
			e.snd.handleRcvdSegment(s)
		}
		s.decRef()
	}

	// If the queue is not empty, make sure we'll wake up in the next
	// iteration.
	if checkRequeue && !e.segmentQueue.empty() {
		e.newSegmentWaker.Assert()
	}

	// Send an ACK for all processed packets if needed.
	if e.rcv.rcvNxt != e.snd.maxSentAck {
		e.snd.sendAck()
	}

	return true
}

// protocolMainLoop is the main loop of the TCP protocol. It runs in its own
// goroutine and is responsible for sending segments and handling received
// segments.
func (e *endpoint) protocolMainLoop(passive bool) error {
	var closeTimer *time.Timer
	var closeWaker sleep.Waker

	defer func() {
		e.waiterQueue.Notify(waiter.EventIn | waiter.EventOut)
		e.completeWorker()

		if e.snd != nil {
			e.snd.resendTimer.Stop()
		}

		if closeTimer != nil {
			closeTimer.Stop()
		}
	}()

	if !passive {
		// This is an active connection, so we must initiate the 3-way
		// handshake, and then inform potential waiters about its
		// completion.
		h, err := newHandshake(e, seqnum.Size(e.receiveBufferAvailable()))
		if err == nil {
			err = h.execute()
		}
		if err != nil {
			e.lastErrorMu.Lock()
			e.lastError = err
			e.lastErrorMu.Unlock()

			e.mu.Lock()
			e.state = stateError
			e.hardError = err
			e.mu.Unlock()

			return err
		}

		// Transfer handshake state to TCP connection. We disable
		// receive window scaling if the peer doesn't support it
		// (indicated by a negative send window scale).
		e.snd = newSender(e, h.iss, h.ackNum-1, h.sndWnd, h.mss, h.sndWndScale)

		e.rcvListMu.Lock()
		e.rcv = newReceiver(e, h.ackNum-1, h.rcvWnd, h.effectiveRcvWndScale())
		e.rcvListMu.Unlock()
	}

	// Tell waiters that the endpoint is connected and writable.
	e.mu.Lock()
	e.state = stateConnected
	e.mu.Unlock()

	e.waiterQueue.Notify(waiter.EventOut)

	// Set up the functions that will be called when the main protocol loop
	// wakes up.
	funcs := []struct {
		w *sleep.Waker
		f func() bool
	}{
		{
			w: &e.sndWaker,
			f: e.handleWrite,
		},
		{
			w: &e.sndCloseWaker,
			f: e.handleClose,
		},
		{
			w: &e.newSegmentWaker,
			f: e.handleSegments,
		},
		{
			w: &closeWaker,
			f: func() bool {
				e.resetConnection(tcpip.ErrConnectionAborted)
				return false
			},
		},
		{
			w: &e.snd.resendWaker,
			f: func() bool {
				if !e.snd.retransmitTimerExpired() {
					e.resetConnection(tcpip.ErrTimeout)
					return false
				}
				return true
			},
		},
		{
			w: &e.notificationWaker,
			f: func() bool {
				n := e.fetchNotifications()
				if n&notifyNonZeroReceiveWindow != 0 {
					e.rcv.nonZeroWindow()
				}

				if n&notifyReceiveWindowChanged != 0 {
					e.rcv.pendingBufSize = seqnum.Size(e.receiveBufferSize())
				}

				if n&notifyClose != 0 && closeTimer == nil {
					// Reset the connection 3 seconds after the
					// endpoint has been closed.
					closeTimer = time.AfterFunc(3*time.Second, func() {
						closeWaker.Assert()
					})
				}
				return true
			},
		},
	}

	// Initialize the sleeper based on the wakers in funcs.
	s := sleep.Sleeper{}
	for i := range funcs {
		s.AddWaker(funcs[i].w, i)
	}

	// Main loop. Handle segments until both send and receive ends of the
	// connection have completed.
	for !e.rcv.closed || !e.snd.closed || e.snd.sndUna != e.snd.sndNxtList {
		e.workMu.Unlock()
		v, _ := s.Fetch(true)
		e.workMu.Lock()
		if !funcs[v].f() {
			return nil
		}
	}

	// Mark endpoint as closed.
	e.mu.Lock()
	e.state = stateClosed
	e.mu.Unlock()

	return nil
}
