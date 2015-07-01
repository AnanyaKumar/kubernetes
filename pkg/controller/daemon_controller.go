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
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/cache"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/record"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/controller/framework"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/fields"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/workqueue"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/watch"
	"github.com/golang/glog"
)

const (
	// Daemon Controllers will periodically check that their daemons are running as expected.
	FullDaemonControllerResyncPeriod = 30 * time.Second // TODO: Figure out if this time seems reasonable.
	// Nodes don't need relisting.
	FullNodeResyncPeriod = 0
)

type DaemonManager struct {
	kubeClient client.Interface
	podControl PodControlInterface

	// To allow injection of syncDaemonController for testing.
	syncHandler func(dcKey string) error
	// A TTLCache of pod creates/deletes each dc expects to see
	expectations ControllerExpectationsInterface
	// A store of daemon controllers, populated by the podController.
	dcStore cache.StoreToDaemonControllerLister
	// A store of pods, populated by the podController
	podStore cache.StoreToPodLister
	// Watches changes to all pods.
	dcController *framework.Controller
	// Watches changes to all pods
	podController *framework.Controller
	// Watches changes to all nodes.
	nodeController *framework.Controller
	// Controllers that need to be updated.
	queue *workqueue.Type
}

func NewDaemonManager(kubeClient client.Interface) *DaemonManager {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(kubeClient.Events(""))

	dm := &DaemonManager{
		kubeClient: kubeClient,
		podControl: RealPodControl{
			kubeClient: kubeClient,
			recorder:   eventBroadcaster.NewRecorder(api.EventSource{Component: "daemon-controller"}),
		},
		expectations: NewControllerExpectations(),
		queue:        workqueue.New(),
	}
	// Manage addition/update of daemon controllers.
	dm.dcStore.Store, dm.dcController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func() (runtime.Object, error) {
				return dm.kubeClient.DaemonControllers(api.NamespaceAll).List(labels.Everything())
			},
			WatchFunc: func(rv string) (watch.Interface, error) {
				return dm.kubeClient.DaemonControllers(api.NamespaceAll).Watch(labels.Everything(), fields.Everything(), rv)
			},
		},
		&api.DaemonController{},
		FullDaemonControllerResyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    dm.enqueueController,
			UpdateFunc: func(old, cur interface{}) {},
			DeleteFunc: dm.enqueueController,
		},
	)
	// Watch for creation/deletion of pods. The reason we watch is that we don't want a daemon controller to create/delete
	// more pods until all the effects (expectations) of a daemon controller's create/delete have been observed.
	dm.podStore.Store, dm.podController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func() (runtime.Object, error) {
				return dm.kubeClient.Pods(api.NamespaceAll).List(labels.Everything(), fields.Everything())
			},
			WatchFunc: func(rv string) (watch.Interface, error) {
				return dm.kubeClient.Pods(api.NamespaceAll).Watch(labels.Everything(), fields.Everything(), rv)
			},
		},
		&api.Pod{},
		PodRelistPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    dm.addPod,
			UpdateFunc: func(old, cur interface{}) {},
			DeleteFunc: dm.deletePod,
		},
	)
	// Watch for new nodes or updates to nodes - daemons are launched on new nodes, and possibly when labels on nodes change,
	_, dm.nodeController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func() (runtime.Object, error) {
				return dm.kubeClient.Nodes().List(labels.Everything(), fields.Everything())
			},
			WatchFunc: func(rv string) (watch.Interface, error) {
				return dm.kubeClient.Nodes().Watch(labels.Everything(), fields.Everything(), rv)
			},
		},
		&api.Node{},
		FullNodeResyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    dm.addNode,
			UpdateFunc: func(old, cur interface{}) {},
			DeleteFunc: dm.deleteNode,
		},
	)
	dm.syncHandler = dm.syncDaemonController
	return dm
}

func (dm *DaemonManager) Run(workers int, stopCh <-chan struct{}) {
	go dm.dcController.Run(stopCh)
	go dm.podController.Run(stopCh)
	go dm.nodeController.Run(stopCh)
	for i := 0; i < workers; i++ {
		go util.Until(dm.worker, time.Second, stopCh)
	}
	<-stopCh
	glog.Infof("Shutting down Daemon Controller Manager")
	dm.queue.ShutDown()
}

func (dm *DaemonManager) worker() {
	for {
		func() {
			key, quit := dm.queue.Get()
			if quit {
				return
			}
			defer dm.queue.Done(key)
			err := dm.syncHandler(key.(string))
			if err != nil {
				glog.Errorf("Error syncing daemon controller: %v", err)
			}
		}()
	}
}

func (dm *DaemonManager) enqueueController(obj interface{}) {
	key, err := controllerKeyFunc(obj)
	if err != nil {
		glog.Errorf("Couldn't get key for object %+v: %v", obj, err)
		return
	}
	dm.queue.Add(key)
}

func (dm *DaemonManager) getPodDaemonController(pod *api.Pod) *api.DaemonController {
	dc, err := dm.dcStore.GetPodDaemonController(pod)
	if err != nil {
		glog.V(4).Infof("No controllers found for pod %v, daemon manager will avoid syncing", pod.Name)
		return nil
	}
	return dc
}

