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

package controller

import (
	"fmt"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/latest"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/validation"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/cache"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/record"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/controller/framework"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/golang/glog"
	"sync/atomic"
)

const (
	CreatedByAnnotation = "kubernetes.io/created-by"
	updateRetries       = 1
)

var (
	controllerKeyFunc = framework.DeletionHandlingMetaNamespaceKeyFunc
)

// Expectations are a way for controllers to tell the controller manager what they expect. eg:
//	ControllerExpectations: {
//		controller1: expects  2 adds in 2 minutes
//		controller2: expects  2 dels in 2 minutes
//		controller3: expects -1 adds in 2 minutes => controller3's expectations have already been met
//	}
//
// Implementation:
//	PodExpectation = pair of atomic counters to track pod creation/deletion
//	ControllerExpectationsStore = TTLStore + a PodExpectation per controller
//
// * Once set expectations can only be lowered
// * A controller isn't synced till its expectations are either fulfilled, or expire
// * Controllers that don't set expectations will get woken up for every matching pod

// expKeyFunc to parse out the key from a PodExpectation
var expKeyFunc = func(obj interface{}) (string, error) {
	if e, ok := obj.(*PodExpectations); ok {
		return e.key, nil
	}
	return "", fmt.Errorf("Could not find key for obj %#v", obj)
}

// ControllerExpectationsInterface is an interface that allows users to set and wait on expectations.
// Only abstracted out for testing.
type ControllerExpectationsInterface interface {
	GetExpectations(controllerKey string) (*PodExpectations, bool, error)
	SatisfiedExpectations(controllerKey string) bool
	DeleteExpectations(controllerKey string)
	ExpectCreations(controllerKey string, adds int) error
	ExpectDeletions(controllerKey string, dels int) error
	CreationObserved(controllerKey string)
	DeletionObserved(controllerKey string)
}

// ControllerExpectations is a ttl cache mapping controllers to what they expect to see before being woken up for a sync.
type ControllerExpectations struct {
	cache.Store
}

// GetExpectations returns the PodExpectations of the given controller.
func (r *ControllerExpectations) GetExpectations(controllerKey string) (*PodExpectations, bool, error) {
	if podExp, exists, err := r.GetByKey(controllerKey); err == nil && exists {
		return podExp.(*PodExpectations), true, nil
	} else {
		return nil, false, err
	}
}

// DeleteExpectations deletes the expectations of the given controller from the TTLStore.
func (r *ControllerExpectations) DeleteExpectations(controllerKey string) {
	if podExp, exists, err := r.GetByKey(controllerKey); err == nil && exists {
		if err := r.Delete(podExp); err != nil {
			glog.V(2).Infof("Error deleting expectations for controller %v: %v", controllerKey, err)
		}
	}
}

// SatisfiedExpectations returns true if the replication manager has observed the required adds/dels
// for the given controller. Add/del counts are established by the controller at sync time, and updated
// as pods are observed by the replication manager's podController.
func (r *ControllerExpectations) SatisfiedExpectations(controllerKey string) bool {
	if podExp, exists, err := r.GetExpectations(controllerKey); exists {
		if podExp.Fulfilled() {
			return true
		} else {
			glog.V(4).Infof("Controller still waiting on expectations %#v", podExp)
			return false
		}
	} else if err != nil {
		glog.V(2).Infof("Error encountered while checking expectations %#v, forcing sync", err)
	} else {
		// When a new controller is created, it doesn't have expectations.
		// When it doesn't see expected watch events for > TTL, the expectations expire.
		//	- In this case it wakes up, creates/deletes pods, and sets expectations again.
		// When it has satisfied expectations and no pods need to be created/destroyed > TTL, the expectations expire.
		//	- In this case it continues without setting expectations till it needs to create/delete pods.
		glog.V(4).Infof("Controller %v either never recorded expectations, or the ttl expired.", controllerKey)
	}
	// Trigger a sync if we either encountered and error (which shouldn't happen since we're
	// getting from local store) or this controller hasn't established expectations.
	return true
}

// setExpectations registers new expectations for the given controller. Forgets existing expectations.
func (r *ControllerExpectations) setExpectations(controllerKey string, add, del int) error {
	podExp := &PodExpectations{add: int64(add), del: int64(del), key: controllerKey}
	glog.V(4).Infof("Setting expectations %+v", podExp)
	return r.Add(podExp)
}

func (r *ControllerExpectations) ExpectCreations(controllerKey string, adds int) error {
	return r.setExpectations(controllerKey, adds, 0)
}

func (r *ControllerExpectations) ExpectDeletions(controllerKey string, dels int) error {
	return r.setExpectations(controllerKey, 0, dels)
}

