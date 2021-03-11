/*
Copyright 2021.

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

package keycloakbackup

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	v1 "k8s.io/api/batch/v1"
	"k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keycloakv1alpha1 "github.com/addreas/keycloak-operator/api/v1alpha1"
	"github.com/addreas/keycloak-operator/pkg/common"
)

const (
	RequeueDelay      = 30 * time.Second
	RequeueDelayError = 5 * time.Second
	ControllerName    = "keycloakbackup-controller"
)

// KeycloakBackupReconciler reconciles a KeycloakBackup object
type KeycloakBackupReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	Keycloak keycloakv1alpha1.Keycloak
}

// +kubebuilder:rbac:groups=keycloak.org,resources=keycloakbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keycloak.org,resources=keycloakbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keycloak.org,resources=keycloakbackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the KeycloakBackup object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *KeycloakBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("keycloakbackup", req.NamespacedName)

	instance := &keycloakv1alpha1.KeycloakBackup{}

	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// If no selector is set we can't figure out which Keycloak instance this backup should
	// be added for. Skip reconcile until a selector has been set.
	if instance.Spec.InstanceSelector == nil {
		log.Info(fmt.Sprintf("backup %v/%v has no instance selector and will be ignored", instance.Namespace, instance.Name))
		return ctrl.Result{Requeue: false}, nil
	}

	keycloaks, err := common.GetMatchingKeycloaks(ctx, r.Client, instance.Spec.InstanceSelector)
	if err != nil {
		return r.ManageError(ctx, instance, err)
	}

	// backups without instances to backup are treated as errors
	if len(keycloaks.Items) == 0 {
		return r.ManageError(ctx, instance, errors.Errorf("no instance to backup for %v/%v", instance.Namespace, instance.Name))
	}

	log.Info(fmt.Sprintf("found %v matching keycloak(s) for backup %v/%v", len(keycloaks.Items), instance.Namespace, instance.Name))

	var currentState *common.BackupState
	for _, keycloak := range keycloaks.Items {
		if keycloak.Spec.Unmanaged {
			return r.ManageError(ctx, instance, errors.Errorf("backups cannot be created for unmanaged keycloak instances"))
		}

		currentState = common.NewBackupState(keycloak)
		err = currentState.Read(ctx, instance, r.Client)
		if err != nil {
			return r.ManageError(ctx, instance, err)
		}
		reconciler := NewKeycloakBackupReconciler(keycloak)
		desiredState := reconciler.InnerReconcile(currentState, instance)
		actionRunner := common.NewClusterActionRunner(ctx, r.Client, r.Scheme, instance)
		err = actionRunner.RunAll(desiredState)
		if err != nil {
			return r.ManageError(ctx, instance, err)
		}
	}

	return r.ManageSuccess(ctx, instance, currentState)
}

func (r *KeycloakBackupReconciler) ManageError(ctx context.Context, instance *keycloakv1alpha1.KeycloakBackup, issue error) (ctrl.Result, error) {
	r.Recorder.Event(instance, "Warning", "ProcessingError", issue.Error())

	instance.Status.Message = issue.Error()
	instance.Status.Ready = false
	instance.Status.Phase = keycloakv1alpha1.BackupPhaseFailing

	err := r.Client.Status().Update(ctx, instance)
	if err != nil {
		r.Log.Error(err, "unable to update status")
	}

	return ctrl.Result{
		RequeueAfter: RequeueDelayError,
		Requeue:      true,
	}, nil
}

func (r *KeycloakBackupReconciler) ManageSuccess(ctx context.Context, instance *keycloakv1alpha1.KeycloakBackup, currentState *common.BackupState) (ctrl.Result, error) {
	resourcesReady, err := currentState.IsResourcesReady()
	if err != nil {
		return r.ManageError(ctx, instance, err)
	}
	instance.Status.Ready = resourcesReady
	instance.Status.Message = ""

	if resourcesReady {
		instance.Status.Phase = keycloakv1alpha1.BackupPhaseCreated
	} else {
		instance.Status.Phase = keycloakv1alpha1.BackupPhaseReconciling
	}

	err = r.Client.Status().Update(ctx, instance)
	if err != nil {
		r.Log.Error(err, "unable to update status")
		return ctrl.Result{
			RequeueAfter: RequeueDelayError,
			Requeue:      true,
		}, nil
	}

	r.Log.Info("desired cluster state met")
	return ctrl.Result{RequeueAfter: RequeueDelay}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KeycloakBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor(ControllerName)
	return ctrl.NewControllerManagedBy(mgr).
		For(&keycloakv1alpha1.KeycloakBackup{}).
		Owns(&v1.Job{}).
		Owns(&v1beta1.CronJob{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
