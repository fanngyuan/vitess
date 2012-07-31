// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	_ "net/http/pprof"
	"net/rpc"
	"os"
	"syscall"

	"code.google.com/p/vitess/go/relog"
	"code.google.com/p/vitess/go/rpcwrap/bsonrpc"
	"code.google.com/p/vitess/go/rpcwrap/jsonrpc"
	"code.google.com/p/vitess/go/sighandler"
	_ "code.google.com/p/vitess/go/snitch"
	"code.google.com/p/vitess/go/umgmt"
	"code.google.com/p/vitess/go/vt/servenv"
	ts "code.google.com/p/vitess/go/vt/tabletserver"
)

const (
	DefaultLameDuckPeriod = 30.0
	DefaultRebindDelay    = 0.0
)

var (
	port = flag.Int("port", 6510, "tcp port to serve on")
	lameDuckPeriod = flag.Float64("lame-duck-period", DefaultLameDuckPeriod,
		"how long to give in-flight transactions to finish")
	rebindDelay = flag.Float64("rebind-delay", DefaultRebindDelay,
		"artificial delay before rebinding a hijacked listener")
	configFile   = flag.String("config", "", "config file name")
	dbConfigFile = flag.String("dbconfig", "", "db config file name")
	queryLog     = flag.String("querylog", "",
		"for testing: log all queries to this file")
)

var config ts.Config = ts.Config {
	1000,
	16,
	20,
	30,
	10000,
	5000,
	30 * 60,
	0,
	30 * 60,
}

var dbconfig map[string]interface{} = map[string]interface{}{
	"host":        "localhost",
	"port":        0,
	"unix_socket": "",
	"uname":       "vt_app",
	"pass":        "",
	"dbname":      "",
	"charset":     "utf8",
}

func main() {
	flag.Parse()
	env.Init("vtocc")

	if *queryLog != "" {
		if f, err := os.OpenFile(*queryLog, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644); err == nil {
			ts.QueryLogger = relog.New(f, "", log.Ldate|log.Lmicroseconds, relog.DEBUG)
		} else {
			relog.Fatal("Error opening file %v: %v", *queryLog, err)
		}
	}
	unmarshalFile(*configFile, &config)
	unmarshalFile(*dbConfigFile, &dbconfig)
	// work-around for jsonism
	if v, ok := dbconfig["port"].(float64); ok {
		dbconfig["port"] = int(v)
	}
	qm := &OccManager{config, dbconfig}
	rpc.Register(qm)
	ts.StartQueryService(config)
	ts.AllowQueries(dbconfig)

	rpc.HandleHTTP()
	jsonrpc.ServeHTTP()
	jsonrpc.ServeRPC()
	bsonrpc.ServeHTTP()
	bsonrpc.ServeRPC()
	relog.Info("started vtocc %v", *port)

	// we delegate out startup to the micromanagement server so these actions
	// will occur after we have obtained our socket.
	usefulLameDuckPeriod := float64(config.QueryTimeout + 1)
	if usefulLameDuckPeriod > *lameDuckPeriod {
		*lameDuckPeriod = usefulLameDuckPeriod
		relog.Info("readjusted -lame-duck-period to %f", *lameDuckPeriod)
	}
	umgmt.SetLameDuckPeriod(float32(*lameDuckPeriod))
	umgmt.SetRebindDelay(float32(*rebindDelay))
	umgmt.AddStartupCallback(func() {
		umgmt.StartHttpServer(fmt.Sprintf(":%v", *port))
	})
	umgmt.AddStartupCallback(func() {
		sighandler.SetSignalHandler(syscall.SIGTERM, umgmt.SigTermHandler)
	})
	umgmt.AddCloseCallback(func() {
		ts.DisallowQueries()
	})

	umgmtSocket := fmt.Sprintf("/tmp/vtocc-%08x-umgmt.sock", *port)
	if umgmtErr := umgmt.ListenAndServe(umgmtSocket); umgmtErr != nil {
		relog.Error("umgmt.ListenAndServe err: %v", umgmtErr)
	}
	relog.Info("done")
}

func unmarshalFile(name string, val interface{}) {
	if name != "" {
		data, err := ioutil.ReadFile(name)
		if err != nil {
			relog.Fatal("could not read %v: %v", val, err)
		}
		if err = json.Unmarshal(data, val); err != nil {
			relog.Fatal("could not read %s: %v", val, err)
		}
	}
	data, _ := json.MarshalIndent(val, "", "  ")
	relog.Info("config: %s\n", data)
}

// OccManager is deprecated. Use SqlQuery.GetSessionId instead.
type OccManager struct {
	config   ts.Config
	dbconfig map[string]interface{}
}

func (self *OccManager) GetSessionId(dbname *string, sessionId *int64) error {
	if *dbname != self.dbconfig["dbname"].(string) {
		return errors.New(fmt.Sprintf("db name mismatch, expecting %v, received %v",
			self.dbconfig["dbname"].(string), *dbname))
	}
	*sessionId = ts.GetSessionId()
	return nil
}
