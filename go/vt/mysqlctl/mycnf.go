// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
  Generate my.cnf files from templates.
*/

package mysqlctl

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

type Mycnf struct {
	ServerId              uint32
	MysqlPort             int
	DataDir               string
	InnodbDataHomeDir     string
	InnodbLogGroupHomeDir string
	SocketFile            string
	StartKey              string
	EndKey                string
	ErrorLogPath          string
	SlowLogPath           string
	RelayLogPath          string
	RelayLogIndexPath     string
	RelayLogInfoPath      string
	BinLogPath            string
	BinLogIndexPath       string
	MasterInfoFile        string
	PidFile               string
	TmpDir                string
	SlaveLoadTmpDir       string
	mycnfMap              map[string]string
}

func (cnf *Mycnf) lookupAndCheck(key string) string {
	key = normKey([]byte(key))
	val := cnf.mycnfMap[key]
	if val == "" {
		panic(fmt.Errorf("Value for key '%v' not set", key))
	}
	return val
}

func (cnf *Mycnf) MysqlAddr() string {
	return fmt.Sprintf("%v:%v", fqdn(), cnf.MysqlPort)
}

func fqdn() string {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	cname, err := net.LookupCNAME(hostname)
	if err != nil {
		panic(err)
	}
	return strings.TrimRight(cname, ".")
}

func normKey(bkey []byte) string {
	return string(bytes.TrimSpace(bkey))
	// FIXME(msolomon) People are careless about hyphen vs underscore - we should normalize.
	//return string(bytes.Replace(bytes.TrimSpace(bkey), []byte("-"), []byte("_"), -1))
}

func ReadMycnf(cnfFile string) (mycnf *Mycnf, err error) {
	defer func(err *error) {
		if x := recover(); x != nil {
			*err = x.(error)
		}
	}(&err)

	f, err := os.Open(cnfFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := bufio.NewReader(f)
	mycnf = new(Mycnf)
	mycnf.mycnfMap = make(map[string]string)
	var lval, rval string
	var parts [][]byte

	for {
		line, _, err := buf.ReadLine()
		if err == io.EOF {
			break
		}
		line = bytes.TrimSpace(line)

		parts = bytes.Split(line, []byte("="))
		if len(parts) < 2 {
			continue
		}
		lval = normKey(parts[0])
		rval = string(bytes.TrimSpace(parts[1]))
		mycnf.mycnfMap[lval] = rval
	}

	serverIdStr := mycnf.lookupAndCheck("server-id")
	serverId, err := strconv.Atoi(serverIdStr)
	if err != nil {
		panic(fmt.Errorf("Failed to convert server-id %v", err))
	}
	mycnf.ServerId = uint32(serverId)

	portStr := mycnf.lookupAndCheck("port")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		panic(fmt.Errorf("Failed: failed to convert port %v", err))
	}
	mycnf.MysqlPort = port
	mycnf.DataDir = mycnf.lookupAndCheck("datadir")
	mycnf.InnodbDataHomeDir = mycnf.lookupAndCheck("innodb_data_home_dir")
	mycnf.InnodbLogGroupHomeDir = mycnf.lookupAndCheck("innodb_log_group_home_dir")
	mycnf.SocketFile = mycnf.lookupAndCheck("socket")
	mycnf.ErrorLogPath = mycnf.lookupAndCheck("log-error")
	mycnf.SlowLogPath = mycnf.lookupAndCheck("slow-query-log-file")
	mycnf.RelayLogPath = mycnf.lookupAndCheck("relay-log")
	mycnf.RelayLogIndexPath = mycnf.lookupAndCheck("relay-log-index")
	mycnf.RelayLogInfoPath = mycnf.lookupAndCheck("relay-log-info-file")
	mycnf.BinLogPath = mycnf.lookupAndCheck("log-bin")
	mycnf.BinLogIndexPath = mycnf.lookupAndCheck("log-bin-index")
	mycnf.MasterInfoFile = mycnf.lookupAndCheck("master-info-file")
	mycnf.PidFile = mycnf.lookupAndCheck("pid-file")
	//These values are currently not being set, hence not checking them.
	mycnf.StartKey = mycnf.mycnfMap["vt_shard_key_range_start"]
	mycnf.EndKey = mycnf.mycnfMap["vt_shard_key_range_end"]

	return mycnf, nil
}
