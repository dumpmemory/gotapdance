/*
TODO: It probably should have read flow that reads messages and says STAAAHP to channel when read
TODO: here we actually can avoid reconnecting if idle for too long
TODO: confirm that all writes are recorded towards data limit

 _______________________TapdanceFlowConn Mode Chart ____________________________
|FlowType     |Default Tag|Diff from old-school bidirectional  | Engines spawned|
|-------------|-----------|------------------------------------|----------------|
|Bidirectional| HTTP GET  |                                    | Writer, Reader |
|Upload       | HTTP POST |acquires upload                     | Writer, Reader |
|ReadOnly     | HTTP GET  |yields upload, writer sync ignored  | Reader         |
\_____________|___________|____________________________________|_______________*/

package tapdance

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"github.com/SergeyFrolov/bsbuffer"
	"github.com/golang/protobuf/proto"
	"io"
	"net"
	"sync"
	"time"
)

// Represents single tapdance flow
type TapdanceFlowConn struct {
	tdRaw *tdRawConn

	bsbuf     *bsbuffer.BSBuffer
	recvbuf   []byte
	headerBuf [6]byte

	writeSliceChan    chan []byte
	writeResultChan   chan ioOpResult
	writtenBytesTotal int

	yieldConfirmed chan struct{} // used by rConn to signal that flow was picked up

	readOnly         bool // if readOnly -- we don't need to wait for write engine to stop
	reconnectSuccess chan bool
	reconnectStarted chan struct{}

	finSent bool // used only by reader to know if it has already scheduled reconnect

	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error

	flowType flowType
}

// Returns TapDance connection, that is ready to be Dial'd
func NewTapDanceConn() (net.Conn, error) {
	return makeTdFlow(flowBidirectional, nil)
}

// Prepares TD flow: does not make any network calls nor sets up engines
func makeTdFlow(flow flowType, tdRaw *tdRawConn) (*TapdanceFlowConn, error) {
	if tdRaw == nil {
		// raw TapDance connection is not given, make a new one
		stationPubkey := Assets().GetPubkey()
		remoteConnId := make([]byte, 16)
		rand.Read(remoteConnId[:])
		tdRaw = makeTdRaw(tagHttpGetIncomplete,
			stationPubkey[:],
			remoteConnId[:])
		tdRaw.sessionId = sessionsTotal.GetAndInc()
	}

	flowConn := &TapdanceFlowConn{tdRaw: tdRaw}
	flowConn.bsbuf = bsbuffer.NewBSBuffer()
	flowConn.closed = make(chan struct{})
	flowConn.flowType = flow
	return flowConn, nil
}

func (flowConn *TapdanceFlowConn) Dial() error {
	if flowConn.tdRaw.tlsConn == nil {
		// if still hasn't dialed
		err := flowConn.tdRaw.Dial()
		if err != nil {
			return err
		}
	}
	// don't lose initial msg from station
	// strip off state transition and push protobuf up for processing
	flowConn.tdRaw.initialMsg.StateTransition = nil
	err := flowConn.processProto(flowConn.tdRaw.initialMsg)
	if err != nil {
		flowConn.closeWithErrorOnce(err)
		return err
	}

	switch flowConn.flowType {
	case flowUpload:
		fallthrough
	case flowBidirectional:
		go flowConn.spawnReaderEngine()
		flowConn.reconnectSuccess = make(chan bool, 1)
		flowConn.reconnectStarted = make(chan struct{})
		flowConn.writeSliceChan = make(chan []byte)
		flowConn.writeResultChan = make(chan ioOpResult)
		go flowConn.spawnWriterEngine()
		return nil
	case flowReadOnly:
		go flowConn.spawnReaderEngine()
		return nil
	default:
		panic("Not implemented")
	}
}

type ioOpResult struct {
	err error
	n   int
}

func (flowConn *TapdanceFlowConn) schedReconnectNow() {
	flowConn.tdRaw.tlsConn.SetReadDeadline(time.Now())
}

