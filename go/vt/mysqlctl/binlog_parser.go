// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
package mysqlctl

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/vitess/go/relog"
	parser "code.google.com/p/vitess/go/vt/sqlparser"
)

/*
Functionality to parse a binlog/relay log and send event streams to clients.
*/

const (
	BLP_LINE_SIZE = 16 * 1024 * 1024
	MAX_TXN_BATCH = 1024
	DML           = "DML"
	DDL           = "DDL"
	BEGIN         = "BEGIN"
	COMMIT        = "COMMIT"
	USE           = "Use "
)

var sqlKwMap = map[string]string{
	"create":   DDL,
	"alter":    DDL,
	"drop":     DDL,
	"truncate": DDL,
	"rename":   DDL,
	"insert":   DML,
	"update":   DML,
	"delete":   DML,
	"begin":    BEGIN,
	"commit":   COMMIT,
}

var (
	STREAM_COMMENT_START   = []byte("/* _stream ")
	BINLOG_DELIMITER       = []byte("/*!*/;")
	BINLOG_POSITION_PREFIX = []byte("# at ")
	BINLOG_ROTATE_TO       = []byte("Rotate to ")
	BINLOG_BEGIN           = []byte("BEGIN")
	BINLOG_COMMIT          = []byte("COMMIT")
	BINLOG_ROLLBACK        = []byte("ROLLBACK")
	BINLOG_SET_TIMESTAMP   = []byte("SET TIMESTAMP=")
	BINLOG_SET_INSERT      = []byte("SET INSERT_ID=")
	BINLOG_END_LOG_POS     = []byte("end_log_pos ")
	BINLOG_XID             = []byte("Xid = ")
	BINLOG_START           = []byte("Start: binlog")
	BINLOG_DB_CHANGE       = []byte("Use ")
	POS                    = []byte(" pos: ")
	SPACE                  = []byte(" ")
)

type BinlogPosition struct {
	coordinates *ReplicationCoordinates
	Position    string
	Timestamp   int64
	Xid         uint64
	GroupId     uint64
}

//Api Interface
type UpdateResponse struct {
	Error string
	BinlogPosition
	EventData
}

type EventData struct {
	SqlType    string
	TableName  string
	Sql        string
	PkColNames []string
	PkValues   [][]interface{}
}

//Raw event buffer used to gather data during parsing.
type eventBuffer struct {
	BinlogPosition
	LogLine []byte
}

func NewEventBuffer(pos *BinlogPosition, line []byte) *eventBuffer {
	buf := &eventBuffer{}
	buf.LogLine = make([]byte, len(line))
	_ = copy(buf.LogLine, line)
	//The RelayPosition is never used, so using the default value
	buf.BinlogPosition.coordinates = &ReplicationCoordinates{RelayFilename: pos.coordinates.RelayFilename,
		MasterFilename: pos.coordinates.MasterFilename,
		MasterPosition: pos.coordinates.MasterPosition,
	}
	buf.BinlogPosition.Timestamp = pos.Timestamp
	return buf
}

type blpStats struct {
	TimeStarted      time.Time
	RotateEventCount uint16
	TxnCount         uint64
	DmlCount         uint64
	DdlCount         uint64
	LineCount        uint64
	BigLineCount     uint64
	BufferFullErrors uint64
	LinesPerSecond   float64
	TxnsPerSecond    float64
}

func (stats blpStats) DebugJsonString() string {
	elapsed := float64(time.Now().Sub(stats.TimeStarted)) / 1e9
	stats.LinesPerSecond = float64(stats.LineCount) / elapsed
	stats.TxnsPerSecond = float64(stats.TxnCount) / elapsed
	data, err := json.MarshalIndent(stats, "  ", "  ")
	if err != nil {
		return err.Error()
	}
	return string(data)
}

