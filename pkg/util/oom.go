/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util

import (
	"fmt"
	"io/ioutil"
	"path"
	"strconv"
)

// Writes 'value' to /proc/<pid>/oom_score_adj. PID = 0 means self
func ApplyOomScoreAdj(pid int, value int) error {
	if value < -1000 || value > 1000 {
		return fmt.Errorf("invalid value(%d) specified for oom_score_adj. Values must be within the range [-1000, 1000]", value)
	}
	if pid < 0 {
		return fmt.Errorf("invalid PID %d specified for oom_score_adj", pid)
	}

	var pidStr string
	if pid == 0 {
		pidStr = "self"
	} else {
		pidStr = strconv.Itoa(pid)
	}

	oom_value, err := ioutil.ReadFile(path.Join("/proc", pidStr, "oom_score_adj"))
	if err != nil {
		return fmt.Errorf("failed to read oom_score_adj: %v", err)
	} else if string(oom_value) != strconv.Itoa(value) {
		if err := ioutil.WriteFile(path.Join("/proc", pidStr, "oom_score_adj"), []byte(strconv.Itoa(value)), 0700); err != nil {
			return fmt.Errorf("failed to set oom_score_adj to %d: %v", value, err)
		}
	}

	return nil
}

// Writes 'value' to /proc/<pid>/oom_score_adj for all processes produced by processLister.
// Keeps trying to write until processLister produces the same list, or until max retries.
// The main use case of this is to set oom_score_adj for all processes in some group (e.g. cgroup).
func ApplyOomScoreAdjProcesses(processLister func() ([]int, error), oomScoreAdj, maxRetries int) error {
	// TODO (Ananya): implement this
	return nil
}