// returns bool indicating success of reconnect
func (flowConn *TapdanceFlowConn) awaitReconnect() bool {
	defer func() { flowConn.writtenBytesTotal = 0 }()
	for {
		select {
		case <-flowConn.reconnectStarted:
		case <-flowConn.closed:
			return false
		case reconnectOk := <-flowConn.reconnectSuccess:
			return reconnectOk
		}
	}
}

// Write writes data to the connection.
// Write can be made to time out and return an Error with Timeout() == true
// after a fixed time limit; see SetDeadline and SetWriteDeadline.
func (flowConn *TapdanceFlowConn) spawnWriterEngine() {
	defer close(flowConn.writeResultChan)
	for {
		select {
		case <-flowConn.reconnectStarted:
			if !flowConn.awaitReconnect() {
				return
			}
		case <-flowConn.closed:
			return
		case b := <-flowConn.writeSliceChan:
			ioResult := ioOpResult{}
			bytesSent := 0

			canSend := func() int {
				// checks the upload limit
				// 6 is max header size (protobufs aren't sent here though)
				// 1024 is max transition message size
				return flowConn.tdRaw.UploadLimit -
					flowConn.writtenBytesTotal - 6 - 1024
			}
			for bytesSent < len(b) {
				idxToSend := len(b)
				if idxToSend-bytesSent > canSend() {
					Logger().Infof("%s reconnecting due to upload limit: "+
						"idxToSend (%d) - bytesSent(%d) > UploadLimit(%d) - "+
						"writtenBytesTotal(%d) - 6 - 1024 \n",
						flowConn.idStr(), idxToSend, bytesSent,
						flowConn.tdRaw.UploadLimit, flowConn.writtenBytesTotal)
					flowConn.schedReconnectNow()
					if !flowConn.awaitReconnect() {
						return
					}
				}
				Logger().Debugf("%s WriterEngine: writing\n%s", flowConn.idStr(), hex.Dump(b))

				if cs := maxInt(canSend(), int(^int16(0))); idxToSend-bytesSent > cs {
					// just reconnected and still can't send: time to chunk
					idxToSend = bytesSent + cs
				}

				// TODO: outerProto limit on data size
				bufToSend := b[bytesSent:idxToSend]
				bufToSendWithHeader := getMsgWithHeader(msg_raw_data, bufToSend) // TODO: optimize!
				headerSize := len(bufToSendWithHeader) - len(bufToSend)

				n, err := flowConn.tdRaw.tlsConn.Write(bufToSendWithHeader)
				if n >= headerSize {
					// TODO: that's kinda hacky
					n -= headerSize
				}
				ioResult.n += n
				bytesSent += n
				flowConn.writtenBytesTotal += len(bufToSendWithHeader)
				if err != nil {
					ioResult.err = err
					break
				}
			}
			select {
			case flowConn.writeResultChan <- ioResult:
			case <-flowConn.closed:
				return
			}
		}
	}
}

func (flowConn *TapdanceFlowConn) spawnReaderEngine() {
	flowConn.updateReadDeadline()
	flowConn.recvbuf = make([]byte, 1500)
	for {
		msgType, msgLen, err := flowConn.readHeader()
		if err != nil {
			flowConn.closeWithErrorOnce(err)
			return
		}
		if msgLen == 0 {
			continue // wtf?
		}
		switch msgType {
		case msg_raw_data:
			buf, err := flowConn.readRawData(msgLen)
			if err != nil {
				flowConn.closeWithErrorOnce(err)
				return
			}
			Logger().Debugf("%s ReaderEngine: read\n%s",
				flowConn.idStr(), hex.Dump(buf))
			_, err = flowConn.bsbuf.Write(buf)
			if err != nil {
				flowConn.closeWithErrorOnce(err)
				return
			}
		case msg_protobuf:
			msg, err := flowConn.readProtobuf(msgLen)
			if err != nil {
				flowConn.closeWithErrorOnce(err)
				return
			}
			err = flowConn.processProto(msg)
			if err != nil {
				flowConn.closeWithErrorOnce(err)
				return
			}
		default:
			flowConn.closeWithErrorOnce(errors.New("Corrupted outer protocol header: " +
				msgType.Str()))
			return
		}
	}
}

