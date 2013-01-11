// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mysqlctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	"code.google.com/p/vitess/go/ioutil2"
	"code.google.com/p/vitess/go/relog"
	"code.google.com/p/vitess/go/vt/hook"
)

// These methods deal with cloning a running instance of mysql.

const (
	maxLagSeconds = 5
)

const (
	SnapshotManifestFile = "snapshot_manifest.json"
)

// Validate that this instance is a reasonable source of data.
func (mysqld *Mysqld) validateCloneSource(serverMode bool) error {
	// NOTE(msolomon) Removing this check for now - I don't see the value of validating this.
	// // needs to be master, or slave that's not too far behind
	// slaveStatus, err := mysqld.slaveStatus()
	// if err != nil {
	// 	if err != ErrNotSlave {
	// 		return fmt.Errorf("mysqlctl: validateCloneSource failed, %v", err)
	// 	}
	// } else {
	// 	lagSeconds, _ := strconv.Atoi(slaveStatus["seconds_behind_master"])
	// 	if lagSeconds > maxLagSeconds {
	// 		return fmt.Errorf("mysqlctl: validateCloneSource failed, lag_seconds exceed maximum tolerance (%v)", lagSeconds)
	// 	}
	// }

	// make sure we can write locally
	if err := mysqld.ValidateSnapshotPath(); err != nil {
		return err
	}

	// run a hook to check local things
	// FIXME(alainjobart) What other parameters do we have to
	// provide? dbname, host, socket?
	params := make(map[string]string)
	if serverMode {
		params["server-mode"] = ""
	}
	if err := hook.NewHook("preflight_snapshot", params).ExecuteOptional(); err != nil {
		return err
	}

	// FIXME(msolomon) check free space based on an estimate of the current
	// size of the db files.
	// Also, check that we aren't already cloning/compressing or acting as a
	// source. Mysqld being down isn't enough, presumably that will be
	// restarted as soon as the snapshot is taken.
	return nil
}

func (mysqld *Mysqld) validateCloneTarget() error {
	// run a hook to check local things
	// FIXME(alainjobart) What other parameters do we have to
	// provide? dbname, host, socket?
	if err := hook.NewSimpleHook("preflight_restore").ExecuteOptional(); err != nil {
		return err
	}

	rows, err := mysqld.fetchSuperQuery("SHOW DATABASES")
	if err != nil {
		return fmt.Errorf("mysqlctl: validateCloneTarget failed, %v", err)
	}

	for _, row := range rows {
		if strings.HasPrefix(row[0].String(), "vt_") {
			dbName := row[0].String()
			tableRows, err := mysqld.fetchSuperQuery("SHOW TABLES FROM " + dbName)
			if err != nil {
				return fmt.Errorf("mysqlctl: validateCloneTarget failed, %v", err)
			} else if len(tableRows) == 0 {
				// no tables == empty db, all is well
				continue
			}
			return fmt.Errorf("mysqlctl: validateCloneTarget failed, found active db %v", dbName)
		}
	}

	return nil
}

func findFilesToServe(srcDir, dstDir string, compress bool) ([]string, []string, error) {
	fiList, err := ioutil.ReadDir(srcDir)
	if err != nil {
		return nil, nil, err
	}
	sources := make([]string, 0, len(fiList))
	destinations := make([]string, 0, len(fiList))
	for _, fi := range fiList {
		if !fi.IsDir() {
			srcPath := path.Join(srcDir, fi.Name())
			var dstPath string
			if compress {
				dstPath = path.Join(dstDir, fi.Name()+".gz")
			} else {
				dstPath = path.Join(dstDir, fi.Name())
			}
			sources = append(sources, srcPath)
			destinations = append(destinations, dstPath)
		}
	}
	return sources, destinations, nil
}

func (mysqld *Mysqld) FindVtDatabases() ([]string, error) {
	fiList, err := ioutil.ReadDir(mysqld.config.DataDir)
	if err != nil {
		return nil, err
	}

	dbNames := make([]string, 0, 16)
	for _, fi := range fiList {
		if strings.HasSuffix(fi.Name(), "vt_") {
			dbNames = append(dbNames, fi.Name())
		}
	}
	return dbNames, nil
}