type Blp struct {
	nextStmtPosition uint64
	inTxn            bool
	txnLineBuffer    []*eventBuffer
	responseStream   []*UpdateResponse
	initialSeek      bool
	startPosition    *ReplicationCoordinates
	currentPosition  *BinlogPosition
	globalState      *UpdateStream
	blpStats
	dbmatch bool
}

func NewBlp(startCoordinates *ReplicationCoordinates, updateStream *UpdateStream) *Blp {
	blp := &Blp{}
	blp.startPosition = startCoordinates
	//The RelayPosition is never used, so using the default value
	currentCoord := NewReplicationCoordinates(startCoordinates.RelayFilename,
		0,
		startCoordinates.MasterFilename,
		startCoordinates.MasterPosition)
	blp.currentPosition = &BinlogPosition{coordinates: currentCoord}
	blp.inTxn = false
	blp.initialSeek = true
	blp.txnLineBuffer = make([]*eventBuffer, 0, MAX_TXN_BATCH)
	blp.responseStream = make([]*UpdateResponse, 0, MAX_TXN_BATCH)
	blp.globalState = updateStream
	//by default assume that the db matches.
	blp.dbmatch = true
	return blp
}

type BinlogParseError struct {
	Msg string
}

func NewBinlogParseError(msg string) *BinlogParseError {
	return &BinlogParseError{Msg: msg}
}

func (err BinlogParseError) Error() string {
	return err.Msg
}

type SendUpdateStreamResponse func(response interface{}) error

//Main entry function for reading and parsing the binlog.
func (blp *Blp) StreamBinlog(sendReply SendUpdateStreamResponse, binlogPrefix string) {
	var binlogReader io.Reader
	defer func() {
		reqIdentifier := fmt.Sprintf("%v:%v", blp.currentPosition.coordinates.MasterFilename, blp.currentPosition.coordinates.MasterPosition)
		if x := recover(); x != nil {
			serr, ok := x.(*BinlogParseError)
			if !ok {
				relog.Error("Uncaught panic for stream @ %v, err: %x ", reqIdentifier, x)
				//panic(x)
			}
			err := *serr
			relog.Error("StreamBinlog error @ %v, error: %v", reqIdentifier, err)
			SendError(sendReply, reqIdentifier, err, blp.currentPosition)
		}
	}()

	blr := NewBinlogReader(binlogPrefix)

	var blrReader, blrWriter *os.File
	var err, pipeErr error

	blrReader, blrWriter, pipeErr = os.Pipe()
	if pipeErr != nil {
		panic(NewBinlogParseError(pipeErr.Error()))
	}
	defer blrWriter.Close()

	go blp.getBinlogStream(blrWriter, blr)
	binlogReader, err = DecodeMysqlBinlog(blrReader)
	if err != nil {
		panic(NewBinlogParseError(err.Error()))
	}
	blp.parseBinlogEvents(sendReply, binlogReader)
}

//Main parse loop
func (blp *Blp) parseBinlogEvents(sendReply SendUpdateStreamResponse, binlogReader io.Reader) {
	// read over the stream and buffer up the transactions
	var err error
	var tmpLine, line []byte
	bigLine := make([]byte, 0, BLP_LINE_SIZE)
	// make the line as big as the max packet size - currently 16MB
	lineReader := bufio.NewReaderSize(binlogReader, BLP_LINE_SIZE)
	readAhead := false

	for {
		if !blp.globalState.isServiceEnabled() {
			panic(NewBinlogParseError("Disconnecting because the Update Stream service has been disabled"))
		}
		bigLine = bigLine[:0]
		tmpLine, err = blp.readBlpLine(lineReader, bigLine)
		if err != nil {
			if err == io.EOF {
				//end of stream
				relog.Info("encountered EOF, quitting")
				return
			}
			panic(NewBinlogParseError(fmt.Sprintf("ReadLine err: , %v", err)))
		}
		if readAhead {
			line = append(line, tmpLine...)
		} else {
			line = tmpLine
		}
		blp.LineCount++
		if len(line) == 0 {
			continue
		}

		if line[0] == '#' {
			//parse positional data
			blp.parsePositionData(line)
		} else {
			//parse event data

			//This is to accont for replicas where we seek to a point before the desired startPosition
			if blp.initialSeek && blp.globalState.usingRelayLogs && blp.nextStmtPosition < blp.startPosition.MasterPosition {
				continue
			}

			//Update Stream processes statements only for the dbname that it is subscribed to.
			blp.parseDbChange(line)
			if !blp.dbmatch {
				continue
			}

			readAhead = true
			if bytes.HasSuffix(line, BINLOG_DELIMITER) {
				readAhead = false
				line = line[:len(line)-len(BINLOG_DELIMITER)]
			}
			if !readAhead {
				blp.parseEventData(sendReply, line)
				line = line[:0]
			}
		}
	}
	//TODO: revisit printing/exporting of stats
	//relog.Info(blp.DebugJsonString())
}