// Decrements the expectation counts of the given controller.
func (r *ControllerExpectations) lowerExpectations(controllerKey string, add, del int) {
	if podExp, exists, err := r.GetExpectations(controllerKey); err == nil && exists {
		podExp.Seen(int64(add), int64(del))
		// The expectations might've been modified since the update on the previous line.
		glog.V(4).Infof("Lowering expectations %+v", podExp)
	}
}

// CreationObserved atomically decrements the `add` expecation count of the given replication controller.
func (r *ControllerExpectations) CreationObserved(controllerKey string) {
	r.lowerExpectations(controllerKey, 1, 0)
}

// DeletionObserved atomically decrements the `del` expectation count of the given replication controller.
func (r *ControllerExpectations) DeletionObserved(controllerKey string) {
	r.lowerExpectations(controllerKey, 0, 1)
}

// Expectations are either fulfilled, or expire naturally.
type Expectations interface {
	Fulfilled() bool
}

// PodExpectations track pod creates/deletes.
type PodExpectations struct {
	add int64
	del int64
	key string
}

// Seen decrements the add and del counters.
func (e *PodExpectations) Seen(add, del int64) {
	atomic.AddInt64(&e.add, -add)
	atomic.AddInt64(&e.del, -del)
}

// Fulfilled returns true if this expectation has been fulfilled.
func (e *PodExpectations) Fulfilled() bool {
	// TODO: think about why this line being atomic doesn't matter
	return atomic.LoadInt64(&e.add) <= 0 && atomic.LoadInt64(&e.del) <= 0
}

// getExpectations returns the add and del expectations of the pod.
func (e *PodExpectations) getExpectations() (int64, int64) {
	return atomic.LoadInt64(&e.add), atomic.LoadInt64(&e.del)
}

// NewControllerExpectations returns a store for PodExpectations.
func NewControllerExpectations() *ControllerExpectations {
	return &ControllerExpectations{cache.NewTTLStore(expKeyFunc, ExpectationsTimeout)}
}

// PodControlInterface is an interface that knows how to add or delete pods
// created as an interface to allow testing.
type PodControlInterface interface {
	// createReplica creates new replicated pods according to the spec.
	createReplica(namespace string, controller *api.ReplicationController) error
	// createReplicaOnNodes creates a new pod according to the spec, on a specified list of nodes.
	createReplicaOnNode(namespace string, controller *api.DaemonController, nodeNames string) error
	// deletePod deletes the pod identified by podID.
	deletePod(namespace string, podID string) error
}

// RealPodControl is the default implementation of PodControllerInterface.
type RealPodControl struct {
	kubeClient client.Interface
	recorder   record.EventRecorder
}

func getReplicaLabelSet(template *api.PodTemplateSpec) labels.Set {
	desiredLabels := make(labels.Set)
	for k, v := range template.Labels {
		desiredLabels[k] = v
	}
	return desiredLabels
}

func getReplicaAnnotationSet(template *api.PodTemplateSpec, object runtime.Object) (labels.Set, error) {
	desiredAnnotations := make(labels.Set)
	for k, v := range template.Annotations {
		desiredAnnotations[k] = v
	}
	createdByRef, err := api.GetReference(object)
	if err != nil {
		return desiredAnnotations, fmt.Errorf("unable to get controller reference: %v", err)
	}
	createdByRefJson, err := latest.Codec.Encode(&api.SerializedReference{
		Reference: *createdByRef,
	})
	if err != nil {
		return desiredAnnotations, fmt.Errorf("unable to serialize controller reference: %v", err)
	}
	desiredAnnotations[CreatedByAnnotation] = string(createdByRefJson)
	return desiredAnnotations, nil
}

func getReplicaPrefix(controllerName string) string {
	// use the dash (if the name isn't too long) to make the pod name a bit prettier
	prefix := fmt.Sprintf("%s-", controllerName)
	if ok, _ := validation.ValidatePodName(prefix, true); !ok {
		prefix = controllerName
	}
	return prefix
}

