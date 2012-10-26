// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tabletmanager

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"code.google.com/p/vitess/go/jscfg"
	"code.google.com/p/vitess/go/relog"
)

type Hook struct {
	Name       string
	Parameters map[string]string
}

type HookResult struct {
	ExitStatus int // 0 if it succeeded
	Stdout     string
	Stderr     string
}

// the hook will return a value between 0 and 255. 0 if it succeeds.
// so we have these additional values here for more information.
var (
	HOOK_SUCCESS                = 0
	HOOK_DOES_NOT_EXIST         = -1
	HOOK_STAT_FAILED            = -2
	HOOK_CANNOT_GET_EXIT_STATUS = -3
)

func (hook *Hook) Execute() (result *HookResult) {
	result = &HookResult{}

	// see if the hook exists
	vthook := os.ExpandEnv("$VTROOT/vthook/" + hook.Name)
	_, err := os.Stat(vthook)
	if err != nil {
		if os.IsNotExist(err) {
			result.ExitStatus = HOOK_DOES_NOT_EXIST
			result.Stdout = "Skipping missing hook: " + vthook + "\n"
			return result
		}

		result.ExitStatus = HOOK_STAT_FAILED
		result.Stderr = "Cannot stat hook: " + vthook + ": " + err.Error() + "\n"
		return result
	}

	// build the args, run it
	args := make([]string, 0, 10)
	for key, value := range hook.Parameters {
		if value != "" {
			args = append(args, "--"+key+"="+value)
		} else {
			args = append(args, "--"+key)
		}
	}
	relog.Info("hook: executing hook: %v %v", vthook, strings.Join(args, " "))
	cmd := exec.Command(vthook, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	if err == nil {
		result.ExitStatus = 0
	} else {
		if cmd.ProcessState != nil && cmd.ProcessState.Sys() != nil {
			result.ExitStatus = cmd.ProcessState.Sys().(syscall.WaitStatus).ExitStatus()
		} else {
			result.ExitStatus = HOOK_CANNOT_GET_EXIT_STATUS
		}
		result.Stderr += "ERROR: " + err.Error() + "\n"
	}

	relog.Info("hook: result is %v", result.String())

	return result
}

func (hr *HookResult) String() string {
	return jscfg.ToJson(hr)
}
