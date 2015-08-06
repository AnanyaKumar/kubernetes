/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package kubectl

import (
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/meta"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/fields"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/wait"
	"github.com/google/gofuzz"
)

const (
	Interval = time.Second * 1
	Timeout  = time.Minute * 5
)

// A Reaper handles terminating an object as gracefully as possible.
// timeout is how long we'll wait for the termination to be successful
// gracePeriod is time given to an API object for it to delete itself cleanly (e.g. pod shutdown)
type Reaper interface {
	Stop(namespace, name string, timeout time.Duration, gracePeriod *api.DeleteOptions) (string, error)
}

type NoSuchReaperError struct {
	kind string
}

func (n *NoSuchReaperError) Error() string {
	return fmt.Sprintf("no reaper has been implemented for %q", n.kind)
}

func IsNoSuchReaperError(err error) bool {
	_, ok := err.(*NoSuchReaperError)
	return ok
}

func ReaperFor(kind string, c client.Interface) (Reaper, error) {
	switch kind {
	case "ReplicationController":
		return &ReplicationControllerReaper{c, Interval, Timeout}, nil
	case "Daemon":
		return &DaemonReaper{c, Interval, Timeout}, nil
	case "Pod":
		return &PodReaper{c}, nil
	case "Service":
		return &ServiceReaper{c}, nil
	}
	return nil, &NoSuchReaperError{kind}
}

func ReaperForReplicationController(c client.Interface, timeout time.Duration) (Reaper, error) {
	return &ReplicationControllerReaper{c, Interval, timeout}, nil
}

type ReplicationControllerReaper struct {
	client.Interface
	pollInterval, timeout time.Duration
}
type DaemonReaper struct {
	client.Interface
	pollInterval, timeout time.Duration
}
type PodReaper struct {
	client.Interface
}
type ServiceReaper struct {
	client.Interface
}

type objInterface interface {
	Delete(name string) error
	Get(name string) (meta.Interface, error)
}

// getOverlappingControllers finds rcs that this controller overlaps, as well as rcs overlapping this controller.
func getOverlappingControllers(c client.ReplicationControllerInterface, rc *api.ReplicationController) ([]api.ReplicationController, error) {
	rcs, err := c.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("error getting replication controllers: %v", err)
	}
	var matchingRCs []api.ReplicationController
	rcLabels := labels.Set(rc.Spec.Selector)
	for _, controller := range rcs.Items {
		newRCLabels := labels.Set(controller.Spec.Selector)
		if labels.SelectorFromSet(newRCLabels).Matches(rcLabels) || labels.SelectorFromSet(rcLabels).Matches(newRCLabels) {
			matchingRCs = append(matchingRCs, controller)
		}
	}
	return matchingRCs, nil
}

