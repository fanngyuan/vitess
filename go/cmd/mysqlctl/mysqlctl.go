// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"code.google.com/p/vitess/go/relog"
	"code.google.com/p/vitess/go/vt/dbconfigs"
	"code.google.com/p/vitess/go/vt/key"
	"code.google.com/p/vitess/go/vt/mysqlctl"
)

var port = flag.Int("port", 6612, "vtocc port")
var mysqlPort = flag.Int("mysql-port", 3306, "mysql port")
var tabletUid = flag.Uint("tablet-uid", 41983, "tablet uid")
var logLevel = flag.String("log.level", "WARNING", "set log level")
var tabletAddr string

func initCmd(mysqld *mysqlctl.Mysqld, subFlags *flag.FlagSet, args []string) {
	waitTime := subFlags.Duration("wait-time", mysqlctl.MysqlWaitTime, "how long to wait for startup")
	subFlags.Parse(args)

	if err := mysqlctl.Init(mysqld, *waitTime); err != nil {
		relog.Fatal("failed init mysql: %v", err)
	}
}

func partialRestoreCmd(mysqld *mysqlctl.Mysqld, subFlags *flag.FlagSet, args []string) {
	fetchConcurrency := subFlags.Int("fetch-concurrency", 3, "how many files to fetch simultaneously")
	fetchRetryCount := subFlags.Int("fetch-retry-count", 3, "how many times to retyr a failed transfer")
	subFlags.Parse(args)
	if subFlags.NArg() != 1 {
		relog.Fatal("Command partialrestore requires <split snapshot manifest file>")
	}

	rs, err := mysqlctl.ReadSplitSnapshotManifest(subFlags.Arg(0))
	if err == nil {
		err = mysqld.RestoreFromPartialSnapshot(rs, *fetchConcurrency, *fetchRetryCount)
	}
	if err != nil {
		relog.Fatal("partialrestore failed: %v", err)
	}
}

func partialSnapshotCmd(mysqld *mysqlctl.Mysqld, subFlags *flag.FlagSet, args []string) {
	start := subFlags.String("start", "", "start of the key range")
	end := subFlags.String("end", "", "end of the key range")
	concurrency := subFlags.Int("concurrency", 3, "how many compression jobs to run simultaneously")
	subFlags.Parse(args)
	if subFlags.NArg() != 2 {
		relog.Fatal("action partialsnapshot requires <db name> <key name>")
	}

	filename, err := mysqld.CreateSplitSnapshot(subFlags.Arg(0), subFlags.Arg(1), key.HexKeyspaceId(*start), key.HexKeyspaceId(*end), tabletAddr, false, *concurrency)
	if err != nil {
		relog.Fatal("partialsnapshot failed: %v", err)
	} else {
		relog.Info("manifest location: %v", filename)
	}
}

func restoreCmd(mysqld *mysqlctl.Mysqld, subFlags *flag.FlagSet, args []string) {
	dontWaitForSlaveStart := subFlags.Bool("dont-wait-for-slave-start", false, "won't wait for replication to start (useful when restoring from master server)")
	fetchConcurrency := subFlags.Int("fetch-concurrency", 3, "how many files to fetch simultaneously")
	fetchRetryCount := subFlags.Int("fetch-retry-count", 3, "how many times to retyr a failed transfer")
	subFlags.Parse(args)
	if subFlags.NArg() != 1 {
		relog.Fatal("Command restore requires <snapshot manifest file>")
	}

	rs, err := mysqlctl.ReadSnapshotManifest(subFlags.Arg(0))
	if err == nil {
		err = mysqld.RestoreFromSnapshot(rs, *fetchConcurrency, *fetchRetryCount, *dontWaitForSlaveStart)
	}
	if err != nil {
		relog.Fatal("restore failed: %v", err)
	}
}

func shutdownCmd(mysqld *mysqlctl.Mysqld, subFlags *flag.FlagSet, args []string) {
	waitTime := subFlags.Duration("wait-time", mysqlctl.MysqlWaitTime, "how long to wait for shutdown")
	subFlags.Parse(args)

	if mysqlErr := mysqlctl.Shutdown(mysqld, true, *waitTime); mysqlErr != nil {
		relog.Fatal("failed shutdown mysql: %v", mysqlErr)
	}
}

func snapshotCmd(mysqld *mysqlctl.Mysqld, subFlags *flag.FlagSet, args []string) {
	concurrency := subFlags.Int("concurrency", 3, "how many compression jobs to run simultaneously")
	subFlags.Parse(args)
	if subFlags.NArg() != 1 {
		relog.Fatal("Command snapshot requires <db name>")
	}

	filename, _, _, err := mysqld.CreateSnapshot(subFlags.Arg(0), tabletAddr, false, *concurrency, false)
	if err != nil {
		relog.Fatal("snapshot failed: %v", err)
	} else {
		relog.Info("manifest location: %v", filename)
	}
}

func snapshotSourceStartCmd(mysqld *mysqlctl.Mysqld, subFlags *flag.FlagSet, args []string) {
	concurrency := subFlags.Int("concurrency", 3, "how many checksum jobs to run simultaneously")
	subFlags.Parse(args)
	if subFlags.NArg() != 1 {
		relog.Fatal("Command snapshotsourcestart requires <db name>")
	}

	filename, slaveStartRequired, readOnly, err := mysqld.CreateSnapshot(subFlags.Arg(0), tabletAddr, false, *concurrency, true)
	if err != nil {
		relog.Fatal("snapshot failed: %v", err)
	} else {
		relog.Info("manifest location: %v", filename)
		relog.Info("slave start required: %v", slaveStartRequired)
		relog.Info("read only: %v", readOnly)
	}
}