//Function to set the dbmatch variable, this parses the "Use <dbname>" statement.
func (blp *Blp) parseDbChange(line []byte) {
	if !bytes.HasPrefix(line, BINLOG_DB_CHANGE) {
		return
	}
	if blp.globalState.dbname == "" {
		relog.Warning("Dbname is not set, will match all database names")
		return
	}

	new_db := string(bytes.TrimLeft(line, USE))
	if new_db != blp.globalState.dbname {
		blp.dbmatch = false
	}
}

func (blp *Blp) parsePositionData(line []byte) {
	if bytes.HasPrefix(line, BINLOG_POSITION_PREFIX) {
		//Master Position
		if blp.nextStmtPosition == 0 {
			return
		}
		blp.currentPosition.coordinates.MasterPosition = blp.nextStmtPosition
	} else if bytes.Index(line, BINLOG_ROTATE_TO) != -1 {
		blp.parseRotateEvent(line)
	} else if bytes.Index(line, BINLOG_END_LOG_POS) != -1 {
		//Ignore the position data that appears at the start line of binlog.
		if bytes.Index(line, BINLOG_START) != -1 {
			return
		}
		blp.parseMasterPosition(line)
	}
	if bytes.Index(line, BINLOG_XID) != -1 {
		blp.parseXid(line)
	}
}

func (blp *Blp) parseEventData(sendReply SendUpdateStreamResponse, line []byte) {
	if bytes.HasPrefix(line, BINLOG_SET_TIMESTAMP) {
		blp.extractEventTimestamp(line)
		blp.initialSeek = false
	} else if bytes.HasPrefix(line, BINLOG_BEGIN) {
		blp.handleBeginEvent(line)
	} else if bytes.HasPrefix(line, BINLOG_ROLLBACK) {
		blp.inTxn = false
		blp.txnLineBuffer = blp.txnLineBuffer[0:0]
	} else if len(line) > 0 {
		sqlType := getSqlType(line)
		if blp.inTxn {
			blp.handleTxnEvent(sendReply, line, sqlType)
		} else if sqlType == DDL {
			blp.handleDdlEvent(sendReply, line)
		}
	}
}

/*
Position Parsing Functions.
*/

func (blp *Blp) parseMasterPosition(line []byte) {
	var err error
	rem := bytes.Split(line, BINLOG_END_LOG_POS)
	masterPosStr := string(bytes.Split(rem[1], SPACE)[0])
	blp.nextStmtPosition, err = strconv.ParseUint(masterPosStr, 10, 64)
	if err != nil {
		panic(NewBinlogParseError(fmt.Sprintf("Error in extracting master position %v", err)))
	}
}

func (blp *Blp) parseXid(line []byte) {
	rem := bytes.Split(line, BINLOG_XID)
	xid, err := strconv.ParseUint(string(rem[1]), 10, 64)
	if err != nil {
		panic(NewBinlogParseError(fmt.Sprintf("Error in extracting Xid position %v", err)))
	}
	blp.currentPosition.Xid = xid
}