func (reaper *ReplicationControllerReaper) Stop(namespace, name string, timeout time.Duration, gracePeriod *api.DeleteOptions) (string, error) {
	rc := reaper.ReplicationControllers(namespace)
	scaler, err := ScalerFor("ReplicationController", NewScalerClient(*reaper))
	if err != nil {
		return "", err
	}
	ctrl, err := rc.Get(name)
	if err != nil {
		return "", err
	}
	if timeout == 0 {
		timeout = Timeout + time.Duration(10*ctrl.Spec.Replicas)*time.Second
	}

	// The rc manager will try and detect all matching rcs for a pod's labels,
	// and only sync the oldest one. This means if we have a pod with labels
	// [(k1, v1)] and rcs with selectors [(k1, v2)] and [(k1, v1), (k2, v2)],
	// the rc manager will sync the older of the two rcs.
	//
	// If there are rcs with a superset of labels, eg:
	// deleting: (k1:v1), superset: (k2:v2, k1:v1)
	//	- It isn't safe to delete the rc because there could be a pod with labels
	//	  (k1:v1) that isn't managed by the superset rc. We can't scale it down
	//	  either, because there could be a pod (k2:v2, k1:v1) that it deletes
	//	  causing a fight with the superset rc.
	// If there are rcs with a subset of labels, eg:
	// deleting: (k2:v2, k1:v1), subset: (k1: v1), superset: (k2:v2, k1:v1, k3:v3)
	//  - It's safe to delete this rc without a scale down because all it's pods
	//	  are being controlled by the subset rc.
	// In theory, creating overlapping controllers is user error, so the loop below
	// tries to account for this logic only in the common case, where we end up
	// with multiple rcs that have an exact match on selectors.

	overlappingCtrls, err := getOverlappingControllers(rc, ctrl)
	if err != nil {
		return "", fmt.Errorf("error getting replication controllers: %v", err)
	}
	exactMatchRCs := []api.ReplicationController{}
	overlapRCs := []string{}
	for _, overlappingRC := range overlappingCtrls {
		if len(overlappingRC.Spec.Selector) == len(ctrl.Spec.Selector) {
			exactMatchRCs = append(exactMatchRCs, overlappingRC)
		} else {
			overlapRCs = append(overlapRCs, overlappingRC.Name)
		}
	}
	if len(overlapRCs) > 0 {
		return "", fmt.Errorf(
			"Detected overlapping controllers for rc %v: %v, please manage deletion individually with --cascade=false.",
			ctrl.Name, strings.Join(overlapRCs, ","))
	}
	if len(exactMatchRCs) == 1 {
		// No overlapping controllers.
		retry := NewRetryParams(reaper.pollInterval, reaper.timeout)
		waitForReplicas := NewRetryParams(reaper.pollInterval, timeout)
		if err = scaler.Scale(namespace, name, 0, nil, retry, waitForReplicas); err != nil {
			return "", err
		}
	}
	if err := rc.Delete(name); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s stopped", name), nil
}

func (reaper *DaemonReaper) Stop(namespace, name string, timeout time.Duration, gracePeriod *api.DeleteOptions) (string, error) {
	// Retrieve the daemon we want to stop.
	daemonClient := reaper.Daemons(namespace)
	daemon, err := daemonClient.Get(name)
	if err != nil {
		return "", err
	}

	// Update the daemon to select for a non-existent NodeName.
	// The daemon manager will then kill all the daemon pods corresponding to daemon daemon.
	nodes, err := reaper.Nodes().List(labels.Everything(), fields.Everything())
	if err != nil {
		return "", err
	}
	var fuzzer = fuzz.New()
	var nameExists bool
	numRetries := 1
	for try := 0; try <= numRetries; try++ {
		var nodeName string
		fuzzer.Fuzz(&nodeName)
		nameExists = false
		for _, node := range nodes.Items {
			nameExists = nameExists || node.Name == nodeName
		}
		if !nameExists {
			daemon.Spec.Template.Spec.NodeName = nodeName
			break
		}
	}
	if nameExists {
		// Probability of reaching here is extremely low, most likely indicates a programming bug/library error.
		return "", fmt.Errorf("Failed to stop node.")
	}
	daemonClient.Update(daemon)

	// Wait for the daemon manager to kill all the daemon's daemon pods.
	daemonPodsKilled := func() (bool, error) {
		updatedDc, err := daemonClient.Get(name)
		if err != nil {
			// We don't return an error, because returning an error will abort wait.Poll, but
			// if there's an error, we want to try getting the daemon again.
			return false, nil
		}
		return updatedDc.Status.CurrentNumberScheduled+updatedDc.Status.NumberMisscheduled == 0, nil
	}
	if err := wait.Poll(reaper.pollInterval, reaper.timeout, daemonPodsKilled); err != nil {
		return "", err
	}

	// Finally, kill the daemon.
	if err := daemonClient.Delete(name); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s stopped", name), nil
}

func (reaper *PodReaper) Stop(namespace, name string, timeout time.Duration, gracePeriod *api.DeleteOptions) (string, error) {
	pods := reaper.Pods(namespace)
	_, err := pods.Get(name)
	if err != nil {
		return "", err
	}
	if err := pods.Delete(name, gracePeriod); err != nil {
		return "", err
	}

	return fmt.Sprintf("%s stopped", name), nil
}

func (reaper *ServiceReaper) Stop(namespace, name string, timeout time.Duration, gracePeriod *api.DeleteOptions) (string, error) {
	services := reaper.Services(namespace)
	_, err := services.Get(name)
	if err != nil {
		return "", err
	}
	if err := services.Delete(name); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s stopped", name), nil
}