// Write writes data to the connection.
// Write can be made to time out and return an Error with Timeout() == true
// after a fixed time limit; see SetDeadline and SetWriteDeadline.
func (flowConn *TapdanceFlowConn) Write(b []byte) (int, error) {
	select {
	case flowConn.writeSliceChan <- b:
	case <-flowConn.closed:
		return 0, flowConn.closeErr
	}
	select {
	case r := <-flowConn.writeResultChan:
		return r.n, r.err
	case <-flowConn.closed:
		return 0, flowConn.closeErr
	}
}

func (flowConn *TapdanceFlowConn) Read(b []byte) (int, error) {
	return flowConn.bsbuf.Read(b)
}

func (rConn *TapdanceFlowConn) readRawData(msgLen int) ([]byte, error) {
	if cap(rConn.recvbuf) < msgLen {
		rConn.recvbuf = make([]byte, msgLen)
	}
	var err error
	var readBytes int
	var readBytesTotal int // both header and body
	// Get the message itself
	for readBytesTotal < msgLen {
		readBytes, err = rConn.tdRaw.tlsConn.Read(rConn.recvbuf[readBytesTotal:])
		readBytesTotal += int(readBytes)
		if err != nil {
			err = rConn.actOnReadError(err)
			if err != nil {
				return rConn.recvbuf[:readBytesTotal], err
			}
		}
	}
	return rConn.recvbuf[:readBytesTotal], err
}

func (flowConn *TapdanceFlowConn) readProtobuf(msgLen int) (msg StationToClient, err error) {
	rbuf := make([]byte, msgLen)
	var readBytes int
	var readBytesTotal int // both header and body
	// Get the message itself
	for readBytesTotal < msgLen {
		readBytes, err = flowConn.tdRaw.tlsConn.Read(rbuf[readBytesTotal:])
		readBytesTotal += readBytes
		if err != nil {
			err = flowConn.actOnReadError(err)
			if err != nil {
				return
			}
		}
	}
	err = proto.Unmarshal(rbuf[:], &msg)
	return
}

func (flowConn *TapdanceFlowConn) readHeader() (msgType MsgType, msgLen int, err error) {
	// For each message we first read outer protocol header to see if it's protobuf or data

	var readBytes int
	var readBytesTotal uint32 // both header and body
	headerSize := uint32(2)

	//TODO: check FIN+last data case
	for readBytesTotal < headerSize {
		readBytes, err = flowConn.tdRaw.tlsConn.Read(flowConn.headerBuf[readBytesTotal:headerSize])
		readBytesTotal += uint32(readBytes)
		if err != nil {
			err = flowConn.actOnReadError(err)
			if err != nil {
				return
			}
		}
	}

	// Get TIL
	typeLen := Uint16toInt16(binary.BigEndian.Uint16(flowConn.headerBuf[0:2]))
	if typeLen < 0 {
		msgType = msg_raw_data
		msgLen = int(-typeLen)
	} else if typeLen > 0 {
		msgType = msg_protobuf
		msgLen = int(typeLen)
	} else {
		// protobuf with size over 32KB, not fitting into 2-byte TL
		msgType = msg_protobuf
		headerSize += 4
		for readBytesTotal < headerSize {
			readBytes, err = flowConn.tdRaw.tlsConn.Read(flowConn.headerBuf[readBytesTotal:headerSize])
			readBytesTotal += uint32(readBytes)
			if err != nil {
				err = flowConn.actOnReadError(err)
				if err != nil {
					return
				}
			}
		}
		msgLen = int(binary.BigEndian.Uint32(flowConn.headerBuf[2:6]))
	}
	return
}