func (mysqld *Mysqld) createSnapshot(snapshotPath string, concurrency int, serverMode bool) ([]SnapshotFile, error) {
	sources := make([]string, 0, 128)
	destinations := make([]string, 0, 128)

	// clean out and start fresh
	relog.Info("removing previous snapshots: %v", snapshotPath)
	if err := os.RemoveAll(snapshotPath); err != nil {
		return nil, err
	}

	// FIXME(msolomon) innodb paths must match patterns in mycnf -
	// probably belongs as a derived path.
	type snapPair struct{ srcDir, dstDir string }
	dps := []snapPair{
		{mysqld.config.InnodbDataHomeDir, path.Join(snapshotPath, innodbDataSubdir)},
		{mysqld.config.InnodbLogGroupHomeDir, path.Join(snapshotPath, innodbLogSubdir)},
	}

	dataDirEntries, err := ioutil.ReadDir(mysqld.config.DataDir)
	if err != nil {
		return nil, err
	}

	for _, de := range dataDirEntries {
		if de.IsDir() {
			// Copy anything that defines a db.opt file - that includes empty databases.
			dbDirPath := path.Join(mysqld.config.DataDir, de.Name())
			_, err := os.Stat(path.Join(dbDirPath, "db.opt"))
			if err == nil {
				dps = append(dps, snapPair{dbDirPath, path.Join(snapshotPath, dataDir, de.Name())})
			} else {
				// Look for at least one .frm file
				dbDirEntries, err := ioutil.ReadDir(dbDirPath)
				if err == nil {
					for _, dbEntry := range dbDirEntries {
						if strings.HasSuffix(dbEntry.Name(), ".frm") {
							dps = append(dps, snapPair{dbDirPath, path.Join(snapshotPath, dataDir, de.Name())})
							break
						}
					}
				} else {
					relog.Warning("unable to scan db dir: %v", err)
				}
			}
		}
	}

	for _, dp := range dps {
		if err := os.MkdirAll(dp.dstDir, 0775); err != nil {
			return nil, err
		}
		if s, d, err := findFilesToServe(dp.srcDir, dp.dstDir, !serverMode); err != nil {
			return nil, err
		} else {
			sources = append(sources, s...)
			destinations = append(destinations, d...)
		}
	}

	return newSnapshotFiles(sources, destinations, mysqld.SnapshotDir, concurrency, !serverMode)
}

// This function runs on the machine acting as the source for the clone.
//
// Check master/slave status and determine restore needs.
// If this instance is a slave, stop replication, otherwise place in read-only mode.
// Record replication position.
// Shutdown mysql
// Check paths for storing data
//
// Depending on the serverMode flag, we do the following:
// serverMode = false:
//   Compress /vt/vt_[0-9a-f]+/data/vt_.+
//   Compute md5() sums (of compressed files)
//   Place in /vt/clone_src where they will be served by http server (not rpc)
//   Restart mysql
// serverMode = true:
//   Make symlinks for /vt/vt_[0-9a-f]+/data/vt_.+ to innodb files
//   Compute md5() sums (of uncompressed files)
//   Place symlinks in /vt/clone_src where they will be served by http server
//   Leave mysql stopped, return slaveStartRequired, readOnly
func (mysqld *Mysqld) CreateSnapshot(dbName, sourceAddr string, allowHierarchicalReplication bool, concurrency int, serverMode bool) (snapshotManifestUrlPath string, slaveStartRequired, readOnly bool, err error) {
	if dbName == "" {
		return "", false, false, errors.New("CreateSnapshot failed: no database name provided")
	}

	if err = mysqld.validateCloneSource(serverMode); err != nil {
		return
	}

	// save initial state so we can restore on Start()
	slaveStartRequired = false
	sourceIsMaster := false
	readOnly = true

	slaveStatus, slaveErr := mysqld.slaveStatus()
	if slaveErr == nil {
		slaveStartRequired = (slaveStatus["Slave_IO_Running"] == "Yes" && slaveStatus["Slave_SQL_Running"] == "Yes")
	} else if slaveErr == ErrNotSlave {
		sourceIsMaster = true
	} else {
		// If we can't get any data, just fail.
		return
	}

	readOnly, err = mysqld.IsReadOnly()
	if err != nil {
		return
	}

	// Stop sources of writes so we can get a consistent replication position.
	// If the source is a slave use the master replication position
	// unless we are allowing hierachical replicas.
	masterAddr := ""
	var replicationPosition *ReplicationPosition
	if sourceIsMaster {
		if err = mysqld.SetReadOnly(true); err != nil {
			return
		}
		replicationPosition, err = mysqld.MasterStatus()
		if err != nil {
			return
		}
		masterAddr = mysqld.Addr()
	} else {
		if err = mysqld.StopSlave(); err != nil {
			return
		}
		replicationPosition, err = mysqld.SlaveStatus()
		if err != nil {
			return
		}
		// We are a slave, check our replication strategy before
		// choosing the master address.
		if allowHierarchicalReplication {
			masterAddr = mysqld.Addr()
		} else {
			masterAddr, err = mysqld.GetMasterAddr()
			if err != nil {
				return
			}
		}
	}

	if err = Shutdown(mysqld, true, MysqlWaitTime); err != nil {
		return
	}

	var smFile string
	dataFiles, snapshotErr := mysqld.createSnapshot(mysqld.SnapshotDir, concurrency, serverMode)
	if snapshotErr != nil {
		relog.Error("CreateSnapshot failed: %v", snapshotErr)
	} else {

		sm := NewSnapshotManifest(sourceAddr, masterAddr, mysqld.replParams.Uname, mysqld.replParams.Pass,
			dbName, dataFiles, replicationPosition)
		smFile = path.Join(mysqld.SnapshotDir, SnapshotManifestFile)
		if snapshotErr = writeJson(smFile, sm); snapshotErr != nil {
			relog.Error("CreateSnapshot failed: %v", snapshotErr)
		}
	}

	// restore our state if required
	if serverMode && snapshotErr == nil {
		relog.Info("server mode snapshot worked, not restarting mysql")
	} else {
		if err = mysqld.SnapshotSourceEnd(slaveStartRequired, readOnly); err != nil {
			return
		}
	}

	if snapshotErr != nil {
		return "", slaveStartRequired, readOnly, snapshotErr
	}
	relative, err := filepath.Rel(mysqld.SnapshotDir, smFile)
	if err != nil {
		return "", slaveStartRequired, readOnly, nil
	}
	return path.Join(SnapshotURLPath, relative), slaveStartRequired, readOnly, nil
}

