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
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"text/template"
)

type VtReplParams struct {
	TabletHost string
	TabletPort int
	StartKey   string
	EndKey     string
}

type Mycnf struct {
	ServerId              uint
	TabletDir             string
	DataDir               string
	MycnfPath             string
	InnodbDataHomeDir     string
	InnodbLogGroupHomeDir string
	DatabaseName          string // for replication
	SocketPath            string
	MysqlPort             int
	VtHost                string
	VtPort                int
	StartKey              string
	EndKey                string
}

var innodbDataSubdir = "innodb/data"
var innodbLogSubdir = "innodb/log"

/* uid is a unique id for a particular tablet - it must be unique within the
tabletservers deployed within a keyspace, lest there be collisions on disk.
 mysqldPort needs to be unique per instance per machine (shocking) but choosing
 this sensibly has nothing to do with the config, so I'll punt.
*/
func NewMycnf(uid uint, mysqlPort int, keyspace string, vtRepl VtReplParams) *Mycnf {
	cnf := new(Mycnf)
	cnf.ServerId = uid
	cnf.MysqlPort = mysqlPort
	cnf.TabletDir = fmt.Sprintf("/vt/vt_%010d", uid)
	cnf.DataDir = path.Join(cnf.TabletDir, "data")
	cnf.MycnfPath = path.Join(cnf.TabletDir, "my.cnf")
	cnf.InnodbDataHomeDir = path.Join(cnf.TabletDir, innodbDataSubdir)
	cnf.InnodbLogGroupHomeDir = path.Join(cnf.TabletDir, innodbLogSubdir)
	cnf.SocketPath = path.Join(cnf.TabletDir, "mysql.sock")
	// this might be empty if you aren't assigned to a keyspace
	cnf.DatabaseName = keyspace
	cnf.VtHost = vtRepl.TabletHost
	cnf.VtPort = vtRepl.TabletPort
	cnf.StartKey = vtRepl.StartKey
	cnf.EndKey = vtRepl.EndKey
	return cnf
}

func (cnf *Mycnf) DirectoryList() []string {
	return []string{
		cnf.DataDir,
		cnf.InnodbDataHomeDir,
		cnf.InnodbLogGroupHomeDir,
		cnf.relayLogDir(),
		cnf.binLogDir(),
	}
}

func (cnf *Mycnf) ErrorLogPath() string {
	return path.Join(cnf.TabletDir, "error.log")
}

func (cnf *Mycnf) SlowLogPath() string {
	return path.Join(cnf.TabletDir, "slow-query.log")
}

func (cnf *Mycnf) relayLogDir() string {
	return path.Join(cnf.TabletDir, "relay-logs")
}

func (cnf *Mycnf) RelayLogPath() string {
	return path.Join(cnf.relayLogDir(),
		fmt.Sprintf("vt-%010d-relay-bin", cnf.ServerId))
}

func (cnf *Mycnf) RelayLogIndexPath() string {
	return cnf.RelayLogPath() + ".index"
}

func (cnf *Mycnf) RelayLogInfoPath() string {
	return path.Join(cnf.TabletDir, "relay-logs", "relay.info")
}

func (cnf *Mycnf) binLogDir() string {
	return path.Join(cnf.TabletDir, "bin-logs")
}

func (cnf *Mycnf) BinLogPath() string {
	return path.Join(cnf.binLogDir(),
		fmt.Sprintf("vt-%010d-bin", cnf.ServerId))
}

func (cnf *Mycnf) BinLogPathForId(fileid int) string {
	return path.Join(cnf.binLogDir(),
		fmt.Sprintf("vt-%010d-bin.%06d", cnf.ServerId, fileid))
}

func (cnf *Mycnf) BinLogIndexPath() string {
	return cnf.BinLogPath() + ".index"
}

func (cnf *Mycnf) MasterInfoPath() string {
	return path.Join(cnf.TabletDir, "master.info")
}

func (cnf *Mycnf) PidFile() string {
	return path.Join(cnf.TabletDir, "mysql.pid")
}

func (cnf *Mycnf) MysqlAddr() string {
	host, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%v:%v", host, cnf.MysqlPort)
}

/*
  Join cnf files cnfPaths and subsitute in the right values.
*/
func MakeMycnf(cnfPaths []string, mycnf *Mycnf, header string) (string, error) {
	myTemplateSource := new(bytes.Buffer)
	for _, line := range strings.Split(header, "\n") {
		fmt.Fprintf(myTemplateSource, "## %v\n", strings.TrimSpace(line))
	}
	myTemplateSource.WriteString("[mysqld]\n")
	for _, path := range cnfPaths {
		data, dataErr := ioutil.ReadFile(path)
		if dataErr != nil {
			return "", dataErr
		}
		myTemplateSource.WriteString("## " + path + "\n")
		myTemplateSource.Write(data)
	}

	myTemplate, err := template.New("").Parse(myTemplateSource.String())
	if err != nil {
		return "", err
	}
	mycnfData := new(bytes.Buffer)
	err = myTemplate.Execute(mycnfData, mycnf)
	if err != nil {
		return "", err
	}
	return mycnfData.String(), nil
}

/* Create a config for this instance. Search cnfPath for the appropriate
cnf template files.
*/
func MakeMycnfForMysqld(mysqld *Mysqld, cnfPath, header string) (string, error) {
	// FIXME(msolomon) determine config list from mysqld struct
	cnfs := []string{"default", "master", "replica"}
	paths := make([]string, len(cnfs))
	for i, name := range cnfs {
		paths[i] = fmt.Sprintf("%v/%v.cnf", cnfPath, name)
	}
	return MakeMycnf(paths, mysqld.config, header)
}

func ReadMycnf(cnfPath string) (*Mycnf, error) {
	f, err := os.Open(cnfPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := bufio.NewReader(f)
	mycnf := &Mycnf{SocketPath: "/var/lib/mysql/mysql.sock",
		MycnfPath: cnfPath,
		// FIXME(msolomon) remove this whole method, just asking for trouble
		VtHost: "localhost",
		VtPort: 6612,
	}
	for {
		line, _, err := buf.ReadLine()
		if err == io.EOF {
			break
		}
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("server-id")) {
			serverId, err := strconv.Atoi(string(bytes.TrimSpace(bytes.Split(line, []byte("="))[1])))
			if err != nil {
				return nil, err
			}
			mycnf.ServerId = uint(serverId)
		} else if bytes.HasPrefix(line, []byte("port")) {
			port, err := strconv.Atoi(string(bytes.TrimSpace(bytes.Split(line, []byte("="))[1])))
			if err != nil {
				return nil, err
			}
			mycnf.MysqlPort = port
		} else if bytes.HasPrefix(line, []byte("innodb_log_group_home_dir")) {
			mycnf.InnodbLogGroupHomeDir = string(bytes.TrimSpace(bytes.Split(line, []byte("="))[1]))
		} else if bytes.HasPrefix(line, []byte("innodb_data_home_dir")) {
			mycnf.InnodbDataHomeDir = string(bytes.TrimSpace(bytes.Split(line, []byte("="))[1]))
		} else if bytes.HasPrefix(line, []byte("socket")) {
			mycnf.SocketPath = string(bytes.TrimSpace(bytes.Split(line, []byte("="))[1]))
		}
	}

	return mycnf, nil
}