// Allows scheduling/doing reconnects in the middle of reads
func (flowConn *TapdanceFlowConn) actOnReadError(err error) error {
	if err == nil {
		return nil
	}

	willScheduleReconnect := false
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		// Timeout is used as a signal to schedule reconnect, as reconnect is indeed time dependent.
		// One can also SetDeadline(NOW) to schedule deadline NOW.
		// After EXPECT_RECONNECT and FIN are sent, deadline is used to signal that flow timed out
		// waiting for FIN back.
		willScheduleReconnect = true
	}

	// "EOF is the error returned by Read when no more input is available. Functions should
	// return EOF only to signal a graceful end of input." (e.g. FIN was received)
	// "ErrUnexpectedEOF means that EOF was encountered in the middle of reading a fixed-size
	// block or data structure."
	willReconnect := (err == io.EOF || err == io.ErrUnexpectedEOF)

	if willScheduleReconnect {
		Logger().Infoln(flowConn.tdRaw.idStr() + " scheduling reconnect")
		if flowConn.finSent {
			// timeout is hit another time before reconnect
			return errors.New("reconnect scheduling: timed out waiting for FIN back")
		}
		if flowConn.flowType != flowReadOnly {
			// notify writer, if needed
			select {
			case <-flowConn.closed:
				return errors.New("reconnect scheduling: closed while notifiyng writer")
			case flowConn.reconnectStarted <- struct{}{}:
			}
		}

		transition := C2S_Transition_C2S_EXPECT_RECONNECT
		if flowConn.flowType == flowUpload {
			transition = C2S_Transition_C2S_EXPECT_UPLOADONLY_RECONN
		}
		_, err = flowConn.tdRaw.writeTransition(transition)
		if err != nil {
			return errors.New("reconnect scheduling: failed to send " +
				transition.String() + ": " + err.Error())
		}

		if flowConn.flowType == flowUpload {
			// for upload-only flows we reconnect right away
			willReconnect = true
		} else {
			flowConn.tdRaw.tlsConn.SetReadDeadline(
				time.Now().Add(time.Millisecond * time.Duration(waitForFINDie)))
			err = flowConn.tdRaw.closeWrite()
			if err != nil {
				Logger().Infoln(flowConn.tdRaw.idStr() + " reconnect scheduling:" +
					"failed to send FIN: " + err.Error() +
					". Closing roughly and moving on.")
				flowConn.tdRaw.Close()
			}
			flowConn.finSent = true
			return nil
		}
	}

	if willReconnect {
		Logger().Infoln(flowConn.tdRaw.idStr() + " reconnecting")
		if (flowConn.flowType != flowUpload && !flowConn.finSent) ||
			err == io.ErrUnexpectedEOF {
			Logger().Infoln(flowConn.tdRaw.idStr() + " reconnect: FIN is unexpected")
		}
		err = flowConn.tdRaw.Redial()
		if flowConn.flowType != flowReadOnly {
			// wake up writer engine
			select {
			case <-flowConn.closed:
			case flowConn.reconnectSuccess <- (err == nil):
			}
		}
		if err != nil {
			return errors.New("reconnect: failed to Redial: " + err.Error())
		}
		flowConn.finSent = false
		// strip off state transition and push protobuf up for processing
		flowConn.tdRaw.initialMsg.StateTransition = nil
		err = flowConn.processProto(flowConn.tdRaw.initialMsg)
		if err == nil {
			flowConn.updateReadDeadline()
			return nil
		} else if err == errMsgClose {
			// errMsgClose actually won't show up here
			Logger().Infoln(flowConn.tdRaw.idStr() + " closing cleanly with MSG_CLOSE")
			return io.EOF
		} // else: proceed and exit as a crash
	}

	Logger().Infoln(flowConn.tdRaw.idStr() + " crashing due to " + err.Error())
	return io.ErrUnexpectedEOF
}