func (r RealPodControl) createReplica(namespace string, controller *api.ReplicationController) error {
	desiredLabels := getReplicaLabelSet(controller.Spec.Template)
	desiredAnnotations, err := getReplicaAnnotationSet(controller.Spec.Template, controller)
	if err != nil {
		return err
	}
	prefix := getReplicaPrefix(controller.Name)

	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			Labels:       desiredLabels,
			Annotations:  desiredAnnotations,
			GenerateName: prefix,
		},
	}
	if err := api.Scheme.Convert(&controller.Spec.Template.Spec, &pod.Spec); err != nil {
		return fmt.Errorf("unable to convert pod template: %v", err)
	}
	if labels.Set(pod.Labels).AsSelector().Empty() {
		return fmt.Errorf("unable to create pod replica, no labels")
	}
	if newPod, err := r.kubeClient.Pods(namespace).Create(pod); err != nil {
		r.recorder.Eventf(controller, "failedCreate", "Error creating: %v", err)
		return fmt.Errorf("unable to create pod replica: %v", err)
	} else {
		glog.V(4).Infof("Controller %v created pod %v", controller.Name, newPod.Name)
		r.recorder.Eventf(controller, "successfulCreate", "Created pod: %v", newPod.Name)
	}
	return nil
}

func (r RealPodControl) createReplicaOnNode(namespace string, controller *api.DaemonController, nodeName string) error {
	desiredLabels := getReplicaLabelSet(controller.Spec.Template)
	desiredAnnotations, err := getReplicaAnnotationSet(controller.Spec.Template, controller)
	if err != nil {
		return err
	}
	prefix := getReplicaPrefix(controller.Name)

	pod := &api.Pod{
		ObjectMeta: api.ObjectMeta{
			Labels:       desiredLabels,
			Annotations:  desiredAnnotations,
			GenerateName: prefix,
		},
	}
	if err := api.Scheme.Convert(&controller.Spec.Template.Spec, &pod.Spec); err != nil {
		return fmt.Errorf("unable to convert pod template: %v", err)
	}
	if labels.Set(pod.Labels).AsSelector().Empty() {
		return fmt.Errorf("unable to create pod replica, no labels")
	}
	pod.Spec.NodeName = nodeName
	if newPod, err := r.kubeClient.Pods(namespace).Create(pod); err != nil {
		r.recorder.Eventf(controller, "failedCreate", "Error creating: %v", err)
		return fmt.Errorf("unable to create pod replica: %v", err)
	} else {
		glog.V(4).Infof("Controller %v created pod %v", controller.Name, newPod.Name)
		r.recorder.Eventf(controller, "successfulCreate", "Created pod: %v", newPod.Name)
	}

	return nil
}

func (r RealPodControl) deletePod(namespace, podID string) error {
	return r.kubeClient.Pods(namespace).Delete(podID, nil)
}

// activePods type allows custom sorting of pods so an rc can pick the best ones to delete.
type activePods []*api.Pod

func (s activePods) Len() int      { return len(s) }
func (s activePods) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

func (s activePods) Less(i, j int) bool {
	// Unassigned < assigned
	if s[i].Spec.NodeName == "" && s[j].Spec.NodeName != "" {
		return true
	}
	// PodPending < PodUnknown < PodRunning
	m := map[api.PodPhase]int{api.PodPending: 0, api.PodUnknown: 1, api.PodRunning: 2}
	if m[s[i].Status.Phase] != m[s[j].Status.Phase] {
		return m[s[i].Status.Phase] < m[s[j].Status.Phase]
	}
	// Not ready < ready
	if !api.IsPodReady(s[i]) && api.IsPodReady(s[j]) {
		return true
	}
	return false
}

// filterActivePods returns pods that have not terminated.
func filterActivePods(pods []api.Pod) []*api.Pod {
	var result []*api.Pod
	for i := range pods {
		if api.PodSucceeded != pods[i].Status.Phase &&
			api.PodFailed != pods[i].Status.Phase {
			result = append(result, &pods[i])
		}
	}
	return result
}

// updateReplicaCount attempts to update the Status.Replicas of the given controller, with a single GET/PUT retry.
func updateReplicaCount(rcClient client.ReplicationControllerInterface, controller api.ReplicationController, numReplicas int) (updateErr error) {
	// This is the steady state. It happens when the rc doesn't have any expectations, since
	// we do a periodic relist every 30s.
	if controller.Status.Replicas == numReplicas {
		return nil
	}
	var getErr error
	glog.V(4).Infof("Updating replica count for rc: %v, %d->%d", controller.Name, controller.Status.Replicas, numReplicas)
	for i, rc := 0, &controller; ; i++ {
		rc.Status.Replicas = numReplicas
		_, updateErr = rcClient.Update(rc)
		if updateErr == nil || i >= updateRetries {
			return updateErr
		}
		// Update the controller with the latest resource version for the next poll
		if rc, getErr = rcClient.Get(controller.Name); getErr != nil {
			// If the GET fails we can't trust status.Replicas anymore. This error
			// is bound to be more interesting than the update failure.
			return getErr
		}
	}
	// Failed 2 updates one of which was with the latest controller, return the update error
	return
}