func (blp *Blp) extractEventTimestamp(line []byte) {
	currentTimestamp, err := strconv.ParseInt(string(line[len(BINLOG_SET_TIMESTAMP):]), 10, 64)
	if err != nil {
		panic(NewBinlogParseError(fmt.Sprintf("Error in extracting timestamp %v", err)))
	}
	blp.currentPosition.Timestamp = currentTimestamp
}

func (blp *Blp) parseRotateEvent(line []byte) {
	blp.RotateEventCount++
	rem := bytes.Split(line, BINLOG_ROTATE_TO)
	rem2 := bytes.Split(rem[1], POS)
	rotateFilename := strings.TrimSpace(string(rem2[0]))
	rotatePos, err := strconv.ParseUint(string(rem2[1]), 10, 64)
	if err != nil {
		panic(NewBinlogParseError(fmt.Sprintf("Error in extracting rotate pos %v from line %s", err, string(line))))
	}
	if !blp.globalState.usingRelayLogs {
		//If the file being parsed is a binlog,
		//then the rotate events only correspond to itself.
		blp.currentPosition.coordinates.MasterFilename = rotateFilename
		blp.currentPosition.coordinates.MasterPosition = rotatePos
	} else {
		//For relay logs, the rotate events could be that of relay log or the binlog,
		//the prefix of rotateFilename is used to test which case is it.
		logsDir, relayFile := path.Split(blp.currentPosition.coordinates.RelayFilename)
		currentPrefix := strings.Split(relayFile, ".")[0]
		rotatePrefix := strings.Split(rotateFilename, ".")[0]
		if currentPrefix == rotatePrefix {
			//relay log rotated
			blp.currentPosition.coordinates.RelayFilename = path.Join(logsDir, rotateFilename)
		} else {
			//master file rotated
			blp.currentPosition.coordinates.MasterFilename = rotateFilename
			blp.currentPosition.coordinates.MasterPosition = rotatePos
		}
	}
}

/*
Data event parsing and handling functions.
*/

func (blp *Blp) handleBeginEvent(line []byte) {
	if len(blp.txnLineBuffer) != 0 || blp.inTxn {
		panic(NewBinlogParseError(fmt.Sprintf("BEGIN encountered with non-empty trxn buffer, len: %d", len(blp.txnLineBuffer))))
	}
	blp.inTxn = true
	event := NewEventBuffer(blp.currentPosition, line)
	blp.txnLineBuffer = append(blp.txnLineBuffer, event)
}

func (blp *Blp) handleDdlEvent(sendReply SendUpdateStreamResponse, line []byte) {
	event := NewEventBuffer(blp.currentPosition, line)
	ddlStream := createDdlStream(event)
	buf := []*UpdateResponse{ddlStream}
	if err := sendStream(sendReply, buf); err != nil {
		panic(NewBinlogParseError(fmt.Sprintf("Error in sending event to client %v", err)))
	}
	blp.DdlCount++
}

func (blp *Blp) handleTxnEvent(sendReply SendUpdateStreamResponse, line []byte, sqlType string) {
	//AutoIncrement parsing
	if bytes.HasPrefix(line, BINLOG_SET_INSERT) {
		blp.extractAutoIncrementId(line)
	} else if bytes.HasPrefix(line, BINLOG_COMMIT) {
		blp.handleCommitEvent(sendReply, line)
	} else if sqlType == DML {
		blp.handleDmlEvent(line)
	}
}

func (blp *Blp) extractAutoIncrementId(line []byte) {
	event := NewEventBuffer(blp.currentPosition, line)
	blp.txnLineBuffer = append(blp.txnLineBuffer, event)
}