// Sets read deadline to {when raw connection was establihsed} + {timeout} - {small random value}
func (flowConn *TapdanceFlowConn) updateReadDeadline() {
	amortizationVal := 0.9
	const minSubtrahend = 50
	const maxSubtrahend = 9500
	deadline := flowConn.tdRaw.establishedAt.Add(time.Millisecond *
		time.Duration(int(float64(flowConn.tdRaw.decoySpec.GetTimeout())*amortizationVal)-
			getRandInt(minSubtrahend, maxSubtrahend)))
	flowConn.tdRaw.tlsConn.SetReadDeadline(deadline)
}

func (flowConn *TapdanceFlowConn) acquireUpload() error {
	_, err := flowConn.tdRaw.writeTransition(C2S_Transition_C2S_ACQUIRE_UPLOAD)
	if err != nil {
		Logger().Infoln(flowConn.idStr() + " Failed attempt to acquire upload:" + err.Error())
	} else {
		Logger().Infoln(flowConn.idStr() + " Sent acquire upload request")
	}
	return err
}

func (flowConn *TapdanceFlowConn) yieldUpload() error {
	_, err := flowConn.tdRaw.writeTransition(C2S_Transition_C2S_YIELD_UPLOAD)
	if err != nil {
		Logger().Infoln(flowConn.idStr() + " Failed attempt to yield upload:" + err.Error())
	} else {
		Logger().Infoln(flowConn.idStr() + " Sent yield upload request")
	}
	return err
}

// TODO: implement on station, currently unused
// wait for rConn to confirm that flow was noticed
func (flowConn *TapdanceFlowConn) waitForYieldConfirmation() error {
	// camouflage issue
	timeout := time.After(20 * time.Second)
	select {
	case <-timeout:
		return errors.New("yeild confirmation timeout")
	case <-flowConn.yieldConfirmed:
		Logger().Infoln(flowConn.idStr() +
			" Successfully received yield confirmation from reader flow!")
		return nil
	case <-flowConn.closed:
		return flowConn.closeErr
	}
}

// Closes connection, channel and sets error ONCE, e.g. error won't be overwritten
func (flowConn *TapdanceFlowConn) closeWithErrorOnce(err error) error {
	if err == nil {
		// safeguard, shouldn't happen
		err = errors.New("closed with nil error!")
	}
	var errOut error
	flowConn.closeOnce.Do(func() {
		Logger().Infoln(flowConn.idStr() + " closed with error " + err.Error())
		flowConn.closeErr = errors.New(flowConn.idStr() + " " + err.Error())
		flowConn.bsbuf.Unblock()
		close(flowConn.closed)
		errOut = flowConn.tdRaw.Close()
	})
	return errOut
}

// Close closes the connection.
// Any blocked Read or Write operations will be unblocked and return errors.
func (flowConn *TapdanceFlowConn) Close() error {
	return flowConn.closeWithErrorOnce(errors.New("closed by application layer"))
}

func (flowConn *TapdanceFlowConn) idStr() string {
	return flowConn.tdRaw.idStr()
}

