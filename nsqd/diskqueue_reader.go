package nsqd

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path"
	"sync"
	"time"
)

var (
	ErrConfirmSizeInvalid = errors.New("Confirm data size invalid.")
	ErrMoveOffsetInvalid  = errors.New("move offset invalid")
	ErrOffsetTypeMismatch = errors.New("offset type mismatch")
)

type diskQueueEndInfo struct {
	EndFileNum  int64
	EndPos      int64
	VirtualEnd  BackendOffset
	TotalMsgCnt int64
}

func (d *diskQueueEndInfo) GetOffset() BackendOffset {
	return d.VirtualEnd
}
func (d *diskQueueEndInfo) GetTotalMsgCnt() int64 {
	return d.TotalMsgCnt
}
func (d *diskQueueEndInfo) IsSame(other BackendQueueEnd) bool {
	if otherDiskEnd, ok := other.(*diskQueueEndInfo); ok {
		return *d == *otherDiskEnd
	}
	return false
}

type diskQueueOffset struct {
	FileNum int64
	Pos     int64
}

func (d *diskQueueOffset) GreatThan(o *diskQueueOffset) bool {
	if d.FileNum > o.FileNum {
		return true
	}
	if d.FileNum == o.FileNum && d.Pos > o.Pos {
		return true
	}
	return false
}

// diskQueueReader implements the BackendQueue interface
// providing a filesystem backed FIFO queue
type diskQueueReader struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	readPos           diskQueueOffset
	endPos            diskQueueOffset
	virtualReadOffset BackendOffset
	virtualEnd        BackendOffset
	// left message number for read
	depth int64
	// total need to read
	totalMsgCnt int64

	sync.RWMutex

	// instantiation time metadata
	readerMetaName  string
	readFrom        string
	dataPath        string
	maxBytesPerFile int64 // currently this cannot change once created
	minMsgSize      int32
	maxMsgSize      int32
	syncEvery       int64         // number of writes per fsync
	syncTimeout     time.Duration // duration of time per fsync
	exitFlag        int32
	needSync        bool

	// keeps track of the position where we have read
	// (but not yet sent over readChan)
	nextReadPos diskQueueOffset

	confirmedOffset        diskQueueOffset
	virtualConfirmedOffset BackendOffset

	readFile *os.File
	reader   *bufio.Reader

	// exposed via ReadChan()
	readResultChan   chan ReadResult
	skipReadErrChan  chan diskQueueOffset
	skipChan         chan BackendOffset
	skipResponseChan chan error

	endUpdatedChan         chan *diskQueueEndInfo
	endUpdatedResponseChan chan error
	exitChan               chan int
	exitSyncChan           chan int
	autoSkipError          bool
	confirmChan            chan BackendOffset
	confirmResponseChan    chan error
	maxConfirmWin          BackendOffset
}

// newDiskQueue instantiates a new instance of diskQueueReader, retrieving metadata
// from the filesystem and starting the read ahead goroutine
func newDiskQueueReader(readFrom string, metaname string, dataPath string, maxBytesPerFile int64,
	minMsgSize int32, maxMsgSize int32,
	syncEvery int64, syncTimeout time.Duration, autoSkip bool) BackendQueueReader {

	d := diskQueueReader{
		readFrom:               readFrom,
		readerMetaName:         metaname,
		dataPath:               dataPath,
		maxBytesPerFile:        maxBytesPerFile,
		minMsgSize:             minMsgSize,
		maxMsgSize:             maxMsgSize,
		readResultChan:         make(chan ReadResult),
		skipReadErrChan:        make(chan diskQueueOffset),
		skipChan:               make(chan BackendOffset),
		skipResponseChan:       make(chan error),
		endUpdatedChan:         make(chan *diskQueueEndInfo),
		endUpdatedResponseChan: make(chan error),
		exitChan:               make(chan int),
		exitSyncChan:           make(chan int),
		confirmChan:            make(chan BackendOffset),
		confirmResponseChan:    make(chan error),
		maxConfirmWin:          BackendOffset(10000),
		syncEvery:              syncEvery,
		syncTimeout:            syncTimeout,
		autoSkipError:          autoSkip,
	}

	// no need to lock here, nothing else could possibly be touching this instance
	err := d.retrieveMetaData()
	if err != nil && !os.IsNotExist(err) {
		nsqLog.LogErrorf("diskqueue(%s) failed to retrieveMetaData %v - %s",
			d.readFrom, d.readerMetaName, err)
	}

	go d.ioLoop()

	return &d
}