func (dm *DaemonManager) addPod(obj interface{}) {
	pod := obj.(*api.Pod)
	if dc := dm.getPodDaemonController(pod); dc != nil {
		dcKey, err := controllerKeyFunc(dc)
		if err != nil {
			glog.Errorf("Couldn't get key for object %+v: %v", dc, err)
		}
		dm.expectations.CreationObserved(dcKey)
		dm.enqueueController(dc)
	}
}

func (dm *DaemonManager) deletePod(obj interface{}) {
	pod, ok := obj.(*api.Pod)

	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale. If the pod
	// changed labels the new rc will not be woken up till the periodic resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			glog.Errorf("Couldn't get object from tombstone %+v", obj)
			return
		}
		pod, ok = tombstone.Obj.(*api.Pod)
		if !ok {
			glog.Errorf("Tombstone contained object that is not a pod %+v", obj)
			return
		}
	}
	if dc := dm.getPodDaemonController(pod); dc != nil {
		dcKey, err := controllerKeyFunc(dc)
		if err != nil {
			glog.Errorf("Couldn't get key for object %+v: %v", dc, err)
		}
		dm.expectations.DeletionObserved(dcKey)
		dm.enqueueController(dc)
	}
}

func (dm *DaemonManager) addNode(obj interface{}) {
	node := obj.(*api.Node)
	daemonControllers, err := dm.dcStore.List()
	if err != nil {
		glog.Errorf("Error getting daemon controllers when adding node %q: %v", node.Name, err)
		return
	}
	for i := range daemonControllers {
		dm.enqueueController(daemonControllers[i])
	}
}

func (dm *DaemonManager) deleteNode(obj interface{}) {
	glog.Infoln("Node has been removed")
	// Get the daemon controller for the node
	// Update expectations
}

func (dm *DaemonManager) manageDaemons(dc *api.DaemonController) error {
	// Find out which nodes are running the daemon pod specified by dc.
	nodeToDaemonPodName := make(map[string]string)
	dcSelector := map[string]string{
		labels.DaemonControllerLabel: dc.Name,
	}
	daemonPods, err := dm.podStore.Pods(dc.Namespace).List(labels.Set(dcSelector).AsSelector())
	if err != nil {
		return err
	}
	for i := range daemonPods.Items {
		nodeToDaemonPodName[daemonPods.Items[i].Spec.NodeName] = daemonPods.Items[i].Name
	}
	// For each node, if the node is running the daemon pod but isn't supposed to, kill the daemon
	// pod. If the node is supposed to run the daemon, but isn't, create the daemon on the node.
	nodeList, err := dm.kubeClient.Nodes().List(labels.Everything(), fields.Everything())
	if err != nil {
		glog.Errorf("Couldn't get list of nodes when adding daemon controller %+v: %v", dc, err)
		return err
	}
	var numCreations, numDeletions int
	for i := range nodeList.Items {
		nodeName := nodeList.Items[i].Name
		shouldRun := true // TODO (Ananya): Compute this based on the node's labels
		daemonPodName, isRunning := nodeToDaemonPodName[nodeName]
		glog.Infoln("examining node with name", nodeName)
		if shouldRun && !isRunning {
			if err := dm.podControl.createReplicaOnNode(dc.Namespace, dc, nodeName); err != nil {
				glog.V(2).Infof("Failed creation, decrementing expectations for controller %q/%q", dc.Namespace, dc.Name)
				util.HandleError(err)
			} else {
				numCreations += 1
			}
		} else if !shouldRun && isRunning {
			if err := dm.podControl.deletePod(dc.Namespace, daemonPodName); err != nil {
				glog.V(2).Infof("Failed deletion, decrementing expectations for controller %q/%q", dc.Namespace, dc.Name)
				util.HandleError(err)
			} else {
				numDeletions += 1
			}
		}
	}
	glog.Infof("Pending stuff: %d %d", numCreations, numDeletions)
	// Set expectations
	dcKey, err := controllerKeyFunc(dc)
	if err != nil {
		glog.Errorf("Couldn't get key for object %+v: %v", dc, err)
	}
	dm.expectations.ExpectCreations(dcKey, numCreations)
	dm.expectations.ExpectDeletions(dcKey, numDeletions)
	return nil
}

func (dm *DaemonManager) syncDaemonController(key string) error {
	startTime := time.Now()
	defer func() {
		glog.V(4).Infof("Finished syncing daemon controller %q (%v)", key, time.Now().Sub(startTime))
	}()
	obj, exists, err := dm.dcStore.Store.GetByKey(key)
	if !exists {
		glog.Infof("Daemon Controller has been deleted %v", key)
		dm.expectations.DeleteExpectations(key)
		return nil
	}
	if err != nil {
		glog.Infof("Unable to retrieve dc %v from store: %v", key, err)
		dm.queue.Add(key)
		return err
	}
	dc := obj.(*api.DaemonController)

	dcKey, err := controllerKeyFunc(dc)
	if err != nil {
		glog.Errorf("Couldn't get key for object %+v: %v", dc, err)
	}
	dcNeedsSync := dm.expectations.SatisfiedExpectations(dcKey)
	if dcNeedsSync {
		return dm.manageDaemons(dc)
	} else {
		dm.queue.Add(key) // TODO (Ananya): Figure out if we should add it back, or let relists take care of syncs
	}
	return nil
}