func (blp *Blp) handleCommitEvent(sendReply SendUpdateStreamResponse, line []byte) {
	if blp.globalState.usingRelayLogs {
		for !blp.slavePosBehindReplication() {
			rp := &ReplicationPosition{MasterLogFile: blp.currentPosition.coordinates.MasterFilename,
				MasterLogPosition: uint(blp.currentPosition.coordinates.MasterPosition)}
			if err := blp.globalState.mysqld.WaitMasterPos(rp, 30); err != nil {
				panic(NewBinlogParseError(fmt.Sprintf("Error in waiting for replication to catch up, %v", err)))
			}
		}
	}
	commitEvent := NewEventBuffer(blp.currentPosition, line)
	commitEvent.BinlogPosition.Xid = blp.currentPosition.Xid
	blp.txnLineBuffer = append(blp.txnLineBuffer, commitEvent)
	//txn block for DMLs, parse it and send events for a txn
	blp.responseStream = buildTxnResponse(blp.txnLineBuffer)

	//Only Begin and Commit events - no point in sending this.
	if len(blp.responseStream) <= 2 {
		blp.inTxn = false
		blp.txnLineBuffer = blp.txnLineBuffer[0:0]
		return
	}

	if err := sendStream(sendReply, blp.responseStream); err != nil {
		panic(NewBinlogParseError(fmt.Sprintf("Error in sending event to client %v", err)))
	}

	blp.inTxn = false
	blp.txnLineBuffer = blp.txnLineBuffer[0:0]
	blp.TxnCount += 1
}

func (blp *Blp) handleDmlEvent(line []byte) {
	if getDmlType(line) == "" {
		panic(NewBinlogParseError(fmt.Sprintf("Invalid dml or out of txn context %v", string(line))))
	}
	event := NewEventBuffer(blp.currentPosition, line)
	blp.txnLineBuffer = append(blp.txnLineBuffer, event)
	blp.DmlCount++
}

/*
Other utility functions.
*/

//This reads a binlog log line.
func (blp *Blp) readBlpLine(lineReader *bufio.Reader, bigLine []byte) (line []byte, err error) {
	for {
		tempLine, tempErr := lineReader.ReadSlice('\n')
		if tempErr == bufio.ErrBufferFull {
			blp.BufferFullErrors++
			bigLine = append(bigLine, tempLine...)
			continue
		} else if tempErr != nil {
			err = tempErr
		} else if len(bigLine) > 0 {
			line = bigLine[:len(bigLine)-1]
			blp.BigLineCount++
		} else {
			line = tempLine[:len(tempLine)-1]
		}
		break
	}
	return line, err
}

//This function streams the raw binlog.
func (blp *Blp) getBinlogStream(writer *os.File, blr *BinlogReader) {
	defer func() {
		if err := recover(); err != nil {
			relog.Error("getBinlogStream failed: %v", err)
		}
	}()
	if blp.globalState.usingRelayLogs {
		//we use RelayPosition for initial seek in case the caller has precise coordinates. But the code
		//is designed to primarily use RelayFilename, MasterFilename and MasterPosition to correctly start
		//streaming the logs if relay logs are being used.
		blr.ServeData(writer, path.Base(blp.startPosition.RelayFilename), int64(blp.startPosition.RelayPosition))
	} else {
		blr.ServeData(writer, blp.startPosition.MasterFilename, int64(blp.startPosition.MasterPosition))
	}
}

//This function determines whether streaming is behind replication as it should be.
func (blp *Blp) slavePosBehindReplication() bool {
	repl, err := blp.globalState.logMetadata.GetCurrentReplicationPosition()
	if err != nil {
		relog.Error(err.Error())
		panic(NewBinlogParseError(fmt.Sprintf("Error in obtaining current replication position %v", err)))
	}
	if repl.MasterFilename == blp.currentPosition.coordinates.MasterFilename {
		if blp.currentPosition.coordinates.MasterPosition <= repl.MasterPosition {
			return true
		}
	} else {
		replExt, err := strconv.ParseUint(strings.Split(repl.MasterFilename, ".")[1], 10, 64)
		if err != nil {
			relog.Error(err.Error())
			panic(NewBinlogParseError(fmt.Sprintf("Error in obtaining current replication position %v", err)))
		}
		parseExt, err := strconv.ParseUint(strings.Split(blp.currentPosition.coordinates.MasterFilename, ".")[1], 10, 64)
		if err != nil {
			relog.Error(err.Error())
			panic(NewBinlogParseError(fmt.Sprintf("Error in obtaining current replication position %v", err)))
		}
		if replExt >= parseExt {
			return true
		}
	}
	return false
}