func (d *diskQueueReader) getCurrentFileEnd(offset diskQueueOffset) (int64, error) {
	curFileName := d.fileName(offset.FileNum)
	f, err := os.Stat(curFileName)
	if err != nil {
		return 0, err
	}
	return f.Size(), nil
}

// Depth returns the depth of the queue
func (d *diskQueueReader) Depth() int64 {
	d.Lock()
	defer d.Unlock()
	if d.confirmedOffset.FileNum > d.endPos.FileNum {
		return 0
	}
	if d.endPos.FileNum == d.confirmedOffset.FileNum {
		return d.endPos.Pos - d.confirmedOffset.Pos
	}

	return int64(d.virtualEnd - d.virtualConfirmedOffset)
}

func (d *diskQueueReader) ReadChan() chan ReadResult {
	return d.readResultChan
}

// Put writes a []byte to the queue
func (d *diskQueueReader) UpdateQueueEnd(e BackendQueueEnd) {
	end, ok := e.(*diskQueueEndInfo)
	if !ok || end == nil {
		return
	}
	d.Lock()
	defer d.Unlock()

	if d.exitFlag == 1 {
		return
	}
	d.endUpdatedChan <- end
	<-d.endUpdatedResponseChan
}

func (d *diskQueueReader) Delete() error {
	return d.exit(true)
}

// Close cleans up the queue and persists metadata
func (d *diskQueueReader) Close() error {
	err := d.exit(false)
	if err != nil {
		return err
	}
	return d.sync()
}

func (d *diskQueueReader) exit(deleted bool) error {
	d.Lock()
	defer d.Unlock()

	d.exitFlag = 1
	close(d.exitChan)
	// ensure that ioLoop has exited
	<-d.exitSyncChan

	if d.readFile != nil {
		d.readFile.Close()
		d.readFile = nil
	}

	if deleted {
		d.skipToEndofFile()
	}
	return nil
}

func (d *diskQueueReader) ConfirmRead(offset BackendOffset) error {
	d.RLock()
	defer d.RUnlock()

	if d.exitFlag == 1 {
		return errors.New("exiting")
	}
	d.confirmChan <- offset
	return <-d.confirmResponseChan
}

func (d *diskQueueReader) getVirtualOffsetDistance(prev diskQueueOffset, next diskQueueOffset) (BackendOffset, error) {
	diff := int64(0)
	if prev.GreatThan(&next) {
		return BackendOffset(diff), ErrMoveOffsetInvalid
	}
	if prev.FileNum == next.FileNum {
		diff = next.Pos - prev.Pos
		return BackendOffset(diff), nil
	}

	fsize, err := d.getCurrentFileEnd(prev)
	if err != nil {
		return BackendOffset(diff), err
	}
	left := fsize - prev.Pos
	prev.FileNum++
	prev.Pos = 0
	vdiff := BackendOffset(0)
	vdiff, err = d.getVirtualOffsetDistance(prev, next)
	return BackendOffset(int64(vdiff) + left), err
}

func (d *diskQueueReader) SkipReadToOffset(offset int64) error {
	d.RLock()
	defer d.RUnlock()
	if d.exitFlag == 1 {
		return errors.New("exiting")
	}
	d.skipChan <- BackendOffset(offset)
	return <-d.skipResponseChan
}

func (d *diskQueueReader) SkipToNext() error {
	d.RLock()
	defer d.RUnlock()

	if d.exitFlag == 1 {
		return errors.New("exiting")
	}
	// TODO: skip to next file number.
	return nil
}

