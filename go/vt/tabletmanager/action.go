// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Actions modify the state of a tablet, shard or keyspace.
//
// They are currenty managed through a series of queues stored in zookeeper.

package tabletmanager

import (
	"encoding/json"
	"io"
	"strings"
	"time"

	"code.google.com/p/vitess/go/jscfg"
	"code.google.com/p/vitess/go/vt/hook"
	"code.google.com/p/vitess/go/vt/mysqlctl"
)

const (
	// FIXME(msolomon) why is ActionState a type, but Action is not?
	TABLET_ACTION_PING  = "Ping"
	TABLET_ACTION_SLEEP = "Sleep"

	TABLET_ACTION_SET_RDONLY  = "SetReadOnly"
	TABLET_ACTION_SET_RDWR    = "SetReadWrite"
	TABLET_ACTION_CHANGE_TYPE = "ChangeType"

	TABLET_ACTION_DEMOTE_MASTER       = "DemoteMaster"
	TABLET_ACTION_PROMOTE_SLAVE       = "PromoteSlave"
	TABLET_ACTION_RESTART_SLAVE       = "RestartSlave"
	TABLET_ACTION_STOP_SLAVE          = "StopSlave"
	TABLET_ACTION_BREAK_SLAVES        = "BreakSlaves"
	TABLET_ACTION_MASTER_POSITION     = "MasterPosition"
	TABLET_ACTION_REPARENT_POSITION   = "ReparentPosition"
	TABLET_ACTION_SLAVE_POSITION      = "SlavePosition"
	TABLET_ACTION_WAIT_SLAVE_POSITION = "WaitSlavePosition"
	TABLET_ACTION_SCRAP               = "Scrap"
	TABLET_ACTION_GET_SCHEMA          = "GetSchema"
	TABLET_ACTION_PREFLIGHT_SCHEMA    = "PreflightSchema"
	TABLET_ACTION_APPLY_SCHEMA        = "ApplySchema"
	TABLET_ACTION_EXECUTE_HOOK        = "ExecuteHook"

	TABLET_ACTION_SNAPSHOT            = "Snapshot"
	TABLET_ACTION_SNAPSHOT_SOURCE_END = "SnapshotSourceEnd"
	TABLET_ACTION_RESTORE             = "Restore"
	TABLET_ACTION_PARTIAL_SNAPSHOT    = "PartialSnapshot"
	TABLET_ACTION_PARTIAL_RESTORE     = "PartialRestore"

	// Shard actions - involve all tablets in a shard
	SHARD_ACTION_REPARENT = "ReparentShard"
	// Recompute derived shard-wise data
	SHARD_ACTION_REBUILD = "RebuildShard"
	// Generic read lock for inexpensive shard-wide actions.
	SHARD_ACTION_CHECK = "CheckShard"
	// Apply a schema change on an entire shard
	SHARD_ACTION_APPLY_SCHEMA = "ApplySchemaShard"

	// Keyspace actions - require very high level locking for consistency
	KEYSPACE_ACTION_REBUILD      = "RebuildKeyspace"
	KEYSPACE_ACTION_APPLY_SCHEMA = "ApplySchemaKeyspace"

	ACTION_STATE_QUEUED  = ActionState("")        // All actions are queued initially
	ACTION_STATE_RUNNING = ActionState("Running") // Running inside vtaction process
	ACTION_STATE_FAILED  = ActionState("Failed")  // Ended with a failure
	ACTION_STATE_DONE    = ActionState("Done")    // Ended with no failure
)

type ActionState string

type ActionNode struct {
	Action     string
	ActionGuid string
	Error      string
	State      ActionState

	// do not serialize the next fields
	path  string // path in zookeeper representing this action
	args  interface{}
	reply interface{}
}

