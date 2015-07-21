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

package qos

import (
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
)

// The OOM score of a process is the percentage of memory it consumes multiplied by 100
// (barring exceptional cases). Giving over a 1000 difference between top tier and best effort
// pods means that best-effort pods will be killed before top-tier pods.
const (
	PodInfraOomAdj   int = -501
	TopTierOomAdj    int = -501
	BestEffortOomAdj int = 501
)

// Policy contains functions that decide what quality of service a pod should get.

// isBestEffort returns true if the pod is a best-effort pod, and false if it is a top-tier pod.
// Best-effort pods use resources when available, but might be evicted to make room for top-tier pods.
func IsBestEffort(podSpec *api.PodSpec) bool {
	for _, container := range podSpec.Containers {
		if container.Resources.Limits.Memory().Value() == 0 || container.Resources.Limits.Cpu().MilliValue() == 0 {
			return true
		}
	}
	return false
}

// getPodOomAdjust returns the amount by which the OOM score of all processes in the pod should be adjusted.
// Pods with lower OOM scores are less likely to be killed if the system runs out of memory.
func GetPodOomAdjust(podSpec *api.PodSpec) int {
	if IsBestEffort(podSpec) {
		return BestEffortOomAdj
	}
	return TopTierOomAdj
}