//This builds UpdateResponse for each transaction.
func buildTxnResponse(trxnLineBuffer []*eventBuffer) (txnResponseList []*UpdateResponse) {
	var err error
	var line []byte
	var dmlType string
	var eventNodeTree *parser.Node
	var autoincId uint64

	for _, event := range trxnLineBuffer {
		line = event.LogLine
		if bytes.HasPrefix(line, BINLOG_BEGIN) {
			streamBuf := new(UpdateResponse)
			streamBuf.BinlogPosition = event.BinlogPosition
			streamBuf.BinlogPosition.Position, err = EncodeCoordinatesToPosition(event.BinlogPosition.coordinates)
			if err != nil {
				panic(NewBinlogParseError(fmt.Sprintf("Error in encoding the position %v", err)))
			}
			streamBuf.SqlType = BEGIN
			txnResponseList = append(txnResponseList, streamBuf)
			continue
		}
		if bytes.HasPrefix(line, BINLOG_COMMIT) {
			commitEvent := createCommitEvent(event)
			txnResponseList = append(txnResponseList, commitEvent)
			continue
		}
		if bytes.HasPrefix(line, BINLOG_SET_INSERT) {
			autoincId, err = strconv.ParseUint(string(line[len(BINLOG_SET_INSERT):]), 10, 64)
			if err != nil {
				panic(NewBinlogParseError(fmt.Sprintf("Error in extracting AutoInc Id %v", err)))
			}
			continue
		}

		dmlType = string(bytes.SplitN(line, []byte(" "), 2)[0])
		if dmlType == "" {
			relog.Error("Invalid DML line : %v", string(line))
			panic(NewBinlogParseError(fmt.Sprintf("Invalid DML, query %s", string(line))))
		}

		//valid dml
		commentIndex := bytes.Index(line, STREAM_COMMENT_START)
		//stream comment not found.
		if commentIndex == -1 {
			relog.Warning("Invalid DML - doesn't have a valid stream comment : %v", string(line))
			continue
			//FIXME: track such cases and potentially change them to errors at a later point.
			//panic(NewBinlogParseError(fmt.Sprintf("Invalid DML, doesn't have a valid stream comment. Sql: %v", string(line))))
		}
		//fmt.Println(lineTxn)
		streamComment := string(line[commentIndex+len(STREAM_COMMENT_START):])
		eventNodeTree = parseStreamComment(streamComment, autoincId)
		response := createUpdateResponse(eventNodeTree, dmlType, event.BinlogPosition)
		txnResponseList = append(txnResponseList, response)
		autoincId = 0
	}

	return txnResponseList
}

/*
Functions that parse the stream comment.
The _stream comment is extracted into a tree node with the following structure.
EventNode.Sub[0] table name
EventNode.Sub[1] Pk column names
EventNode.Sub[2:] Pk Value lists
*/
// Example query: insert into vtocc_e(foo) values ('foo') /* _stream vtocc_e (eid id name ) (null 1 'bmFtZQ==' ); */
// the "null" value is used for auto-increment columns.

