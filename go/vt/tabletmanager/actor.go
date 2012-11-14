// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tabletmanager

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/vitess/go/jscfg"
	"code.google.com/p/vitess/go/relog"
	"code.google.com/p/vitess/go/vt/key"
	"code.google.com/p/vitess/go/vt/mysqlctl"
	"code.google.com/p/vitess/go/zk"
	"launchpad.net/gozk/zookeeper"
)

// The actor applies individual commands to execute an action read
// from a node in zookeeper. Anything that modifies the state of the
// table should be applied by this code.
//
// The actor signals completion by removing the action node from zookeeper.
//
// Errors are written to the action node and must (currently) be resolved
// by hand using zk tools.

const (
	restartSlaveDataFilename = "restart_slave_data.json"
)

type TabletActorError string

func (e TabletActorError) Error() string {
	return string(e)
}

type RestartSlaveData struct {
	ReplicationState *mysqlctl.ReplicationState
	WaitPosition     *mysqlctl.ReplicationPosition
	TimePromoted     int64 // used to verify replication - a row will be inserted with this timestamp
	Parent           TabletAlias
	Force            bool
}

type TabletActor struct {
	mysqld       *mysqlctl.Mysqld
	zconn        zk.Conn
	zkTabletPath string
	zkVtRoot     string
}

func NewTabletActor(mysqld *mysqlctl.Mysqld, zconn zk.Conn) *TabletActor {
	return &TabletActor{mysqld, zconn, "", ""}
}

// FIXME(msolomon) protect against unforeseen panics and classify errors as "fatal" or
// resolvable. For instance, if your zk connection fails, better to just fail. If data
// is corrupt, you can't fix it gracefully.
func (ta *TabletActor) HandleAction(actionPath, action, actionGuid string, forceRerun bool) error {
	data, stat, zkErr := ta.zconn.Get(actionPath)
	if zkErr != nil {
		relog.Error("HandleAction failed: %v", zkErr)
		return zkErr
	}

	actionNode, err := ActionNodeFromJson(data, actionPath)
	if err != nil {
		relog.Error("HandleAction failed unmarshaling %v: %v", actionPath, err)
		return err
	}

	switch actionNode.State {
	case ACTION_STATE_RUNNING:
		if !forceRerun {
			relog.Warning("HandleAction waiting for running action: %v", actionPath)
			_, err := WaitForCompletion(ta.zconn, actionPath, 0)
			return err
		}
	case ACTION_STATE_FAILED:
		// this should not be happening any more, but keep it for now
		return fmt.Errorf(actionNode.Error)
	case ACTION_STATE_DONE:
		// this is bad
		return fmt.Errorf("Unexpected finished ActionNode in action queue: %v", actionPath)
	}

	// Claim the action by this process.
	actionNode.State = ACTION_STATE_RUNNING
	newData := ActionNodeToJson(actionNode)
	_, zkErr = ta.zconn.Set(actionPath, newData, stat.Version())
	if zkErr != nil {
		if zookeeper.IsError(zkErr, zookeeper.ZBADVERSION) {
			// The action is schedule by another actor. Most likely
			// the tablet restarted during an action. Just wait for completion.
			relog.Warning("HandleAction waiting for scheduled action: %v", actionPath)
			_, err := WaitForCompletion(ta.zconn, actionPath, 0)
			return err
		} else {
			return zkErr
		}
	}

	ta.zkTabletPath = TabletPathFromActionPath(actionPath)
	ta.zkVtRoot = VtRootFromTabletPath(ta.zkTabletPath)

	relog.Info("HandleAction: %v %v", actionPath, data)

	// validate actions, but don't write this back into zk
	if actionNode.Action != action || actionNode.ActionGuid != actionGuid {
		relog.Error("HandleAction validation failed %v: (%v,%v) (%v,%v)",
			actionPath, actionNode.Action, action, actionNode.ActionGuid, actionGuid)
		return TabletActorError("invalid action initiation: " + action + " " + actionGuid)
	}

	actionErr := ta.dispatchAction(actionNode)
	err = StoreActionResponse(ta.zconn, actionNode, actionPath, actionErr)
	if err != nil {
		return err
	}

	// remove from zk on completion
	zkErr = ta.zconn.Delete(actionPath, -1)
	if zkErr != nil {
		relog.Error("HandleAction failed deleting: %v", zkErr)
		return zkErr
	}
	return actionErr
}

