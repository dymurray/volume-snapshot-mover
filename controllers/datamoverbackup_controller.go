/*
Copyright 2022.

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

package controllers

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	pvcv1alpha1 "github.com/konveyor/volume-snapshot-mover/api/v1alpha1"
	snapv1 "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
)

const ConditionReconciled = "Reconciled"
const ReconciledReasonError = "Error"
const ReconciledReasonComplete = "Complete"
const ReconcileCompleteMessage = "Reconcile complete"

// DataMoverBackupReconciler reconciles a DataMoverBackup object
type DataMoverBackupReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	Log            logr.Logger
	Context        context.Context
	NamespacedName types.NamespacedName
	EventRecorder  record.EventRecorder
	req            ctrl.Request
}

//+kubebuilder:rbac:groups=pvc.oadp.openshift.io,resources=datamoverbackups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=pvc.oadp.openshift.io,resources=datamoverbackups/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=pvc.oadp.openshift.io,resources=datamoverbackups/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the DataMoverBackup object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.11.0/pkg/reconcile
func (r *DataMoverBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Set reconciler vars
	r.Log = log.FromContext(ctx).WithValues("dmb", req.NamespacedName)
	result := ctrl.Result{}
	r.Context = ctx
	// needed to preserve the application ns whenever we fetch the latest DMB instance
	r.req = req

	// Get DMB CR from cluster
	dmb := pvcv1alpha1.DataMoverBackup{}
	if err := r.Get(ctx, req.NamespacedName, &dmb); err != nil {
		r.Log.Error(err, "unable to fetch DataMoverBackup CR")
		return result, err
	}

	// add protected namespace
	r.NamespacedName = types.NamespacedName{
		Namespace: dmb.Spec.ProtectedNamespace,
		Name:      dmb.Name,
	}

	if dmb.Status.Conditions != nil {
		for i := range dmb.Status.Conditions {
			if dmb.Status.Conditions[i].Type == ConditionReconciled && dmb.Status.Conditions[i].Status == metav1.ConditionTrue {
				// stop reconciling on this resource
				return ctrl.Result{
					Requeue: false,
				}, nil
			}
		}
	}

	/*if dmb.Status.DataMoverBackupStarted {
		// wait for it to complete... poll every 5 seconds
	}*/

	// Run through all reconcilers associated with DMB needs
	// Reconciliation logic

	_, err := ReconcileBatch(r.Log,
		r.ValidateDataMoverBackup,
		r.MirrorVolumeSnapshot,
		r.BindPVC,
		// TODO: Does data mover specific bits belong in a separate controller?
		r.CreateResticSecret,
		r.CreateReplicationSource,
		r.CleanBackupResources,
	)

	DMBComplete, err := r.setDMBRepSourceStatus(r.Log)
	if !DMBComplete {
		return ctrl.Result{Requeue: true}, err
	}

	// Update the status with any errors, or set completed condition
	if err != nil {
		r.Log.Info(fmt.Sprintf("Error from batch reconcile: %v", err))
		// Set failed status condition
		apimeta.SetStatusCondition(&dmb.Status.Conditions,
			metav1.Condition{
				Type:    ConditionReconciled,
				Status:  metav1.ConditionFalse,
				Reason:  ReconciledReasonError,
				Message: err.Error(),
			})
	} else {
		// Set complete status condition
		apimeta.SetStatusCondition(&dmb.Status.Conditions,
			metav1.Condition{
				Type:    ConditionReconciled,
				Status:  metav1.ConditionTrue,
				Reason:  ReconciledReasonComplete,
				Message: ReconcileCompleteMessage,
			})
	}

	statusErr := r.Client.Status().Update(ctx, &dmb)
	if err == nil { // Don't mask previous error
		err = statusErr
	}

	return ctrl.Result{}, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *DataMoverBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pvcv1alpha1.DataMoverBackup{}).
		Owns(&snapv1.VolumeSnapshotContent{}).
		Owns(&snapv1.VolumeSnapshot{}).
		Owns(&v1.PersistentVolumeClaim{}).
		Owns(&v1.Pod{}).
		WithEventFilter(datamoverBackupPredicate(r.Scheme)).
		Complete(r)
}
