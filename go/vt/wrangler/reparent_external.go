// Copyright 2013, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wrangler

import (
	"fmt"
	"sync"

	"code.google.com/p/vitess/go/relog"
	"code.google.com/p/vitess/go/vt/concurrency"
	tm "code.google.com/p/vitess/go/vt/tabletmanager"
)

func (wr *Wrangler) ShardExternallyReparented(zkShardPath, zkMasterElectTabletPath string, scrapStragglers bool) error {
	if err := tm.IsShardPath(zkShardPath); err != nil {
		return err
	}
	if err := tm.IsTabletPath(zkMasterElectTabletPath); err != nil {
		return err
	}

	shardInfo, err := tm.ReadShard(wr.zconn, zkShardPath)
	if err != nil {
		return err
	}

	tabletMap, err := GetTabletMapForShard(wr.zconn, zkShardPath)
	if err != nil {
		return err
	}

	slaveTabletMap, foundMaster, err := slaveTabletMap(tabletMap)
	if err != nil {
		return err
	}

	currentMasterTabletPath, err := shardInfo.MasterTabletPath()
	if err != nil {
		return err
	}
	if currentMasterTabletPath == zkMasterElectTabletPath {
		return fmt.Errorf("master-elect tablet %v is already master", zkMasterElectTabletPath)
	}

	masterElectTablet, ok := tabletMap[zkMasterElectTabletPath]
	if !ok {
		return fmt.Errorf("master-elect tablet not found in replication graph %v %v", zkMasterElectTabletPath, zkShardPath, mapKeys(tabletMap))
	}

	// grab the shard lock
	actionPath, err := wr.ai.ShardExternallyReparented(zkShardPath, zkMasterElectTabletPath)
	if err != nil {
		return err
	}
	if err = wr.obtainActionLock(actionPath); err != nil {
		return err
	}

	relog.Info("reparentShard starting ShardExternallyReparented:%v action:%v", masterElectTablet, actionPath)

	reparentErr := wr.reparentShardExternal(slaveTabletMap, foundMaster, masterElectTablet, scrapStragglers)
	if reparentErr == nil {
		// only log if it works, if it fails we'll show the error
		relog.Info("reparentShardExternal finished")
	}

	err = wr.handleActionError(actionPath, reparentErr, false)
	if reparentErr != nil {
		if err != nil {
			relog.Warning("handleActionError failed: %v", err)
		}
		return reparentErr
	}

	return nil
}

func (wr *Wrangler) reparentShardExternal(slaveTabletMap map[string]*tm.TabletInfo, masterTablet, masterElectTablet *tm.TabletInfo, scrapStragglers bool) error {

	// we fix the new master in the replication graph
	err := wr.slaveWasPromoted(masterElectTablet)
	if err != nil {
		// This suggests that the master-elect is dead. This is bad.
		return fmt.Errorf("slaveWasPromoted failed: %v", err, masterTablet.Path())
	}

	// Once the slave is promoted, remove it from our map
	delete(slaveTabletMap, masterElectTablet.Path())

	// then fix all the slaves, including the old master
	err = wr.restartSlavesExternal(slaveTabletMap, masterTablet, masterElectTablet, scrapStragglers)
	if err != nil {
		return err
	}

	// and rebuild the shard graph
	relog.Info("rebuilding shard serving graph data in zk")
	return wr.rebuildShard(masterElectTablet.ShardPath(), []string{masterTablet.Cell, masterElectTablet.Cell})
}

func (wr *Wrangler) restartSlavesExternal(slaveTabletMap map[string]*tm.TabletInfo, masterTablet, masterElectTablet *tm.TabletInfo, scrapStragglers bool) error {
	recorder := concurrency.AllErrorRecorder{}
	wg := sync.WaitGroup{}

	swrd := tm.SlaveWasRestartedData{
		Parent:               masterElectTablet.Alias(),
		ExpectedMasterAddr:   masterElectTablet.MysqlAddr,
		ExpectedMasterIpAddr: masterElectTablet.MysqlIpAddr,
		ScrapStragglers:      scrapStragglers,
	}

	// do all the slaves
	for _, ti := range slaveTabletMap {
		wg.Add(1)
		go func(ti *tm.TabletInfo) {
			recorder.RecordError(wr.slaveWasRestarted(ti, &swrd))
			wg.Done()
		}(ti)
	}
	wg.Wait()

	// then do the master
	recorder.RecordError(wr.slaveWasRestarted(masterTablet, &swrd))
	return recorder.Error()
}

func (wr *Wrangler) slaveWasRestarted(ti *tm.TabletInfo, swrd *tm.SlaveWasRestartedData) (err error) {
	relog.Info("slaveWasRestarted(%v)", ti.Alias())
	actionPath, err := wr.ai.SlaveWasRestarted(ti.Alias(), swrd)
	if err != nil {
		return err
	}
	return wr.ai.WaitForCompletion(actionPath, wr.actionTimeout())
}