func (mysqld *Mysqld) SnapshotSourceEnd(slaveStartRequired, readOnly bool) error {
	// Try to restart mysqld
	if err := Start(mysqld, MysqlWaitTime); err != nil {
		return err
	}

	// Restore original mysqld state that we saved above.
	if slaveStartRequired {
		if err := mysqld.StartSlave(); err != nil {
			return err
		}

		// this should be quick, but we might as well just wait
		if err := mysqld.WaitForSlaveStart(SlaveStartDeadline); err != nil {
			return err
		}
	}

	// And set read-only mode
	if err := mysqld.SetReadOnly(readOnly); err != nil {
		return err
	}

	return nil
}

func writeJson(filename string, x interface{}) error {
	data, err := json.MarshalIndent(x, "  ", "  ")
	if err != nil {
		return err
	}
	return ioutil2.WriteFileAtomic(filename, data, 0660)
}

func ReadSnapshotManifest(filename string) (*SnapshotManifest, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	sm := new(SnapshotManifest)
	if err = json.Unmarshal(data, sm); err != nil {
		return nil, fmt.Errorf("ReadSnapshotManifest failed: %v %v", filename, err)
	}
	return sm, nil
}

// This piece runs on the presumably empty machine acting as the target in the
// create replica action.
//
// validate target (self)
// shutdown_mysql()
// create temp data directory /vt/target/vt_<keyspace>
// copy compressed data files via HTTP
// verify md5sum of compressed files
// uncompress into /vt/vt_<target-uid>/data/vt_<keyspace>
// start_mysql()
// clean up compressed files
func (mysqld *Mysqld) RestoreFromSnapshot(snapshotManifest *SnapshotManifest, fetchConcurrency, fetchRetryCount int, encoding string, dontWaitForSlaveStart bool) error {
	if snapshotManifest == nil {
		return errors.New("RestoreFromSnapshot: nil snapshotManifest")
	}

	relog.Debug("validateCloneTarget")
	if err := mysqld.validateCloneTarget(); err != nil {
		return err
	}

	relog.Debug("Shutdown mysqld")
	if err := Shutdown(mysqld, true, MysqlWaitTime); err != nil {
		return err
	}

	relog.Debug("Fetch snapshot")
	if err := mysqld.fetchSnapshot(snapshotManifest, fetchConcurrency, fetchRetryCount, encoding); err != nil {
		return err
	}

	relog.Debug("Restart mysqld")
	if err := Start(mysqld, MysqlWaitTime); err != nil {
		return err
	}

	cmdList := StartReplicationCommands(snapshotManifest.ReplicationState)
	relog.Info("StartReplicationCommands %#v", cmdList)
	if err := mysqld.executeSuperQueryList(cmdList); err != nil {
		return err
	}

	if !dontWaitForSlaveStart {
		if err := mysqld.WaitForSlaveStart(SlaveStartDeadline); err != nil {
			return err
		}
	}

	if err := hook.NewSimpleHook("postflight_restore").ExecuteOptional(); err != nil {
		return err
	}

	return nil
}

func (mysqld *Mysqld) fetchSnapshot(snapshotManifest *SnapshotManifest, fetchConcurrency, fetchRetryCount int, encoding string) error {
	replicaDbPath := path.Join(mysqld.config.DataDir, snapshotManifest.DbName)

	cleanDirs := []string{mysqld.SnapshotDir, replicaDbPath,
		mysqld.config.InnodbDataHomeDir, mysqld.config.InnodbLogGroupHomeDir}

	// clean out and start fresh
	// FIXME(msolomon) this might be changed to allow partial recovery, but at that point
	// we are starting to reimplement rsync.
	for _, dir := range cleanDirs {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0775); err != nil {
			return err
		}
	}

	return fetchFiles(snapshotManifest, mysqld.TabletDir, fetchConcurrency, fetchRetryCount, encoding)
}