func (d *diskQueueReader) SkipToEnd() error {
	d.RLock()
	defer d.RUnlock()

	if d.exitFlag == 1 {
		return errors.New("exiting")
	}
	d.skipChan <- d.virtualEnd
	return <-d.skipResponseChan
}

func (d *diskQueueReader) stepOffset(cur diskQueueOffset, step int64, maxStep diskQueueOffset) (diskQueueOffset, error) {
	newOffset := cur
	var err error
	if cur.FileNum > maxStep.FileNum {
		return newOffset, fmt.Errorf("offset invalid: %v , %v", cur, maxStep)
	}
	if step == 0 {
		return newOffset, nil
	}
	for {
		end := int64(0)
		if cur.FileNum < maxStep.FileNum {
			end, err = d.getCurrentFileEnd(newOffset)
			if err != nil {
				return newOffset, err
			}
		} else {
			end = maxStep.Pos
		}
		diff := end - newOffset.Pos
		if step > diff {
			newOffset.FileNum++
			newOffset.Pos = 0
			if newOffset.GreatThan(&maxStep) {
				return newOffset, fmt.Errorf("offset invalid: %v , %v, need step: %v",
					newOffset, maxStep, step)
			}
			step -= diff
			if step == 0 {
				return newOffset, nil
			}
		} else {
			newOffset.Pos += step
			if newOffset.GreatThan(&maxStep) {
				return newOffset, fmt.Errorf("offset invalid: %v , %v, need step: %v",
					newOffset, maxStep, step)
			}
			return newOffset, nil
		}
	}
}

func (d *diskQueueReader) internalConfirm(offset BackendOffset) error {
	if int64(offset) == -1 {
		d.confirmedOffset = d.readPos
		d.virtualConfirmedOffset = d.virtualReadOffset
		return nil
	}
	if offset <= d.virtualConfirmedOffset {
		return nil
	}
	if offset > d.virtualReadOffset {
		nsqLog.LogErrorf("confirm exceed read: %v, %v", offset, d.virtualReadOffset)
		return ErrConfirmSizeInvalid
	}
	diffVirtual := int64(offset - d.virtualConfirmedOffset)
	newConfirm, err := d.stepOffset(d.confirmedOffset, diffVirtual, d.readPos)
	if err != nil {
		nsqLog.LogErrorf("confirmed exceed the read pos: %v, %v", offset, d.virtualReadOffset)
		return ErrConfirmSizeInvalid
	}
	d.confirmedOffset = newConfirm
	d.virtualConfirmedOffset = offset
	return nil
}

func (d *diskQueueReader) internalSkipTo(voffset BackendOffset) error {
	if d.readFile != nil {
		d.readFile.Close()
		d.readFile = nil
	}
	newPos := d.endPos
	var err error
	if voffset > d.virtualEnd {
		nsqLog.LogErrorf("internal skip error : %v, skipping to : %v", err, voffset)
		return ErrMoveOffsetInvalid
	} else if voffset == d.virtualEnd {
		newPos = d.endPos
	} else {
		newPos, err = d.stepOffset(d.readPos, int64(voffset-d.virtualReadOffset), d.endPos)
		if err != nil {
			nsqLog.LogErrorf("internal skip error : %v, skipping to : %v", err, voffset)
			return err
		}
	}

	d.readPos = newPos
	d.virtualReadOffset = voffset

	d.confirmedOffset = d.readPos
	d.virtualConfirmedOffset = d.virtualReadOffset
	return nil
}

func (d *diskQueueReader) skipToNextFile() error {
	if d.readFile != nil {
		d.readFile.Close()
		d.readFile = nil
	}
	if d.readPos.FileNum >= d.endPos.FileNum {
		return d.skipToEndofFile()
	}

	d.readPos.FileNum++
	d.readPos.Pos = 0
	if d.confirmedOffset != d.readPos {
		nsqLog.LogErrorf("skip confirm from %v to %v.", d.confirmedOffset, d.readPos)
	}
	d.confirmedOffset = d.readPos
	d.virtualConfirmedOffset = d.virtualReadOffset
	return nil
}

