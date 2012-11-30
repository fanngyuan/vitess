// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mysqlctl

/*
the binlogreader is intended to "tail -f" a binlog, but be smart enough
to stop tailing it when mysql is done writing to that binlog.  The stop
condition is if EOF is reached *and* the next file has appeared.
*/

import (
	"bufio"
	"code.google.com/p/vitess/go/relog"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	BINLOG_HEADER_SIZE = 4  // copied from mysqlbinlog.cc for mysql 5.0.33
	EVENT_HEADER_SIZE  = 19 // 4.0 and above, can be larger in 5.x
)

type stats struct {
	Reads     int64
	Bytes     int64
	Sleeps    int64
	StartTime time.Time
}

type request struct {
	config        *Mycnf
	startPosition int64
	file          *os.File
	nextFilename  string
	stats
}

type BinlogReader struct {
	binLogPrefix string

	// these parameters will have reasonable default values but can be tuned
	BinlogBlockSize int64
	MaxWaitTimeout  float64
	LogWaitTimeout  float64
}

func (blr *BinlogReader) binLogPathForId(fileId int) string {
	return fmt.Sprintf("%v.%06d", blr.binLogPrefix, fileId)
}

func NewBinlogReader(binLogPrefix string) *BinlogReader {
	return &BinlogReader{binLogPrefix: binLogPrefix, BinlogBlockSize: 16 * 1024, MaxWaitTimeout: 3600.0, LogWaitTimeout: 5.0}
}

/*
 based on http://forge.mysql.com/wiki/MySQL_Internals_Binary_Log
 +=====================================+
 | event  | timestamp         0 : 4    |
 | header +----------------------------+
 |        | type_code         4 : 1    |
 |        +----------------------------+
 |        | server_id         5 : 4    |
 |        +----------------------------+
 |        | event_length      9 : 4    |
 |        +----------------------------+
 |        | next_position    13 : 4    |
 |        +----------------------------+
 |        | flags            17 : 2    |
 +=====================================+
 | event  | fixed part       19 : y    |
 | data   +----------------------------+
 |        | variable part              |
 +=====================================+
*/
func readFirstEventSize(binlog io.ReadSeeker) uint32 {
	pos, _ := binlog.Seek(0, 1)
	defer binlog.Seek(pos, 0)

	if _, err := binlog.Seek(BINLOG_HEADER_SIZE+9, 0); err != nil {
		panic("failed binlog seek: " + err.Error())
	}

	var eventLength uint32
	if err := binary.Read(binlog, binary.LittleEndian, &eventLength); err != nil {
		panic("failed binlog read: " + err.Error())
	}
	return eventLength
}

func (blr *BinlogReader) serve(filename string, startPosition int64, writer http.ResponseWriter) {
	flusher := writer.(http.Flusher)
	stats := stats{StartTime: time.Now()}

	binlogFile, nextLog := blr.open(filename)
	defer binlogFile.Close()
	positionWaitStart := make(map[int64]time.Time)

	if startPosition > 0 {
		// the start position can be greater than the file length
		// in which case, we just keep rotating files until we find it
		for {
			size, err := binlogFile.Seek(0, 2)
			if err != nil {
				relog.Error("BinlogReader.serve seek err: %v", err)
				return
			}
			if startPosition > size {
				startPosition -= size

				// swap to next file
				binlogFile.Close()
				binlogFile, nextLog = blr.open(nextLog)

				// normally we chomp subsequent headers, so we have to
				// add this back into the position
				//startPosition += BINLOG_HEADER_SIZE
			} else {
				break
			}
		}

		// inject the header again to fool mysqlbinlog
		// FIXME(msolomon) experimentally determine the header size.
		// 5.1.50 is 106, 5.0.24 is 98
		firstEventSize := readFirstEventSize(binlogFile)
		prefixSize := int64(BINLOG_HEADER_SIZE + firstEventSize)
		writer.Header().Set("Vt-Binlog-Offset", strconv.FormatInt(prefixSize, 10))
		relog.Info("BinlogReader.serve inject header + first event: %v", prefixSize)

		position, err := binlogFile.Seek(0, 0)
		if err == nil {
			_, err = io.CopyN(writer, binlogFile, prefixSize)
			//relog.Info("BinlogReader %x copy @ %v:%v,%v", stats.StartTime, binlogFile.Name(), position, written)
		}
		if err != nil {
			relog.Error("BinlogReader.serve err: %v", err)
			return
		}
		position, err = binlogFile.Seek(startPosition, 0)
		relog.Info("BinlogReader %x seek to startPosition %v @ %v:%v", stats.StartTime, startPosition, binlogFile.Name(), position)
	} else {
		writer.Header().Set("Vt-Binlog-Offset", "0")
	}

	// FIXME(msolomon) register stats on http handler
	for {
		//position, _ := binlogFile.Seek(0, 1)
		written, err := io.CopyN(writer, binlogFile, blr.BinlogBlockSize)
		//relog.Info("BinlogReader %x copy @ %v:%v,%v", stats.StartTime, binlogFile.Name(), position, written)
		if err != nil && err != io.EOF {
			relog.Error("BinlogReader.serve err: %v", err)
			return
		}

		stats.Reads++
		stats.Bytes += written

		if written != blr.BinlogBlockSize {
			if _, statErr := os.Stat(nextLog); statErr == nil {
				relog.Info("BinlogReader swap log file: %v", nextLog)
				// swap to next log file
				binlogFile.Close()
				binlogFile, nextLog = blr.open(nextLog)
				positionWaitStart = make(map[int64]time.Time)
				binlogFile.Seek(BINLOG_HEADER_SIZE, 0)
			} else {
				flusher.Flush()
				position, _ := binlogFile.Seek(0, 1)
				relog.Info("BinlogReader %x wait for more data: %v:%v", stats.StartTime, binlogFile.Name(), position)
				// wait for more data
				time.Sleep(time.Duration(blr.LogWaitTimeout * 1e9))
				stats.Sleeps++
				now := time.Now()
				if lastSlept, ok := positionWaitStart[position]; ok {
					if (now.Sub(lastSlept)) > time.Duration(blr.MaxWaitTimeout*1e9) {
						relog.Error("MAX_WAIT_TIMEOUT exceeded, closing connection")
						return
					}
				} else {
					positionWaitStart[position] = now
				}
			}
		}
	}
}

