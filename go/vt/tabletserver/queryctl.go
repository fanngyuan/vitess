// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tabletserver

import (
	"encoding/json"

	"code.google.com/p/vitess/go/mysql"
	"code.google.com/p/vitess/go/relog"
	"code.google.com/p/vitess/go/vt/tabletserver/proto"
)

type Config struct {
	CachePoolCap       int
	PoolSize           int
	StreamPoolSize     int
	TransactionCap     int
	TransactionTimeout float64
	MaxResultSize      int
	StreamBufferSize   int
	QueryCacheSize     int
	SchemaReloadTime   float64
	QueryTimeout       float64
	IdleTimeout        float64
}

type DBConfig struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Uname      string `json:"uname"`
	Pass       string `json:"pass"`
	Dbname     string `json:"dbname"`
	UnixSocket string `json:"unix_socket"`
	Charset    string `json:"charset"`
	Memcache   string `json:"memcache"`
}

func (d DBConfig) String() string {
	data, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err.Error()
	}
	return string(data)
}

func (d DBConfig) Redacted() interface{} {
	d.Pass = relog.Redact(d.Pass)
	return d
}

func (d DBConfig) MysqlParams() mysql.ConnectionParams {
	return mysql.ConnectionParams{
		Host:       d.Host,
		Port:       d.Port,
		Uname:      d.Uname,
		Pass:       d.Pass,
		Dbname:     d.Dbname,
		UnixSocket: d.UnixSocket,
		Charset:    d.Charset,
	}
}

var SqlQueryRpcService *SqlQuery

func RegisterQueryService(config Config) {
	if SqlQueryRpcService != nil {
		relog.Warning("RPC service already up %v", SqlQueryRpcService)
		return
	}
	SqlQueryRpcService = NewSqlQuery(config)
	proto.RegisterAuthenticated(SqlQueryRpcService)
}

// AllowQueries can take an indefinite amount of time to return because
// it keeps retrying until it obtains a valid connection to the database.
func AllowQueries(dbconfig DBConfig) {
	defer logError()
	SqlQueryRpcService.allowQueries(dbconfig)
}

// DisallowQueries can take a long time to return (not indefinite) because
// it has to wait for queries & transactions to be completed or killed,
// and also for house keeping goroutines to be terminated.
func DisallowQueries(forRestart bool) {
	defer logError()
	SqlQueryRpcService.disallowQueries(forRestart)
}

// Reload the schema. If the query service is not running, nothing will happen
func ReloadSchema() {
	defer logError()
	SqlQueryRpcService.reloadSchema()
}

func GetSessionId() int64 {
	return SqlQueryRpcService.sessionId
}
