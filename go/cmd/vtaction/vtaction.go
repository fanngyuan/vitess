// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"expvar"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/rpc"
	"os"

	"code.google.com/p/vitess/go/relog"
	"code.google.com/p/vitess/go/rpcwrap/bsonrpc"
	"code.google.com/p/vitess/go/rpcwrap/jsonrpc"
	_ "code.google.com/p/vitess/go/snitch"
	"code.google.com/p/vitess/go/zk"

	"code.google.com/p/vitess/go/vt/mysqlctl"
	"code.google.com/p/vitess/go/vt/tabletmanager"
)

var port = flag.Int("port", 0, "port for debug http server")
var action = flag.String("action", "", "management action to perform")
var actionNode = flag.String("action-node", "",
	"path to zk node representing the action")
var actionGuid = flag.String("action-guid", "",
	"a label to help track processes")
var logLevel = flag.String("log.level", "debug", "set log level")
var logFilename = flag.String("logfile", "/dev/stderr", "log path")

// FIXME(msolomon) temporary, until we are starting mysql ourselves
var mycnfFile = flag.String("mycnf-file", "/etc/my.cnf", "path to my.cnf")

func init() {
	expvar.NewString("binary-name").Set("vtaction")
}

func main() {
	flag.Parse()

	rpc.HandleHTTP()
	jsonrpc.ServeHTTP()
	jsonrpc.ServeRPC()
	bsonrpc.ServeHTTP()
	bsonrpc.ServeRPC()

	logFile, err := os.OpenFile(*logFilename,
		os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		panic(err)
	}
	logger := relog.New(logFile, fmt.Sprintf("vtaction [%v] ", os.Getpid()),
		log.Ldate|log.Lmicroseconds|log.Lshortfile,
		relog.LogNameToLogLevel(*logLevel))
	relog.SetLogger(logger)
	relog.Info("started vtaction %v", os.Args)

	// we delegate out startup to the micromanagement server so these actions
	// will occur after we have obtained our socket.
	bindAddr := fmt.Sprintf(":%v", *port)
	httpServer := &http.Server{Addr: bindAddr}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil {
			relog.Error("httpServer.ListenAndServe err: %v", err)
		}
	}()

	mycnf, mycnfErr := mysqlctl.ReadMycnf(*mycnfFile)
	if mycnfErr != nil {
		relog.Fatal("mycnf read failed: %v", mycnfErr)
	}
	dbaconfig := map[string]interface{}{
		"uname":       "vt_dba",
		"unix_socket": mycnf.SocketFile,
		"pass":        "",
		"dbname":      "",
		"charset":     "utf8",
		"host":        "",
		"port":        0,
	}
	mysqld := mysqlctl.NewMysqld(mycnf, dbaconfig)

	zconn := zk.NewMetaConn(5e9)
	defer zconn.Close()
	actor := tabletmanager.NewTabletActor(mysqld, zconn)

	actionErr := actor.HandleAction(*actionNode, *action, *actionGuid)
	if actionErr != nil {
		relog.Fatal("action error: %v", actionErr)
	}

	relog.Info("finished vtaction %v", os.Args)
}