func (blr *BinlogReader) HandleBinlogRequest(rw http.ResponseWriter, req *http.Request) {
	defer func() {
		if err := recover(); err != nil {
			// nothing to do, but note it here and soldier on
			relog.Error("HandleBinlogRequest failed: %v", err)
		}
	}()

	// FIXME(msolomon) some sort of security, no?
	relog.Info("serve %v", req.URL.Path)
	// path is something like /vt/vt-xxxxxx-bin-log:position
	pieces := strings.SplitN(path.Base(req.URL.Path), ":", 2)
	pos, _ := strconv.ParseInt(pieces[1], 10, 64)
	blr.serve(pieces[0], pos, rw)
}

// return open log file and the name of the next log path to watch
func (blr *BinlogReader) open(name string) (*os.File, string) {
	ext := path.Ext(name)
	fileId, err := strconv.Atoi(ext[1:])
	if err != nil {
		panic(errors.New("bad binlog name: " + name))
	}
	logPath := blr.binLogPathForId(fileId)
	if !strings.HasSuffix(logPath, name) {
		panic(errors.New("binlog name mismatch: " + logPath + " vs " + name))
	}
	file, err := os.Open(logPath)
	if err != nil {
		panic(err)
	}
	nextLog := blr.binLogPathForId(fileId + 1)
	return file, nextLog
}

func (blr *BinlogReader) ServeData(writer io.Writer, filename string, startPosition int64) {
	stats := stats{StartTime: time.Now()}

	binlogFile, nextLog := blr.open(filename)
	defer binlogFile.Close()
	positionWaitStart := make(map[int64]time.Time)

	//var offsetString string
	bufWriter := bufio.NewWriterSize(writer, 16*1024)

	if startPosition > 0 {
		size, err := binlogFile.Seek(0, 2)
		if err != nil {
			relog.Error("BinlogReader.ServeData seek err: %v", err)
			return
		}
		if startPosition > size {
			relog.Error("BinlogReader.ServeData: start position %v greater than size %v", startPosition, size)
			return
		}

		// inject the header again to fool mysqlbinlog
		// FIXME(msolomon) experimentally determine the header size.
		// 5.1.50 is 106, 5.0.24 is 98
		firstEventSize := readFirstEventSize(binlogFile)
		prefixSize := int64(BINLOG_HEADER_SIZE + firstEventSize)
		relog.Info("BinlogReader.serve inject header + first event: %v", prefixSize)
		//offsetString = fmt.Sprintf("Vt-Binlog-Offset: %v\n", strconv.FormatInt(prefixSize, 10))

		position, err := binlogFile.Seek(0, 0)
		if err == nil {
			_, err = io.CopyN(writer, binlogFile, prefixSize)
			//relog.Info("Sending prefix, BinlogReader copy @ %v:%v,%v", binlogFile.Name(), position, written)
		}
		if err != nil {
			relog.Error("BinlogReader.ServeData err: %v", err)
			return
		}
		position, err = binlogFile.Seek(startPosition, 0)
		if err != nil {
			relog.Error("Failed BinlogReader seek to startPosition %v @ %v:%v", startPosition, binlogFile.Name(), position)
			return
		}
		relog.Info("BinlogReader seek to startPosition %v @ %v:%v", startPosition, binlogFile.Name(), position)
	}

	for {
		//position, _ := binlogFile.Seek(0, 1)
		written, err := io.CopyN(writer, binlogFile, blr.BinlogBlockSize)
		if err != nil && err != io.EOF {
			relog.Error("BinlogReader.serve err: %v", err)
			return
		}
		//relog.Info("BinlogReader copy @ %v:%v,%v", binlogFile.Name(), position, written)

		stats.Reads++
		stats.Bytes += written

		if written != blr.BinlogBlockSize {
			bufWriter.Flush()
			if _, statErr := os.Stat(nextLog); statErr == nil {
				//relog.Info("BinlogReader swap log file: %v", nextLog)
				// swap to next log file
				binlogFile.Close()
				binlogFile, nextLog = blr.open(nextLog)
				positionWaitStart = make(map[int64]time.Time)
				binlogFile.Seek(BINLOG_HEADER_SIZE, 0)
			} else {
				position, _ := binlogFile.Seek(0, 1)
				//relog.Info("BinlogReader %x wait for more data: %v:%v", stats.StartTime, binlogFile.Name(), position)
				// wait for more data
				time.Sleep(time.Duration(blr.LogWaitTimeout * 1e9))
				stats.Sleeps++
				now := time.Now()
				if lastSlept, ok := positionWaitStart[position]; ok {
					if (now.Sub(lastSlept)) > time.Duration(blr.MaxWaitTimeout*1e9) {
						relog.Error("MAX_WAIT_TIMEOUT exceeded, closing connection")
						return
					}
				} else {
					positionWaitStart[position] = now
				}
			}
		}
	}
}
