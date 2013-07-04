// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wrangler

import (
	"fmt"
	"net"
	"path"
	"sync"
	"time"

	"code.google.com/p/vitess/go/relog"
	"code.google.com/p/vitess/go/vt/naming"
	tm "code.google.com/p/vitess/go/vt/tabletmanager"
	"code.google.com/p/vitess/go/zk"
)

// As with all distributed systems, things can skew. These functions
// explore data in zookeeper and attempt to square that with reality.
//
// Given the node counts are usually large, this work should be done
// with as much parallelism as is viable.
//
// This may eventually move into a separate package.

type vresult struct {
	zkPath string
	err    error
}

func (wr *Wrangler) waitForResults(wg *sync.WaitGroup, results chan vresult) error {
	timer := time.NewTimer(wr.actionTimeout())
	done := make(chan bool, 1)
	go func() {
		wg.Wait()
		done <- true
	}()

	var err error
wait:
	for {
		select {
		case vd := <-results:
			relog.Info("checking %v", vd.zkPath)
			if vd.err != nil {
				err = fmt.Errorf("some validation errors - see log")
				relog.Error("%v: %v", vd.zkPath, vd.err)
			}
		case <-timer.C:
			err = fmt.Errorf("timed out during validate")
			break wait
		case <-done:
			// To prevent a false positive, once we are 'done',
			// drain the result channel completely.
			for {
				select {
				case vd := <-results:
					relog.Info("checking %v", vd.zkPath)
					if vd.err != nil {
						err = fmt.Errorf("some validation errors - see log")
						relog.Error("%v: %v", vd.zkPath, vd.err)
					}
				default:
					break wait
				}
			}
		}
	}

	return err
}

// Validate all tablets in all discoverable cells, even if they are
// not in the replication graph.
func (wr *Wrangler) validateAllTablets(zkKeyspacesPath string, wg *sync.WaitGroup, results chan<- vresult) {
	replicationPaths, err := zk.ChildrenRecursive(wr.zconn, zkKeyspacesPath)
	if err != nil {
		results <- vresult{zkKeyspacesPath, err}
		return
	}

	cellSet := make(map[string]bool, 16)
	for _, p := range replicationPaths {
		p := path.Join(zkKeyspacesPath, p)
		if tm.IsTabletReplicationPath(p) {
			cell, _, _ := tm.ParseTabletReplicationPath(p)
			cellSet[cell] = true
		}
	}

	vtSubTree, err := tm.VtSubtree(zkKeyspacesPath)
	if err != nil {
		results <- vresult{zkKeyspacesPath, err}
		return
	}

	for cell, _ := range cellSet {
		zkTabletsPath := path.Join("/zk", cell, vtSubTree, "tablets")
		tabletUids, _, err := wr.zconn.Children(zkTabletsPath)
		if err != nil {
			results <- vresult{zkTabletsPath, err}
		} else {
			for _, tabletUid := range tabletUids {
				tabletPath := path.Join(zkTabletsPath, tabletUid)
				wg.Add(1)
				go func() {
					results <- vresult{tabletPath, tm.Validate(wr.zconn, tabletPath, "")}
					wg.Done()
				}()
			}
		}
	}
}

func (wr *Wrangler) validateKeyspace(zkKeyspacePath string, pingTablets bool, wg *sync.WaitGroup, results chan<- vresult) {
	// Validate replication graph by traversing each shard.
	zkShardsPath := path.Join(zkKeyspacePath, "shards")
	shards, _, err := wr.zconn.Children(zkShardsPath)
	if err != nil {
		results <- vresult{zkShardsPath, err}
	}
	for _, shard := range shards {
		zkShardPath := path.Join(zkShardsPath, shard)
		wg.Add(1)
		go func() {
			wr.validateShard(zkShardPath, pingTablets, wg, results)
			wg.Done()
		}()
	}
}