//This parese a paricular pk tuple.
func parsePkTuple(tokenizer *parser.Tokenizer, autoincIdPtr *uint64) (pkTuple *parser.Node) {
	//pkTuple is a list of pk value Nodes
	pkTuple = parser.NewSimpleParseNode(parser.NODE_LIST, "")
	autoincId := *autoincIdPtr
	//start scanning the list
	for tempNode := tokenizer.Scan(); tempNode.Type != ')'; tempNode = tokenizer.Scan() {
		switch tempNode.Type {
		case parser.NULL:
			pkTuple.Push(parser.NewParseNode(parser.NUMBER, []byte(strconv.FormatUint(autoincId, 10))))
			autoincId++
		case '-':
			//handle negative numbers
			t2 := tokenizer.Scan()
			if t2.Type != parser.NUMBER {
				panic(NewBinlogParseError("Error in parsing stream comment: illegal construct, - followed by a non-number"))
			}
			t2.Value = append(tempNode.Value, t2.Value...)
			pkTuple.Push(t2)
		case parser.ID, parser.NUMBER:
			pkTuple.Push(tempNode)
		case parser.STRING:
			b := tempNode.Value
			decoded := make([]byte, base64.StdEncoding.DecodedLen(len(b)))
			numDecoded, err := base64.StdEncoding.Decode(decoded, b)
			if err != nil {
				panic(NewBinlogParseError("Error in base64 decoding pkValue"))
			}
			tempNode.Value = decoded[:numDecoded]
			pkTuple.Push(tempNode)
		default:
			panic(NewBinlogParseError(fmt.Sprintf("1 - Error in parsing stream comment: illegal node type %v %v", tempNode.Type, string(tempNode.Value))))
		}
	}
	return pkTuple
}

//This parses the stream comment.
func parseStreamComment(dmlComment string, autoincId uint64) (EventNode *parser.Node) {
	EventNode = parser.NewSimpleParseNode(parser.NODE_LIST, "")

	tokenizer := parser.NewStringTokenizer(dmlComment)
	var node *parser.Node
	var pkTuple *parser.Node

	node = tokenizer.Scan()
	if node.Type == parser.ID {
		//Table Name
		EventNode.Push(node)
	}

	for node = tokenizer.Scan(); node.Type != ';'; node = tokenizer.Scan() {
		switch node.Type {
		case '(':
			//pkTuple is a list of pk value Nodes
			pkTuple = parsePkTuple(tokenizer, &autoincId)
			EventNode.Push(pkTuple)
		default:
			panic(NewBinlogParseError(fmt.Sprintf("2 - Error in parsing stream comment: illegal node type %v %v", node.Type, string(node.Value))))
		}
	}

	return EventNode
}

//This builds UpdateResponse from the parsed tree, also handles a multi-row update.
func createUpdateResponse(eventTree *parser.Node, dmlType string, blpPos BinlogPosition) (response *UpdateResponse) {
	var err error
	if eventTree.Len() < 3 {
		panic(NewBinlogParseError(fmt.Sprintf("Invalid comment structure, len of tree %v", eventTree.Len())))
	}

	tableName := string(eventTree.At(0).Value)
	pkColNamesNode := eventTree.At(1)
	pkColNames := make([]string, 0, pkColNamesNode.Len())
	for _, pkCol := range pkColNamesNode.Sub {
		if pkCol.Type != parser.ID {
			panic(NewBinlogParseError("Error in the stream comment, illegal type for column name."))
		}
		pkColNames = append(pkColNames, string(pkCol.Value))
	}
	pkColLen := pkColNamesNode.Len()

	response = new(UpdateResponse)
	response.BinlogPosition = blpPos
	response.BinlogPosition.Position, err = EncodeCoordinatesToPosition(blpPos.coordinates)
	if err != nil {
		panic(NewBinlogParseError(fmt.Sprintf("Error in encoding the position %v", err)))
	}
	response.SqlType = dmlType
	response.TableName = tableName
	response.PkColNames = pkColNames
	response.PkValues = make([][]interface{}, len(eventTree.Sub[2:]))

	rowPk := make([]interface{}, pkColLen)
	for _, node := range eventTree.Sub[2:] {
		rowPk = rowPk[:0]
		if node.Len() != pkColLen {
			panic(NewBinlogParseError("Error in the stream comment, length of pk values doesn't match column names."))
		}
		rowPk = encodePkValues(node.Sub)
		response.PkValues = append(response.PkValues, rowPk)
	}
	return response
}