func (d *diskQueueReader) skipToEndofFile() error {
	if d.readFile != nil {
		d.readFile.Close()
		d.readFile = nil
	}

	d.readPos = d.endPos
	d.virtualReadOffset = d.virtualEnd
	if d.confirmedOffset != d.readPos {
		nsqLog.LogErrorf("skip confirm from %v to %v.", d.confirmedOffset, d.readPos)
	}
	d.confirmedOffset = d.readPos
	d.virtualConfirmedOffset = d.virtualReadOffset

	return nil
}

// readOne performs a low level filesystem read for a single []byte
// while advancing read positions and rolling files, if necessary
func (d *diskQueueReader) readOne() (BackendOffset, []byte, error) {
	var err error
	var msgSize int32
	voffset := BackendOffset(0)

CheckFileOpen:

	voffset = d.virtualReadOffset
	if d.readFile == nil {
		curFileName := d.fileName(d.readPos.FileNum)
		d.readFile, err = os.OpenFile(curFileName, os.O_RDONLY, 0600)
		if err != nil {
			return voffset, nil, err
		}

		nsqLog.Logf("DISKQUEUE(%s): readOne() opened %s", d.readerMetaName, curFileName)

		if d.readPos.Pos > 0 {
			_, err = d.readFile.Seek(d.readPos.Pos, 0)
			if err != nil {
				d.readFile.Close()
				d.readFile = nil
				return voffset, nil, err
			}
		}

		d.reader = bufio.NewReader(d.readFile)
	}
	if d.readPos.FileNum < d.endPos.FileNum {
		stat, err := d.readFile.Stat()
		if err != nil {
			return voffset, nil, err
		}
		if d.readPos.Pos >= stat.Size() {
			d.readPos.FileNum++
			d.readPos.Pos = 0
			nsqLog.Logf("DISKQUEUE(%s): readOne() read end, try next: %v",
				d.readerMetaName, d.readPos.FileNum)
			d.readFile.Close()
			d.readFile = nil
			goto CheckFileOpen
		}
	}

	err = binary.Read(d.reader, binary.BigEndian, &msgSize)
	if err != nil {
		d.readFile.Close()
		d.readFile = nil
		return voffset, nil, err
	}

	if msgSize < d.minMsgSize || msgSize > d.maxMsgSize {
		// this file is corrupt and we have no reasonable guarantee on
		// where a new message should begin
		d.readFile.Close()
		d.readFile = nil
		return voffset, nil, fmt.Errorf("invalid message read size (%d)", msgSize)
	}

	readBuf := make([]byte, msgSize)
	_, err = io.ReadFull(d.reader, readBuf)
	if err != nil {
		d.readFile.Close()
		d.readFile = nil
		return voffset, nil, err
	}

	voffset = d.virtualReadOffset

	totalBytes := int64(4 + msgSize)

	// we only advance next* because we have not yet sent this to consumers
	// (where readFileNum, readPos will actually be advanced)
	oldPos := d.readPos
	d.readPos.Pos = d.readPos.Pos + totalBytes
	nsqLog.LogDebugf("=== read move forward: %v, %v, %v", oldPos,
		d.virtualReadOffset, d.readPos)

	d.virtualReadOffset += BackendOffset(totalBytes)

	// TODO: each data file should embed the maxBytesPerFile
	// as the first 8 bytes (at creation time) ensuring that
	// the value can change without affecting runtime
	isEnd := false
	if d.readPos.FileNum < d.endPos.FileNum {
		stat, err := d.readFile.Stat()
		if err == nil {
			isEnd = d.readPos.Pos >= stat.Size()
		} else {
			return voffset, readBuf, err
		}
	}
	if (d.readPos.Pos > d.maxBytesPerFile) && !isEnd {
		nsqLog.LogErrorf("should be end since next position is larger than maxfile size. %v", d.readPos)
	}
	if isEnd {
		if d.readFile != nil {
			d.readFile.Close()
			d.readFile = nil
		}

		d.readPos.FileNum++
		d.readPos.Pos = 0
	}

	// see if we need to clean up the old file
	if oldPos.FileNum != d.readPos.FileNum {
		// sync every time we start reading from a new file
		d.needSync = true
	}

	return voffset, readBuf, nil
}