func (ta *TabletActor) dispatchAction(actionNode *ActionNode) (err error) {
	defer func() {
		if x := recover(); x != nil {
			if panicErr, ok := x.(error); ok {
				err = panicErr
			} else {
				err = fmt.Errorf("dispatchAction panic: %v", x)
			}
			err = relog.NewPanicError(err)
		}
	}()

	switch actionNode.Action {
	case TABLET_ACTION_BREAK_SLAVES:
		err = ta.mysqld.BreakSlaves()
	case TABLET_ACTION_CHANGE_TYPE:
		err = ta.changeType(actionNode.Args)
	case TABLET_ACTION_DEMOTE_MASTER:
		err = ta.demoteMaster()
	case TABLET_ACTION_MASTER_POSITION:
		err = ta.masterPosition(actionNode)
	case TABLET_ACTION_PARTIAL_RESTORE:
		err = ta.partialRestore(actionNode.Args)
	case TABLET_ACTION_PARTIAL_SNAPSHOT:
		err = ta.partialSnapshot(actionNode, actionNode.Args)
	case TABLET_ACTION_PING:
		// Just an end-to-end verification that we got the message.
		err = nil
	case TABLET_ACTION_PROMOTE_SLAVE:
		err = ta.promoteSlave(actionNode.Args)
	case TABLET_ACTION_RESTART_SLAVE:
		err = ta.restartSlave(actionNode.Args)
	case TABLET_ACTION_RESTORE:
		err = ta.restore(actionNode.Args)
	case TABLET_ACTION_SCRAP:
		err = ta.scrap()
	case TABLET_ACTION_GET_SCHEMA:
		err = ta.getSchema(actionNode)
	case TABLET_ACTION_PREFLIGHT_SCHEMA:
		err = ta.preflightSchema(actionNode, actionNode.Args)
	case TABLET_ACTION_APPLY_SCHEMA:
		err = ta.applySchema(actionNode, actionNode.Args)
	case TABLET_ACTION_EXECUTE_HOOK:
		err = ta.executeHook(actionNode, actionNode.Args)
	case TABLET_ACTION_SET_RDONLY:
		err = ta.setReadOnly(true)
	case TABLET_ACTION_SET_RDWR:
		err = ta.setReadOnly(false)
	case TABLET_ACTION_SLEEP:
		err = ta.sleep(actionNode.Args)
	case TABLET_ACTION_SLAVE_POSITION:
		err = ta.slavePosition(actionNode)
	case TABLET_ACTION_REPARENT_POSITION:
		err = ta.reparentPosition(actionNode, actionNode.Args)
	case TABLET_ACTION_SNAPSHOT:
		err = ta.snapshot(actionNode)
	case TABLET_ACTION_STOP_SLAVE:
		err = ta.mysqld.StopSlave()
	case TABLET_ACTION_WAIT_SLAVE_POSITION:
		err = ta.waitSlavePosition(actionNode, actionNode.Args)
	default:
		err = TabletActorError("invalid action: " + actionNode.Action)
	}

	return
}