// FIXME(msolomon) This validate presumes the master is up and running.
// Even when that isn't true, there are validation processes that might be valuable.
func (wr *Wrangler) validateShard(zkShardPath string, pingTablets bool, wg *sync.WaitGroup, results chan<- vresult) {
	shardInfo, err := tm.ReadShard(wr.zconn, zkShardPath)
	if err != nil {
		results <- vresult{zkShardPath, err}
		return
	}

	aliases, err := tm.FindAllTabletAliasesInShard(wr.zconn, zkShardPath)
	if err != nil {
		results <- vresult{zkShardPath, err}
	}

	shardTablets := make([]string, 0, 16)
	for _, alias := range aliases {
		shardTablets = append(shardTablets, tm.TabletPathForAlias(alias))
	}

	tabletMap, _ := GetTabletMap(wr.zconn, shardTablets)

	var masterAlias naming.TabletAlias
	for _, alias := range aliases {
		zkTabletPath := tm.TabletPathForAlias(alias)
		tabletInfo, ok := tabletMap[zkTabletPath]
		if !ok {
			results <- vresult{zkTabletPath, fmt.Errorf("tablet not found in map: %v", zkTabletPath)}
			continue
		}
		if tabletInfo.Parent.Uid == naming.NO_TABLET {
			if masterAlias.Cell != "" {
				results <- vresult{zkTabletPath, fmt.Errorf("%v: already has a master %v", zkTabletPath, masterAlias)}
			} else {
				masterAlias = alias
			}
		}
	}

	if masterAlias.Cell == "" {
		results <- vresult{zkShardPath, fmt.Errorf("no master for shard %v", zkShardPath)}
	} else if shardInfo.MasterAlias != masterAlias {
		results <- vresult{zkShardPath, fmt.Errorf("master mismatch for shard %v: found %v, expected %v", zkShardPath, masterAlias, shardInfo.MasterAlias)}
	}

	for _, alias := range aliases {
		zkTabletPath := tm.TabletPathForAlias(alias)
		zkTabletReplicationPath := path.Join(zkShardPath, masterAlias.String())
		if alias != masterAlias {
			zkTabletReplicationPath += "/" + alias.String()
		}
		wg.Add(1)
		go func() {
			results <- vresult{zkTabletReplicationPath, tm.Validate(wr.zconn, zkTabletPath, zkTabletReplicationPath)}
			wg.Done()
		}()
	}

	if pingTablets {
		wr.validateReplication(shardInfo, tabletMap, results)
		wr.pingTablets(tabletMap, wg, results)
	}

	return
}

func strInList(sl []string, s string) bool {
	for _, x := range sl {
		if x == s {
			return true
		}
	}
	return false
}

func (wr *Wrangler) validateReplication(shardInfo *tm.ShardInfo, tabletMap map[string]*tm.TabletInfo, results chan<- vresult) {
	masterTabletPath := tm.TabletPathForAlias(shardInfo.MasterAlias)
	_, ok := tabletMap[masterTabletPath]
	if !ok {
		err := fmt.Errorf("master not in tablet map: %v", masterTabletPath)
		results <- vresult{masterTabletPath, err}
		return
	}

	actionPath, err := wr.ai.GetSlaves(shardInfo.MasterAlias)
	if err != nil {
		results <- vresult{masterTabletPath, err}
		return
	}
	sa, err := wr.ai.WaitForCompletionReply(actionPath, wr.actionTimeout())
	if err != nil {
		results <- vresult{masterTabletPath, err}
		return
	}
	slaveAddrs := sa.(*tm.SlaveList).Addrs
	if len(slaveAddrs) == 0 {
		results <- vresult{masterTabletPath, fmt.Errorf("no slaves found: %v", masterTabletPath)}
		return
	}

	// Some addresses don't resolve in all locations, just use IP address
	if err != nil {
		results <- vresult{masterTabletPath, fmt.Errorf("resolve slaves failed: %v", err)}
		return
	}

	tabletIpMap := make(map[string]*tm.Tablet)
	for tabletPath, tablet := range tabletMap {
		ipAddr, _, err := net.SplitHostPort(tablet.MysqlIpAddr)
		if err != nil {
			results <- vresult{tabletPath, fmt.Errorf("bad mysql addr: %v %v %v", tablet.MysqlIpAddr, tabletPath, err)}
			continue
		}
		tabletIpMap[ipAddr] = tablet.Tablet
	}

	// See if every slave is in the replication graph.
	for _, slaveAddr := range slaveAddrs {
		if tabletIpMap[slaveAddr] == nil {
			results <- vresult{shardInfo.ShardPath(), fmt.Errorf("slave not in replication graph: %v (mysql instance without vttablet?)", slaveAddr)}
		}
	}

	// See if every entry in the replication graph is connected to the master.
	for tabletPath, tablet := range tabletMap {
		if !tablet.IsSlaveType() {
			continue
		}

		ipAddr, _, err := net.SplitHostPort(tablet.MysqlIpAddr)
		if err != nil {
			results <- vresult{tabletPath, fmt.Errorf("bad mysql addr: %v %v", tabletPath, err)}
		} else if !strInList(slaveAddrs, ipAddr) {
			results <- vresult{tabletPath, fmt.Errorf("slave not replicating: %v %v %q", tabletPath, ipAddr, slaveAddrs)}
		}
	}
}