func (flowConn *TapdanceFlowConn) processProto(msg StationToClient) error {
	handleConfigInfo := func(conf *ClientConf) {
		currGen := Assets().GetGeneration()
		if conf.GetGeneration() < currGen {
			Logger().Infoln(flowConn.idStr()+" not appliying new config due"+
				" to lower generation: ", conf.GetGeneration(), " "+
				"(have:", currGen, ")")
			return
		} else if conf.GetGeneration() < currGen {
			Logger().Infoln(flowConn.idStr()+" not appliying new config due"+
				" to currently having same generation: ", currGen)
			return
		}

		_err := Assets().SetClientConf(conf)
		if _err != nil {
			Logger().Errorln(flowConn.idStr() +
				"Could not save SetClientConf():" + _err.Error())
		}
	}
	Logger().Debugln(flowConn.idStr() + " processing incoming protobuf: " + msg.String())
	// handle ConfigInfo
	if confInfo := msg.ConfigInfo; confInfo != nil {
		handleConfigInfo(confInfo)
		// TODO: if we ever get a ``safe'' decoy rotation - code below has to be rewritten
		if !Assets().IsDecoyInList(flowConn.tdRaw.decoySpec) {
			Logger().Warningln(flowConn.idStr() + ": current decoy is no " +
				"longer in the list, changing it! Read flow probably will break!")
			// if current decoy is no longer in the list
			flowConn.tdRaw.decoySpec = Assets().GetDecoy()
		}
		if !Assets().IsDecoyInList(flowConn.tdRaw.decoySpec) {
			Logger().Warningln(flowConn.idStr() + " current decoy is no " +
				"longer in the list, changing it! Write flow probably will break!")
			// if current decoy is no longer in the list
			flowConn.tdRaw.decoySpec = Assets().GetDecoy()
		}
	}

	// note that flowConn don't see first-message transitions, such as INIT or RECONNECT
	stateTransition := msg.GetStateTransition()
	switch stateTransition {
	case S2C_Transition_S2C_NO_CHANGE:
	// carry on
	case S2C_Transition_S2C_SESSION_CLOSE:
		Logger().Infof(flowConn.idStr() + " received MSG_CLOSE")
		return errMsgClose
	case S2C_Transition_S2C_ERROR:
		err := errors.New("message from station:" +
			msg.GetErrReason().String())
		Logger().Errorln(flowConn.idStr() + " " + err.Error())
		flowConn.closeWithErrorOnce(err)
		return err
	case S2C_Transition_S2C_CONFIRM_RECONNECT:
		fallthrough
	case S2C_Transition_S2C_SESSION_INIT:
		fallthrough
	default:
		err := errors.New("Unexpected StateTransition " +
			"in initialized Conn:" + stateTransition.String())
		Logger().Errorln(flowConn.idStr() + " " + err.Error())
		flowConn.closeWithErrorOnce(err)
		return err
	}
	return nil
}

// LocalAddr returns the local network address.
func (flowConn *TapdanceFlowConn) LocalAddr() net.Addr {
	// not sure if this function is meaningful in TapDance context
	return flowConn.tdRaw.tlsConn.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (flowConn *TapdanceFlowConn) RemoteAddr() net.Addr {
	return flowConn.tdRaw.tlsConn.RemoteAddr()
}

func (flowConn *TapdanceFlowConn) NetworkConn() net.Conn {
	return flowConn.tdRaw.tcpConn
}

// TODO: Deadlines should probably behave differently. No idea how.
// SetDeadline sets the read and write deadlines associated
// with the connection. It is equivalent to calling both
// SetReadDeadline and SetWriteDeadline.
//
// A deadline is an absolute time after which I/O operations
// fail with a timeout (see type Error) instead of
// blocking. The deadline applies to all future I/O, not just
// the immediately following call to Read or Write.
//
// An idle timeout can be implemented by repeatedly extending
// the deadline after successful Read or Write calls.
//
// A zero value for t means I/O operations will not time out.
func (flowConn *TapdanceFlowConn) SetDeadline(t time.Time) error {
	return flowConn.tdRaw.tlsConn.SetDeadline(t)
}

// SetReadDeadline sets the deadline for future Read calls.
// A zero value for t means Read will not time out.
func (flowConn *TapdanceFlowConn) SetReadDeadline(t time.Time) error {
	return flowConn.tdRaw.tlsConn.SetReadDeadline(t)
}

// SetWriteDeadline sets the deadline for future Write calls.
// Even if write times out, it may return n > 0, indicating that
// some of the data was successfully written.
// A zero value for t means Write will not time out.
func (flowConn *TapdanceFlowConn) SetWriteDeadline(t time.Time) error {
	return flowConn.tdRaw.tlsConn.SetWriteDeadline(t)
}