func snapshotSourceEndCmd(mysqld *mysqlctl.Mysqld, subFlags *flag.FlagSet, args []string) {
	slaveStartRequired := subFlags.Bool("slave-start", false, "will restart replication")
	readWrite := subFlags.Bool("read-write", false, "will make the server read-write")
	subFlags.Parse(args)

	err := mysqld.SnapshotSourceEnd(*slaveStartRequired, !(*readWrite))
	if err != nil {
		relog.Fatal("snapshotsourceend failed: %v", err)
	}
}

func startCmd(mysqld *mysqlctl.Mysqld, subFlags *flag.FlagSet, args []string) {
	waitTime := subFlags.Duration("wait-time", mysqlctl.MysqlWaitTime, "how long to wait for startup")
	subFlags.Parse(args)

	if err := mysqlctl.Start(mysqld, *waitTime); err != nil {
		relog.Fatal("failed start mysql: %v", err)
	}
}

func teardownCmd(mysqld *mysqlctl.Mysqld, subFlags *flag.FlagSet, args []string) {
	force := subFlags.Bool("force", false, "will remove the root directory even if mysqld shutdown fails")
	subFlags.Parse(args)

	if err := mysqlctl.Teardown(mysqld, *force); err != nil {
		relog.Fatal("failed teardown mysql (forced? %v): %v", *force, err)
	}
}

type command struct {
	name   string
	method func(*mysqlctl.Mysqld, *flag.FlagSet, []string)
	params string
	help   string
}

var commands = []command{
	command{"init", initCmd, "",
		"Initalizes the directory structure and starts mysqld"},
	command{"teardown", teardownCmd, "[-force]",
		"Shuts mysqld down, and removes the directory"},

	command{"start", startCmd, "[-wait-time=20s]",
		"Starts mysqld on an already 'init'-ed directory"},
	command{"shutdown", shutdownCmd, "[-wait-time=20s]",
		"Shuts down mysqld, does not remove any file"},

	command{"snapshot", snapshotCmd,
		"[-concurrency=3] <db name>",
		"Takes a full snapshot, copying the innodb data files"},
	command{"snapshotsourcestart", snapshotSourceStartCmd,
		"[-concurrency=3] <db name>",
		"Enters snapshot server mode (mysqld stopped, serving innodb data files)"},
	command{"snapshotsourceend", snapshotSourceEndCmd,
		"[-slave-start] [-read-write]",
		"Gets out of snapshot server mode"},
	command{"restore", restoreCmd,
		"[-fetch-concurrency=3] [-fetch-retry-count=3] [-dont-wait-for-slave-start] <snapshot manifest file>",
		"Restores a full snapshot"},

	command{"partialsnapshot", partialSnapshotCmd,
		"[-start=<start key>] [-stop=<stop key>] [-concurrency=3] <db name> <key name>",
		"Takes a partial snapshot using 'select * into' commands"},
	command{"partialrestore", partialRestoreCmd,
		"[-fetch-concurrency=3] [-fetch-retry-count=3] <split snapshot manifest file>",
		"Restores a database from a partial snapshot"},
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [global parameters] command [command parameters]\n", os.Args[0])

		fmt.Fprintf(os.Stderr, "\nThe global optional parameters are:\n")
		flag.PrintDefaults()

		fmt.Fprintf(os.Stderr, "\nThe commands are listed below. Use '%s <command> -h' for more help.\n\n", os.Args[0])
		for _, cmd := range commands {
			fmt.Fprintf(os.Stderr, "  %s", cmd.name)
			if cmd.params != "" {
				fmt.Fprintf(os.Stderr, " %s", cmd.params)
			}
			fmt.Fprintf(os.Stderr, "\n")
		}
		fmt.Fprintf(os.Stderr, "\n")
	}
	dbConfigsFile, dbCredentialsFile := dbconfigs.RegisterCommonFlags()
	flag.Parse()

	logger := relog.New(os.Stderr, "",
		log.Ldate|log.Lmicroseconds|log.Lshortfile,
		relog.LogNameToLogLevel(*logLevel))
	relog.SetLogger(logger)

	tabletAddr = fmt.Sprintf("%v:%v", "localhost", *port)
	mycnf := mysqlctl.NewMycnf(uint32(*tabletUid), *mysqlPort, mysqlctl.VtReplParams{})
	dbcfgs, err := dbconfigs.Init(mycnf.SocketFile, *dbConfigsFile, *dbCredentialsFile)
	if err != nil {
		relog.Fatal("%v", err)
	}
	mysqld := mysqlctl.NewMysqld(mycnf, dbcfgs.Dba, dbcfgs.Repl)

	action := flag.Arg(0)
	for _, cmd := range commands {
		if cmd.name == action {
			subFlags := flag.NewFlagSet(action, flag.ExitOnError)
			subFlags.Usage = func() {
				fmt.Fprintf(os.Stderr, "Usage: %s %s %s\n\n", os.Args[0], cmd.name, cmd.params)
				fmt.Fprintf(os.Stderr, "%s\n\n", cmd.help)
				subFlags.PrintDefaults()
			}

			cmd.method(mysqld, subFlags, flag.Args()[1:])
			return
		}
	}
	relog.Fatal("invalid action: %v", action)
}