//Interprets the parsed node and correctly encodes the primary key values.
func encodePkValues(pkValues []*parser.Node) (rowPk []interface{}) {
	for _, pkVal := range pkValues {
		if pkVal.Type == parser.STRING {
			rowPk = append(rowPk, string(pkVal.Value))
		} else if pkVal.Type == parser.NUMBER {
			//pkVal.Value is a byte array, convert this to string and use strconv to find
			//the right numberic type.
			valstr := string(pkVal.Value)
			if ival, err := strconv.ParseInt(valstr, 0, 64); err == nil {
				rowPk = append(rowPk, ival)
			} else if uval, err := strconv.ParseUint(valstr, 0, 64); err == nil {
				rowPk = append(rowPk, uval)
			} else if fval, err := strconv.ParseFloat(valstr, 64); err == nil {
				rowPk = append(rowPk, fval)
			} else {
				panic(NewBinlogParseError(fmt.Sprintf("Error in encoding pkValues %v", err)))
			}
		} else {
			panic(NewBinlogParseError("Error in encoding pkValues - Unsupported type"))
		}
	}
	return rowPk
}

//This sends the stream to the client.
func sendStream(sendReply SendUpdateStreamResponse, responseBuf []*UpdateResponse) (err error) {
	for _, event := range responseBuf {
		err = sendReply(event)
		if err != nil {
			return NewBinlogParseError(fmt.Sprintf("Error in sending reply to client, %v", err))
		}
	}
	return nil
}

//This sends the error to the client.
func SendError(sendReply SendUpdateStreamResponse, reqIdentifier string, inputErr error, blpPos *BinlogPosition) {
	var err error
	streamBuf := new(UpdateResponse)
	streamBuf.Error = inputErr.Error()
	if blpPos != nil {
		streamBuf.BinlogPosition = *blpPos
		streamBuf.BinlogPosition.Position, err = EncodeCoordinatesToPosition(blpPos.coordinates)
		if err != nil {
			panic(NewBinlogParseError(fmt.Sprintf("Error in encoding the position %v", err)))
		}
	}
	buf := []*UpdateResponse{streamBuf}
	err = sendStream(sendReply, buf)
	if err != nil {
		relog.Error("Error in communicating message %v with the client: %v", inputErr, err)
	}
}

//This creates the response for COMMIT event.
func createCommitEvent(eventBuf *eventBuffer) (streamBuf *UpdateResponse) {
	var err error
	streamBuf = new(UpdateResponse)
	streamBuf.BinlogPosition = eventBuf.BinlogPosition
	streamBuf.BinlogPosition.Position, err = EncodeCoordinatesToPosition(eventBuf.BinlogPosition.coordinates)
	if err != nil {
		panic(NewBinlogParseError(fmt.Sprintf("Error in encoding the position %v", err)))
	}
	streamBuf.SqlType = COMMIT
	return
}

//This creates the response for DDL event.
func createDdlStream(lineBuffer *eventBuffer) (ddlStream *UpdateResponse) {
	var err error
	ddlStream = new(UpdateResponse)
	ddlStream.BinlogPosition = lineBuffer.BinlogPosition
	ddlStream.BinlogPosition.Position, err = EncodeCoordinatesToPosition(lineBuffer.BinlogPosition.coordinates)
	if err != nil {
		panic(NewBinlogParseError(fmt.Sprintf("Error in encoding the position %v", err)))
	}
	ddlStream.SqlType = DDL
	ddlStream.Sql = string(lineBuffer.LogLine)
	return ddlStream
}

func getSqlType(line []byte) string {
	line = bytes.TrimSpace(line)
	firstKw := string(bytes.SplitN(line, SPACE, 2)[0])
	sqlType, _ := sqlKwMap[firstKw]
	return sqlType
}

func getDmlType(line []byte) string {
	line = bytes.TrimSpace(line)
	dmlKw := string(bytes.SplitN(line, SPACE, 2)[0])
	sqlType, ok := sqlKwMap[dmlKw]
	if ok && sqlType == DML {
		return dmlKw
	}
	return ""
}