// sync fsyncs the current writeFile and persists metadata
func (d *diskQueueReader) sync() error {
	err := d.persistMetaData()
	if err != nil {
		return err
	}

	d.needSync = false
	return nil
}

// retrieveMetaData initializes state from the filesystem
func (d *diskQueueReader) retrieveMetaData() error {
	var f *os.File
	var err error

	fileName := d.metaDataFileName()
	f, err = os.OpenFile(fileName, os.O_RDONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = fmt.Fscanf(f, "%d\n%d,%d,%d\n%d,%d,%d\n",
		&d.totalMsgCnt,
		&d.confirmedOffset.FileNum, &d.confirmedOffset.Pos, &d.virtualConfirmedOffset,
		&d.endPos.FileNum, &d.endPos.Pos, &d.virtualEnd)
	if err != nil {
		return err
	}
	d.readPos = d.confirmedOffset
	d.virtualReadOffset = d.virtualConfirmedOffset

	return nil
}

// persistMetaData atomically writes state to the filesystem
func (d *diskQueueReader) persistMetaData() error {
	var f *os.File
	var err error

	fileName := d.metaDataFileName()
	tmpFileName := fmt.Sprintf("%s.%d.tmp", fileName, rand.Int())

	// write to tmp file
	f, err = os.OpenFile(tmpFileName, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(f, "%d\n%d,%d,%d\n%d,%d,%d\n",
		d.totalMsgCnt,
		d.confirmedOffset.FileNum, d.confirmedOffset.Pos, d.virtualConfirmedOffset,
		d.endPos.FileNum, d.endPos.Pos, d.virtualEnd)
	if err != nil {
		f.Close()
		return err
	}
	f.Sync()
	f.Close()

	// atomically rename
	return atomicRename(tmpFileName, fileName)
}

func (d *diskQueueReader) metaDataFileName() string {
	return fmt.Sprintf(path.Join(d.dataPath, "%s.diskqueue.meta.reader.dat"),
		d.readerMetaName)
}

func (d *diskQueueReader) fileName(fileNum int64) string {
	return fmt.Sprintf(path.Join(d.dataPath, "%s.diskqueue.%06d.dat"), d.readFrom, fileNum)
}

func (d *diskQueueReader) checkTailCorruption() {
	if d.readPos.FileNum < d.endPos.FileNum || d.readPos.Pos < d.endPos.Pos {
		return
	}

	if d.readPos != d.endPos {
		nsqLog.LogErrorf(
			"diskqueue(%s) readFileNum > endFileNum (%v > %v), corruption, skipping to end ...",
			d.readerMetaName, d.readPos, d.endPos)

		d.skipToEndofFile()
		d.needSync = true
	}
}

func (d *diskQueueReader) handleReadError() {
	// shadow should not change the bad file, just log it.
	// TODO: try to find next message position from index log.
	newRead := d.readPos
	newRead.FileNum++
	newRead.Pos = 0
	if newRead.GreatThan(&d.endPos) {
		newRead = d.endPos
		d.virtualReadOffset = d.virtualEnd
	} else {
		vdiff, err := d.getVirtualOffsetDistance(d.readPos, newRead)
		if err != nil {
			nsqLog.LogErrorf("diskqueue(%s) move error (%v > %v), corruption, skipping to next...",
				d.readerMetaName, d.readPos, newRead)
			d.skipToNextFile()
			return
		}
		d.virtualReadOffset += vdiff
	}
	d.readPos = newRead
	d.confirmedOffset = d.readPos
	d.virtualConfirmedOffset = d.virtualReadOffset

	// significant state change, schedule a sync on the next iteration
	d.needSync = true
}

// ioLoop provides the backend for exposing a go channel (via ReadChan())
// in support of multiple concurrent queue consumers
//
// it works by looping and branching based on whether or not the queue has data
// to read and blocking until data is either read or written over the appropriate
// go channels
//
// conveniently this also means that we're asynchronously reading from the filesystem
// NOTE: never call any lock op or it may cause deadlock.
func (d *diskQueueReader) ioLoop() {
	var dataRead ReadResult
	var rerr error
	var syncerr error
	var count int64
	var r chan ReadResult
	continueRead := true

	syncTicker := time.NewTicker(d.syncTimeout)

	for {
		// dont sync all the time :)
		if count == d.syncEvery {
			count = 0
			d.needSync = true
		}

		if d.needSync {
			syncerr = d.sync()
			if syncerr != nil {
				nsqLog.LogErrorf("diskqueue(%s) failed to sync - %s",
					d.readerMetaName, syncerr)
			}
		}

		if rerr != nil {
			r = nil
		} else if d.virtualConfirmedOffset+d.maxConfirmWin < d.virtualReadOffset {
			// too much waiting confirm, just hold on.
			r = nil
		} else {
			if (d.readPos.FileNum < d.endPos.FileNum) || (d.readPos.Pos < d.endPos.Pos) {
				if continueRead {
					dataRead.offset, dataRead.data, rerr = d.readOne()
					dataRead.err = rerr
					if rerr != nil {
						nsqLog.LogErrorf("reading from diskqueue(%s) at %d of %s - %s",
							d.readerMetaName, d.readPos, d.fileName(d.readPos.FileNum), dataRead.err)
						if d.autoSkipError {
							d.handleReadError()
							rerr = nil
							continue
						}
					} else {
						continueRead = false
					}
				}
				r = d.readResultChan
			} else {
				r = nil
			}
		}

		select {
		// the Go channel spec dictates that nil channel operations (read or write)
		// in a select are skipped, we set r to d.readChan only when there is data to read
		case r <- dataRead:
			// moveForward sets needSync flag if a file is removed
			if rerr == nil {
				d.checkTailCorruption()
				continueRead = true
			}
		case skipInfo := <-d.skipChan:
			skiperr := d.internalSkipTo(skipInfo)
			rerr = nil
			d.skipResponseChan <- skiperr
		case endPos := <-d.endUpdatedChan:
			count++
			if d.endPos.FileNum != endPos.EndFileNum && endPos.EndPos == 0 {
				// a new file for the position
				d.needSync = true
			}
			if d.readPos.FileNum > endPos.EndFileNum {
				d.readPos.FileNum = endPos.EndFileNum
				d.readPos.Pos = endPos.EndPos
				d.virtualReadOffset = endPos.VirtualEnd
			}
			if (d.readPos.FileNum == endPos.EndFileNum) && (d.readPos.Pos > endPos.EndPos) {
				d.readPos.Pos = endPos.EndPos
				d.virtualReadOffset = endPos.VirtualEnd
			}
			d.endPos.Pos = endPos.EndPos
			d.endPos.FileNum = endPos.EndFileNum
			d.totalMsgCnt = endPos.TotalMsgCnt
			d.virtualEnd = endPos.VirtualEnd
			d.endUpdatedResponseChan <- nil

		case confirmInfo := <-d.confirmChan:
			d.confirmResponseChan <- d.internalConfirm(confirmInfo)

		case <-syncTicker.C:
			if count > 0 {
				count = 0
				d.needSync = true
			}
		case <-d.exitChan:
			goto exit
		}
	}

exit:
	nsqLog.Logf("DISKQUEUE(%s): closing ... ioLoop", d.readerMetaName)
	syncTicker.Stop()
	d.exitSyncChan <- 1
}