func (wr *Wrangler) pingTablets(tabletMap map[string]*tm.TabletInfo, wg *sync.WaitGroup, results chan<- vresult) {
	for zkTabletPath, tabletInfo := range tabletMap {
		wg.Add(1)
		go func(zkTabletPath string, tabletInfo *tm.TabletInfo) {
			defer wg.Done()

			zkTabletPid := path.Join(tabletInfo.Path(), "pid")
			_, _, err := wr.zconn.Get(zkTabletPid)
			if err != nil {
				results <- vresult{zkTabletPath, fmt.Errorf("no pid node %v: %v %v", zkTabletPid, err, tabletInfo.Hostname())}
				return
			}

			actionPath, err := wr.ai.Ping(zkTabletPath)
			if err != nil {
				results <- vresult{zkTabletPath, fmt.Errorf("%v: %v %v", actionPath, err, tabletInfo.Hostname())}
				return
			}

			err = wr.ai.WaitForCompletion(actionPath, wr.actionTimeout())
			if err != nil {
				results <- vresult{zkTabletPath, fmt.Errorf("%v: %v %v", actionPath, err, tabletInfo.Hostname())}
			}
		}(zkTabletPath, tabletInfo)
	}
}

// Validate a whole zk tree
func (wr *Wrangler) Validate(zkKeyspacesPath string, pingTablets bool) error {
	// Results from various actions feed here.
	results := make(chan vresult, 16)
	wg := &sync.WaitGroup{}

	// Validate all tablets in all cells, even if they are not discoverable
	// by the replication graph.
	wg.Add(1)
	go func() {
		wr.validateAllTablets(zkKeyspacesPath, wg, results)
		wg.Done()
	}()

	// Validate replication graph by traversing each keyspace and then each shard.
	keyspaces, _, err := wr.zconn.Children(zkKeyspacesPath)
	if err != nil {
		results <- vresult{zkKeyspacesPath, err}
	} else {
		for _, keyspace := range keyspaces {
			zkKeyspace := path.Join(zkKeyspacesPath, keyspace)
			wg.Add(1)
			go func() {
				wr.validateKeyspace(zkKeyspace, pingTablets, wg, results)
				wg.Done()
			}()
		}
	}
	return wr.waitForResults(wg, results)
}

func (wr *Wrangler) ValidateKeyspace(zkKeyspacePath string, pingTablets bool) error {
	wg := &sync.WaitGroup{}
	results := make(chan vresult, 16)
	wg.Add(1)
	go func() {
		wr.validateKeyspace(zkKeyspacePath, pingTablets, wg, results)
		wg.Done()
	}()
	return wr.waitForResults(wg, results)
}

func (wr *Wrangler) ValidateShard(zkShardPath string, pingTablets bool) error {
	wg := &sync.WaitGroup{}
	results := make(chan vresult, 16)
	wg.Add(1)
	go func() {
		wr.validateShard(zkShardPath, pingTablets, wg, results)
		wg.Done()
	}()
	return wr.waitForResults(wg, results)
}
