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

package daemon

import (
	"fmt"
	"strconv"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/validation"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/fields"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/generic"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/fielderrors"
)

// daemonStrategy implements verification logic for daemons.
type daemonStrategy struct {
	runtime.ObjectTyper
	api.NameGenerator
}

// Strategy is the default logic that applies when creating and updating Daemon objects.
var Strategy = daemonStrategy{api.Scheme, api.SimpleNameGenerator}

// NamespaceScoped returns true because all Daemons need to be within a namespace.
func (daemonStrategy) NamespaceScoped() bool {
	return true
}

// PrepareForCreate clears the status of a daemon before creation.
func (daemonStrategy) PrepareForCreate(obj runtime.Object) {
	daemon := obj.(*api.Daemon)
	daemon.Status = api.DaemonStatus{}
}

// PrepareForUpdate clears fields that are not allowed to be set by end users on update.
func (daemonStrategy) PrepareForUpdate(obj, old runtime.Object) {
	newDaemon := obj.(*api.Daemon)
	oldDaemon := old.(*api.Daemon)
	newDaemon.Status = oldDaemon.Status
}

// Validate validates a new daemon.
func (daemonStrategy) Validate(ctx api.Context, obj runtime.Object) fielderrors.ValidationErrorList {
	daemon := obj.(*api.Daemon)
	return validation.ValidateDaemon(daemon)
}

// AllowCreateOnUpdate is false for daemon; this means a POST is
// needed to create one
func (daemonStrategy) AllowCreateOnUpdate() bool {
	return false
}

// ValidateUpdate is the default update validation for an end user.
func (daemonStrategy) ValidateUpdate(ctx api.Context, obj, old runtime.Object) fielderrors.ValidationErrorList {
	validationErrorList := validation.ValidateDaemon(obj.(*api.Daemon))
	updateErrorList := validation.ValidateDaemonUpdate(old.(*api.Daemon), obj.(*api.Daemon))
	return append(validationErrorList, updateErrorList...)
}

func (daemonStrategy) AllowUnconditionalUpdate() bool {
	return true
}

// DaemonToSelectableFields returns a label set that represents the object.
func DaemonToSelectableFields(daemon *api.Daemon) fields.Set {
	return fields.Set{
		"metadata.name":                 daemon.Name,
		"status.currentNumberScheduled": strconv.Itoa(daemon.Status.CurrentNumberScheduled),
		"status.numberMisscheduled":     strconv.Itoa(daemon.Status.NumberMisscheduled),
		"status.desiredNumberScheduled": strconv.Itoa(daemon.Status.DesiredNumberScheduled),
	}
}

// MatchDaemon is the filter used by the generic etcd backend to route
// watch events from etcd to clients of the apiserver only interested in specific
// labels/fields.
func MatchDaemon(label labels.Selector, field fields.Selector) generic.Matcher {
	return &generic.SelectionPredicate{
		Label: label,
		Field: field,
		GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
			daemon, ok := obj.(*api.Daemon)
			if !ok {
				return nil, nil, fmt.Errorf("given object is not a daemon.")
			}
			return labels.Set(daemon.ObjectMeta.Labels), DaemonToSelectableFields(daemon), nil
		},
	}
}
