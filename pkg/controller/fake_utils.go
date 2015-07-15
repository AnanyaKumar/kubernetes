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

package controller

import (
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
)

type FakePodControl struct {
	controllerSpec []api.ReplicationController
	daemonSpec     []api.DaemonController
	deletePodName  []string
	lock           sync.Mutex
	err            error
}

func (f *FakePodControl) CreateReplica(namespace string, spec *api.ReplicationController) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	if f.err != nil {
		return f.err
	}
	f.controllerSpec = append(f.controllerSpec, *spec)
	return nil
}

func (f *FakePodControl) CreateReplicaOnNode(namespace string, daemon *api.DaemonController, nodeName string) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	if f.err != nil {
		return f.err
	}
	f.daemonSpec = append(f.daemonSpec, *daemon)
	return nil
}

func (f *FakePodControl) DeletePod(namespace string, podName string) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	if f.err != nil {
		return f.err
	}
	f.deletePodName = append(f.deletePodName, podName)
	return nil
}

func ValidateSyncReplication(t *testing.T, fakePodControl *FakePodControl, expectedCreates, expectedDeletes int) {
	if len(fakePodControl.controllerSpec) != expectedCreates {
		t.Errorf("Unexpected number of creates.  Expected %d, saw %d\n", expectedCreates, len(fakePodControl.controllerSpec))
	}
	if len(fakePodControl.deletePodName) != expectedDeletes {
		t.Errorf("Unexpected number of deletes.  Expected %d, saw %d\n", expectedDeletes, len(fakePodControl.deletePodName))
	}
}

func ValidateSyncDaemons(t *testing.T, fakePodControl *FakePodControl, expectedCreates, expectedDeletes int) {
	if len(fakePodControl.daemonSpec) != expectedCreates {
		t.Errorf("Unexpected number of creates.  Expected %d, saw %d\n", expectedCreates, len(fakePodControl.daemonSpec))
	}
	if len(fakePodControl.deletePodName) != expectedDeletes {
		t.Errorf("Unexpected number of deletes.  Expected %d, saw %d\n", expectedDeletes, len(fakePodControl.deletePodName))
	}
}

func (f *FakePodControl) clear() {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.deletePodName = []string{}
	f.controllerSpec = []api.ReplicationController{}
}