// Write the result of an action into zookeeper
func StoreActionResponse(zconn zk.Conn, actionNode *ActionNode, actionPath string, actionErr error) error {
	// change our state
	if actionErr != nil {
		// on failure, set an error field on the node
		actionNode.Error = actionErr.Error()
		actionNode.State = ACTION_STATE_FAILED
	} else {
		actionNode.Error = ""
		actionNode.State = ACTION_STATE_DONE
	}

	// and write the data
	data := ActionNodeToJson(actionNode)
	actionLogPath := ActionToActionLogPath(actionPath)
	_, err := zconn.Create(actionLogPath, data, 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
	if err != nil {
		// this code is to address the case where the
		// tablet/shard/keyspace was created without the
		// 'actionlog' path. Let's correct it.
		if zookeeper.IsError(err, zookeeper.ZNONODE) {
			// the parent doesn't exists, try to create it
			actionLogDir := path.Dir(actionLogPath)
			_, err = zconn.Create(actionLogDir, "", 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
			if err != nil {
				return err
			}

			// and try to store again
			_, err := zconn.Create(actionLogPath, data, 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}
	return nil
}

// Store a structure inside an ActionNode Results map as the 'Result'
// field.
//
// See wrangler/Wrangler.WaitForActionResponse
func (ta *TabletActor) storeActionResult(actionNode *ActionNode, val interface{}) error {
	data, err := json.MarshalIndent(val, "", "  ")
	if err != nil {
		return err
	}
	actionNode.Results = make(map[string]string)
	actionNode.Results["Result"] = string(data)
	return nil
}

func (ta *TabletActor) sleep(args map[string]string) error {
	duration, ok := args["Duration"]
	if !ok {
		return fmt.Errorf("missing Duration in args")
	}
	d, err := time.ParseDuration(duration)
	if err != nil {
		return err
	}
	time.Sleep(d)
	return nil
}

func (ta *TabletActor) setReadOnly(rdonly bool) error {
	err := ta.mysqld.SetReadOnly(rdonly)
	if err != nil {
		return err
	}

	tablet, err := ReadTablet(ta.zconn, ta.zkTabletPath)
	if err != nil {
		return err
	}
	if rdonly {
		tablet.State = STATE_READ_ONLY
	} else {
		tablet.State = STATE_READ_WRITE
	}
	return UpdateTablet(ta.zconn, ta.zkTabletPath, tablet)
}

func (ta *TabletActor) changeType(args map[string]string) error {
	dbType, ok := args["DbType"]
	if !ok {
		return fmt.Errorf("missing DbType in args")
	}
	return ChangeType(ta.zconn, ta.zkTabletPath, TabletType(dbType))
}

func (ta *TabletActor) demoteMaster() error {
	_, err := ta.mysqld.DemoteMaster()
	if err != nil {
		return err
	}

	tablet, err := ReadTablet(ta.zconn, ta.zkTabletPath)
	if err != nil {
		return err
	}
	tablet.State = STATE_READ_ONLY
	// NOTE(msolomon) there is no serving graph update - the master tablet will
	// be replaced. Even though writes may fail, reads will succeed. It will be
	// less noisy to simply leave the entry until well promote the master.
	return UpdateTablet(ta.zconn, ta.zkTabletPath, tablet)
}

func (ta *TabletActor) promoteSlave(args map[string]string) error {
	zkShardActionPath, ok := args["ShardActionPath"]
	if !ok {
		return fmt.Errorf("missing ShardActionPath in args")
	}

	tablet, err := ReadTablet(ta.zconn, ta.zkTabletPath)
	if err != nil {
		return err
	}

	zkRestartSlaveDataPath := path.Join(zkShardActionPath, restartSlaveDataFilename)
	// The presence of this node indicates that the promote action succeeded.
	stat, err := ta.zconn.Exists(zkRestartSlaveDataPath)
	if stat != nil {
		err = fmt.Errorf("slave restart data already exists - suspicious: %v", zkRestartSlaveDataPath)
	}
	if err != nil {
		return err
	}

	// No slave data, perform the action.
	alias := TabletAlias{tablet.Tablet.Cell, tablet.Tablet.Uid}
	rsd := &RestartSlaveData{Parent: alias, Force: (tablet.Parent.Uid == NO_TABLET)}
	rsd.ReplicationState, rsd.WaitPosition, rsd.TimePromoted, err = ta.mysqld.PromoteSlave(false)
	if err != nil {
		return err
	}
	relog.Debug("PromoteSlave %#v", *rsd)
	// This data is valuable - commit it to zk first.
	_, err = ta.zconn.Create(zkRestartSlaveDataPath, jscfg.ToJson(rsd), 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
	if err != nil {
		return err
	}

	// Remove tablet from the replication graph if this is not already the master.
	if tablet.Parent.Uid != NO_TABLET {
		oldReplicationPath := tablet.ReplicationPath()
		err = ta.zconn.Delete(oldReplicationPath, -1)
		if err != nil && !zookeeper.IsError(err, zookeeper.ZNONODE) {
			return err
		}
	}
	// Update tablet regardless - trend towards consistency.
	tablet.State = STATE_READ_WRITE
	tablet.Type = TYPE_MASTER
	tablet.Parent.Cell = ""
	tablet.Parent.Uid = NO_TABLET
	err = UpdateTablet(ta.zconn, ta.zkTabletPath, tablet)
	if err != nil {
		return err
	}
	// NOTE(msolomon) A serving graph update is required, but in order for the
	// shard to be consistent the master must be scrapped first. That is
	// externally coordinated by the wrangler reparent action.

	// Insert the new tablet location in the replication graph now that
	// we've updated the tablet.
	newReplicationPath := tablet.ReplicationPath()
	_, err = ta.zconn.Create(newReplicationPath, "", 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
	if err != nil && !zookeeper.IsError(err, zookeeper.ZNODEEXISTS) {
		return err
	}

	return nil
}

func (ta *TabletActor) masterPosition(actionNode *ActionNode) error {
	position, err := ta.mysqld.MasterStatus()
	if err != nil {
		return err
	}
	relog.Debug("MasterPosition %#v", *position)
	return ta.storeActionResult(actionNode, position)
}

func (ta *TabletActor) slavePosition(actionNode *ActionNode) error {
	position, err := ta.mysqld.SlaveStatus()
	if err != nil {
		return err
	}
	relog.Debug("SlavePosition %#v", *position)
	return ta.storeActionResult(actionNode, position)
}

func (ta *TabletActor) reparentPosition(actionNode *ActionNode, args map[string]string) error {
	slavePosition, ok := args["SlavePosition"]
	if !ok {
		return fmt.Errorf("missing SlavePosition in args")
	}
	slavePos := new(mysqlctl.ReplicationPosition)
	if err := json.Unmarshal([]byte(slavePosition), slavePos); err != nil {
		return err
	}

	replicationState, waitPosition, timePromoted, err := ta.mysqld.ReparentPosition(slavePos)
	if err != nil {
		return err
	}
	rsd := new(RestartSlaveData)
	rsd.ReplicationState = replicationState
	rsd.TimePromoted = timePromoted
	rsd.WaitPosition = waitPosition
	parts := strings.Split(ta.zkTabletPath, "/")
	uid, err := strconv.ParseUint(parts[len(parts)-1], 10, 32)
	if err != nil {
		return fmt.Errorf("bad tablet uid %v", err)
	}
	rsd.Parent = TabletAlias{parts[2], uint(uid)}
	relog.Debug("reparentPosition %#v", *rsd)
	return ta.storeActionResult(actionNode, rsd)
}

func (ta *TabletActor) waitSlavePosition(actionNode *ActionNode, args map[string]string) error {
	zkArgsPath, ok := args["ArgsPath"]
	if !ok {
		return fmt.Errorf("missing ArgsPath in args")
	}

	data, _, err := ta.zconn.Get(zkArgsPath)
	if err != nil {
		return err
	}

	slavePos := new(SlavePositionReq)
	if err = json.Unmarshal([]byte(data), slavePos); err != nil {
		return err
	}

	relog.Debug("WaitSlavePosition %#v %v", *slavePos, zkArgsPath)
	err = ta.mysqld.WaitMasterPos(&slavePos.ReplicationPosition, slavePos.WaitTimeout)
	if err != nil {
		return err
	}

	return ta.slavePosition(actionNode)
}

func (ta *TabletActor) restartSlave(args map[string]string) error {
	zkShardActionPath, ok := args["ShardActionPath"]
	if !ok {
		return fmt.Errorf("missing ShardActionPath in args")
	}

	tablet, err := ReadTablet(ta.zconn, ta.zkTabletPath)
	if err != nil {
		return err
	}

	data, ok := args["RestartSlaveData"]
	if !ok {
		zkRestartSlaveDataPath := path.Join(zkShardActionPath, restartSlaveDataFilename)
		data, _, err = ta.zconn.Get(zkRestartSlaveDataPath)
		if err != nil {
			return err
		}
	}
	rsd := new(RestartSlaveData)
	err = json.Unmarshal([]byte(data), rsd)
	if err != nil {
		return err
	}

	// If this check fails, we seem reparented. The only part that could have failed
	// is the insert in the replication graph. Do NOT try to reparent
	// again. That will either wedge replication to corrupt data.
	if tablet.Parent != rsd.Parent {
		relog.Debug("restart with new parent")
		// Remove tablet from the replication graph.
		oldReplicationPath := tablet.ReplicationPath()
		err = ta.zconn.Delete(oldReplicationPath, -1)
		if err != nil && !zookeeper.IsError(err, zookeeper.ZNONODE) {
			return err
		}

		// Move a lag slave into the orphan lag type so we can safely ignore
		// this reparenting until replication catches up.
		if tablet.Type == TYPE_LAG {
			tablet.Type = TYPE_LAG_ORPHAN
		} else {
			err = ta.mysqld.RestartSlave(rsd.ReplicationState, rsd.WaitPosition, rsd.TimePromoted)
			if err != nil {
				return err
			}
		}
		// Once this action completes, update authoritive tablet node first.
		tablet.Parent = rsd.Parent
		err = UpdateTablet(ta.zconn, ta.zkTabletPath, tablet)
		if err != nil {
			return err
		}
	} else if rsd.Force {
		err = ta.mysqld.RestartSlave(rsd.ReplicationState, rsd.WaitPosition, rsd.TimePromoted)
		if err != nil {
			return err
		}
		// Complete the special orphan accounting.
		if tablet.Type == TYPE_LAG_ORPHAN {
			tablet.Type = TYPE_LAG
			err = UpdateTablet(ta.zconn, ta.zkTabletPath, tablet)
			if err != nil {
				return err
			}
		}
	}

	// Insert the new tablet location in the replication graph now that
	// we've updated the tablet.
	newReplicationPath := tablet.ReplicationPath()
	_, err = ta.zconn.Create(newReplicationPath, "", 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
	if err != nil && !zookeeper.IsError(err, zookeeper.ZNODEEXISTS) {
		return err
	}

	return nil
}

func (ta *TabletActor) scrap() error {
	return Scrap(ta.zconn, ta.zkTabletPath, false)
}

func (ta *TabletActor) getSchema(actionNode *ActionNode) error {
	// read the tablet to get the dbname
	tablet, err := ReadTablet(ta.zconn, ta.zkTabletPath)
	if err != nil {
		return err
	}

	// and get the schema
	sd, err := ta.mysqld.GetSchema(tablet.DbName())
	if err != nil {
		return err
	}

	return ta.storeActionResult(actionNode, sd)
}

func (ta *TabletActor) preflightSchema(actionNode *ActionNode, args map[string]string) error {
	// get the parameters
	change, ok := args["Change"]
	if !ok {
		return fmt.Errorf("missing Change in args")
	}

	// read the tablet to get the dbname
	tablet, err := ReadTablet(ta.zconn, ta.zkTabletPath)
	if err != nil {
		return err
	}

	// and preflight the change
	scr := ta.mysqld.PreflightSchemaChange(tablet.DbName(), change)
	return ta.storeActionResult(actionNode, scr)
}

func (ta *TabletActor) applySchema(actionNode *ActionNode, args map[string]string) error {
	// get the parameters
	sc := &mysqlctl.SchemaChange{}
	if err := json.Unmarshal([]byte(args["SchemaChange"]), sc); err != nil {
		return fmt.Errorf("SchemaChange json.Unmarshal failed: %v %v", args["SchemaChange"], err)
	}

	// read the tablet to get the dbname
	tablet, err := ReadTablet(ta.zconn, ta.zkTabletPath)
	if err != nil {
		return err
	}

	// and apply the change
	scr := ta.mysqld.ApplySchemaChange(tablet.DbName(), sc)
	return ta.storeActionResult(actionNode, scr)
}

func (ta *TabletActor) executeHook(actionNode *ActionNode, args map[string]string) (err error) {
	// reconstruct the Hook, execute it
	name := args["HookName"]
	delete(args, "HookName")
	hook := &Hook{Name: name, Parameters: args}
	hr := hook.Execute()

	// and store the result
	return ta.storeActionResult(actionNode, hr)
}

// Operate on a backup tablet. Shutdown mysqld and copy the data files aside.
func (ta *TabletActor) snapshot(actionNode *ActionNode) error {
	tablet, err := ReadTablet(ta.zconn, ta.zkTabletPath)
	if err != nil {
		return err
	}

	if tablet.Type != TYPE_BACKUP {
		return fmt.Errorf("expected backup type, not %v: %v", tablet.Type, ta.zkTabletPath)
	}

	filename, err := ta.mysqld.CreateSnapshot(tablet.DbName(), tablet.Addr, false)
	if err != nil {
		return err
	}

	actionNode.Results = make(map[string]string)
	if tablet.Parent.Uid == NO_TABLET {
		// If this is a master, this will be the new parent.
		// FIXME(msolomon) this doesn't work in hierarchical replication.
		actionNode.Results["Parent"] = tablet.Path()
	} else {
		actionNode.Results["Parent"] = TabletPathForAlias(tablet.Parent)
	}
	actionNode.Results["Manifest"] = filename
	return nil
}

// fetch a json file and parses it
func fetchAndParseJsonFile(addr, filename string, result interface{}) error {
	// read the manifest
	murl := "http://" + addr + filename
	resp, err := http.Get(murl)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Error fetching url %v: %v", murl, resp.Status)
	}
	data, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}

	// unpack it
	return json.Unmarshal(data, result)
}

// Operate on restore tablet.
// Check that the SnapshotManifest is valid and the master has not changed.
// Shutdown mysqld.
// Load the snapshot from source tablet.
// Restart mysqld and replication.
// Put tablet into the replication graph as a spare.
func (ta *TabletActor) restore(args map[string]string) error {
	// get arguments
	zkSrcTabletPath, ok := args["SrcTabletPath"]
	if !ok {
		return fmt.Errorf("missing SrcTabletPath in args")
	}
	zkSrcFilePath, ok := args["SrcFilePath"]
	if !ok {
		return fmt.Errorf("missing SrcFilePath in args")
	}
	zkParentPath, ok := args["zkParentPath"]
	if !ok {
		return fmt.Errorf("missing zkParentPath in args")
	}

	// read our current tablet, verify its state
	tablet, err := ReadTablet(ta.zconn, ta.zkTabletPath)
	if err != nil {
		return err
	}
	if tablet.Type != TYPE_RESTORE {
		return fmt.Errorf("expected restore type, not %v: %v", tablet.Type, ta.zkTabletPath)
	}

	// read the source tablet, compute zkSrcFilePath if default
	sourceTablet, err := ReadTablet(ta.zconn, zkSrcTabletPath)
	if err != nil {
		return err
	}
	if strings.ToLower(zkSrcFilePath) == "default" {
		zkSrcFilePath = path.Join(mysqlctl.SnapshotDir(uint32(sourceTablet.Uid)), mysqlctl.SnapshotManifestFile)
	}

	// read the parent tablet, verify its state
	parentTablet, err := ReadTablet(ta.zconn, zkParentPath)
	if err != nil {
		return err
	}
	if parentTablet.Type != TYPE_MASTER {
		return fmt.Errorf("restore expected master parent: %v %v", parentTablet.Type, zkParentPath)
	}

	// read & unpack the manifest
	sm := new(mysqlctl.SnapshotManifest)
	if err := fetchAndParseJsonFile(sourceTablet.Addr, zkSrcFilePath, sm); err != nil {
		return err
	}

	// and do the action
	if err := ta.mysqld.RestoreFromSnapshot(sm); err != nil {
		relog.Error("RestoreFromSnapshot failed: %v", err)
		return err
	}

	// Once this action completes, update authoritive tablet node first.
	tablet.Parent = parentTablet.Alias()
	tablet.Keyspace = sourceTablet.Keyspace
	tablet.Shard = sourceTablet.Shard
	tablet.Type = TYPE_SPARE
	tablet.KeyRange = sourceTablet.KeyRange

	if err := UpdateTablet(ta.zconn, ta.zkTabletPath, tablet); err != nil {
		return err
	}

	return CreateTabletReplicationPaths(ta.zconn, ta.zkTabletPath, tablet.Tablet)
}

// Operate on a backup tablet. Halt mysqld (read-only, lock tables)
// and dump the partial data files.
func (ta *TabletActor) partialSnapshot(actionNode *ActionNode, args map[string]string) error {
	keyName, ok := args["KeyName"]
	if !ok {
		return fmt.Errorf("missing KeyName in args")
	}
	startKey, ok := args["StartKey"]
	if !ok {
		return fmt.Errorf("missing StartKey in args")
	}
	endKey, ok := args["EndKey"]
	if !ok {
		return fmt.Errorf("missing EndKey in args")
	}

	tablet, err := ReadTablet(ta.zconn, ta.zkTabletPath)
	if err != nil {
		return err
	}

	if tablet.Type != TYPE_BACKUP {
		return fmt.Errorf("expected backup type, not %v: %v", tablet.Type, ta.zkTabletPath)
	}

	filename, err := ta.mysqld.CreateSplitSnapshot(tablet.DbName(), keyName, key.HexKeyspaceId(startKey), key.HexKeyspaceId(endKey), tablet.Addr, false)
	if err != nil {
		return err
	}

	actionNode.Results = make(map[string]string)
	if tablet.Parent.Uid == NO_TABLET {
		// If this is a master, this will be the new parent.
		// FIXME(msolomon) this doens't work in hierarchical replication.
		actionNode.Results["Parent"] = tablet.Path()
	} else {
		actionNode.Results["Parent"] = TabletPathForAlias(tablet.Parent)
	}
	actionNode.Results["Manifest"] = filename
	return nil
}

// Operate on restore tablet.
// Check that the SnapshotManifest is valid and the master has not changed.
// Put Mysql in read-only mode.
// Load the snapshot from source tablet.
// FIXME(alainjobart) which state should the tablet be in? it is a slave,
//   but with a much smaller keyspace. For now, do the same as snapshot,
//   but this is very dangerous, it cannot be used as a real slave
//   or promoted to master in the same shard!
// Put tablet into the replication graph as a spare.
func (ta *TabletActor) partialRestore(args map[string]string) error {
	// get arguments
	zkSrcTabletPath, ok := args["SrcTabletPath"]
	if !ok {
		return fmt.Errorf("missing SrcTabletPath in args")
	}
	zkSrcFilePath, ok := args["SrcFilePath"]
	if !ok {
		return fmt.Errorf("missing SrcFilePath in args")
	}
	zkParentPath, ok := args["zkParentPath"]
	if !ok {
		return fmt.Errorf("missing zkParentPath in args")
	}

	// read our current tablet, verify its state
	tablet, err := ReadTablet(ta.zconn, ta.zkTabletPath)
	if err != nil {
		return err
	}
	if tablet.Type != TYPE_RESTORE {
		return fmt.Errorf("expected restore type, not %v: %v", tablet.Type, ta.zkTabletPath)
	}

	// read the source tablet
	sourceTablet, err := ReadTablet(ta.zconn, zkSrcTabletPath)
	if err != nil {
		return err
	}

	// read the parent tablet, verify its state
	parentTablet, err := ReadTablet(ta.zconn, zkParentPath)
	if err != nil {
		return err
	}
	if parentTablet.Type != TYPE_MASTER {
		return fmt.Errorf("restore expected master parent: %v %v", parentTablet.Type, zkParentPath)
	}

	// read & unpack the manifest
	ssm := new(mysqlctl.SplitSnapshotManifest)
	if err := fetchAndParseJsonFile(sourceTablet.Addr, zkSrcFilePath, ssm); err != nil {
		return err
	}

	// and do the action
	if err := ta.mysqld.RestoreFromPartialSnapshot(ssm); err != nil {
		relog.Error("RestoreFromPartialSnapshot failed: %v", err)
		return err
	}

	// Once this action completes, update authoritive tablet node first.
	tablet.Parent = parentTablet.Alias()
	tablet.Keyspace = sourceTablet.Keyspace
	tablet.Shard = sourceTablet.Shard
	tablet.Type = TYPE_SPARE
	tablet.KeyRange = ssm.KeyRange

	if err := UpdateTablet(ta.zconn, ta.zkTabletPath, tablet); err != nil {
		return err
	}

	return CreateTabletReplicationPaths(ta.zconn, ta.zkTabletPath, tablet.Tablet)
}

// Make this external, since in needs to be forced from time to time.
func Scrap(zconn zk.Conn, zkTabletPath string, force bool) error {
	tablet, err := ReadTablet(zconn, zkTabletPath)
	if err != nil {
		return err
	}

	wasIdle := false
	replicationPath := ""
	if tablet.Type == TYPE_IDLE {
		wasIdle = true
	} else {
		replicationPath = tablet.ReplicationPath()
	}
	tablet.Type = TYPE_SCRAP
	tablet.Parent = TabletAlias{}
	// Update the tablet first, since that is canonical.
	err = UpdateTablet(zconn, zkTabletPath, tablet)
	if err != nil {
		return err
	}

	// Remove any pending actions. Presumably forcing a scrap means you don't
	// want the agent doing anything and the machine requires manual attention.
	if force {
		actionPath := TabletActionPath(zkTabletPath)
		err = PurgeActions(zconn, actionPath)
		if err != nil {
			relog.Warning("purge actions failed: %v %v", actionPath, err)
		}
	}

	if !wasIdle {
		err = zconn.Delete(replicationPath, -1)
		if err != nil {
			switch err.(*zookeeper.Error).Code {
			case zookeeper.ZNONODE:
				relog.Debug("no replication path: %v", replicationPath)
				return nil
			case zookeeper.ZNOTEMPTY:
				// If you are forcing the scrapping of a master, you can't update the
				// replication graph yet, since other nodes are still under the impression
				// they are slaved to this tablet.
				// If the node was not empty, we can't do anything about it - the replication
				// graph needs to be fixed by reparenting. If the action was forced, assume
				// the user knows best and squelch the error.
				if tablet.Parent.Uid == NO_TABLET && force {
					return nil
				}
			default:
				return err
			}
		}
	}
	return nil
}

// Make this external, since these transitions need to be forced from time to time.
func ChangeType(zconn zk.Conn, zkTabletPath string, newType TabletType) error {
	tablet, err := ReadTablet(zconn, zkTabletPath)
	if err != nil {
		return err
	}
	if !IsTrivialTypeChange(tablet.Type, newType) {
		return fmt.Errorf("cannot change tablet type %v -> %v %v", tablet.Type, newType, zkTabletPath)
	}
	tablet.Type = newType
	if newType == TYPE_IDLE {
		if tablet.Parent.Uid == NO_TABLET {
			// With a master the node cannot be set to idle unless we have already removed all of
			// the derived paths. The global replication path is a good indication that this has
			// been resolved.
			stat, err := zconn.Exists(tablet.ReplicationPath())
			if err != nil {
				return err
			}
			if stat != nil && stat.NumChildren() != 0 {
				return fmt.Errorf("cannot change tablet type %v -> %v - reparent action has not finished %v", tablet.Type, newType, zkTabletPath)
			}
		}
		tablet.Parent = TabletAlias{}
		tablet.Keyspace = ""
		tablet.Shard = ""
		tablet.KeyRange = key.KeyRange{}
	}
	return UpdateTablet(zconn, zkTabletPath, tablet)
}