func ActionNodeFromJson(data, path string) (*ActionNode, error) {
	decoder := json.NewDecoder(strings.NewReader(data))

	// decode the ActionNode
	node := &ActionNode{}
	err := decoder.Decode(node)
	if err != nil {
		return nil, err
	}
	node.path = path

	// figure out our args and reply types
	switch node.Action {
	case TABLET_ACTION_PING:
	case TABLET_ACTION_SLEEP:
		node.args = new(time.Duration)
	case TABLET_ACTION_SET_RDONLY:
	case TABLET_ACTION_SET_RDWR:
	case TABLET_ACTION_CHANGE_TYPE:
		node.args = new(TabletType)

	case TABLET_ACTION_DEMOTE_MASTER:
	case TABLET_ACTION_PROMOTE_SLAVE:
		node.args = new(string)
	case TABLET_ACTION_RESTART_SLAVE:
		node.args = &RestartSlaveArgs{}
	case TABLET_ACTION_STOP_SLAVE:
	case TABLET_ACTION_BREAK_SLAVES:
	case TABLET_ACTION_MASTER_POSITION:
		node.reply = &mysqlctl.ReplicationPosition{}
	case TABLET_ACTION_REPARENT_POSITION:
		node.args = &mysqlctl.ReplicationPosition{}
		node.reply = &RestartSlaveData{}
	case TABLET_ACTION_SLAVE_POSITION:
		node.reply = &mysqlctl.ReplicationPosition{}
	case TABLET_ACTION_WAIT_SLAVE_POSITION:
		node.args = new(string)
		node.reply = &mysqlctl.ReplicationPosition{}
	case TABLET_ACTION_SCRAP:
	case TABLET_ACTION_GET_SCHEMA:
		node.reply = &mysqlctl.SchemaDefinition{}
	case TABLET_ACTION_PREFLIGHT_SCHEMA:
		node.args = new(string)
		node.reply = &mysqlctl.SchemaChangeResult{}
	case TABLET_ACTION_APPLY_SCHEMA:
		node.args = &mysqlctl.SchemaChange{}
		node.reply = &mysqlctl.SchemaChangeResult{}
	case TABLET_ACTION_EXECUTE_HOOK:
		node.args = &hook.Hook{}
		node.reply = &hook.HookResult{}

	case TABLET_ACTION_SNAPSHOT:
		node.args = &SnapshotArgs{}
		node.reply = &SnapshotReply{}
	case TABLET_ACTION_SNAPSHOT_SOURCE_END:
		node.args = &SnapshotSourceEndArgs{}
	case TABLET_ACTION_RESTORE:
		node.args = &RestoreArgs{}
	case TABLET_ACTION_PARTIAL_SNAPSHOT:
		node.args = &PartialSnapshotArgs{}
		node.reply = &SnapshotReply{}
	case TABLET_ACTION_PARTIAL_RESTORE:
		node.args = &RestoreArgs{}

	case SHARD_ACTION_REPARENT:
		node.args = new(string)
	case SHARD_ACTION_REBUILD:
	case SHARD_ACTION_CHECK:
	case SHARD_ACTION_APPLY_SCHEMA:
		node.args = &ApplySchemaShardArgs{}

	case KEYSPACE_ACTION_REBUILD:
	case KEYSPACE_ACTION_APPLY_SCHEMA:
		node.args = &ApplySchemaKeyspaceArgs{}
	}

	// decode the args
	if node.args != nil {
		err = decoder.Decode(node.args)
	} else {
		var a interface{}
		err = decoder.Decode(&a)
	}
	if err == io.EOF {
		// no args, no reply, we're done (backward compatible mode)
		return node, nil
	} else if err != nil {
		return nil, err
	}

	// decode the reply
	if node.reply != nil {
		err = decoder.Decode(node.reply)
	} else {
		var a interface{}
		err = decoder.Decode(&a)
	}
	if err == io.EOF {
		// no reply, we're done (backward compatible mode)
		return node, nil
	} else if err != nil {
		return nil, err
	}

	return node, nil
}

func (n *ActionNode) Path() string {
	return n.path
}

func ActionNodeToJson(n *ActionNode) string {
	result := jscfg.ToJson(n) + "\n"
	if n.args == nil {
		result += "{}\n"
	} else {
		result += jscfg.ToJson(n.args) + "\n"
	}
	if n.reply == nil {
		result += "{}\n"
	} else {
		result += jscfg.ToJson(n.reply) + "\n"
	}
	return result
}
