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

package keycloakuser

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keycloakv1alpha1 "github.com/addreas/keycloak-operator/api/v1alpha1"
	"github.com/addreas/keycloak-operator/pkg/common"
)

const (
	ControllerName    = "controller_keycloakuser"
	RequeueDelayError = 5 * time.Second
)

// KeycloakUserReconciler reconciles a KeycloakUser object
type KeycloakUserReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=keycloak.org,resources=keycloakusers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keycloak.org,resources=keycloakusers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keycloak.org,resources=keycloakusers/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the KeycloakUser object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *KeycloakUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("keycloakuser", req.NamespacedName)

	instance := &keycloakv1alpha1.KeycloakUser{}

	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// If no selector is set we can't figure out which realm instance this user should
	// be added to. Skip reconcile until a selector has been set.
	if instance.Spec.RealmSelector == nil {
		log.Info(fmt.Sprintf("user %v/%v has no realm selector and will be ignored", instance.Namespace, instance.Name))
		return ctrl.Result{Requeue: false}, nil
	}

	// Find the realms that this user should be added to based on the label selector
	realms, err := common.GetMatchingRealms(ctx, r.Client, instance.Spec.RealmSelector)
	if err != nil {
		return r.ManageError(ctx, instance, err)
	}

	log.Info(fmt.Sprintf("found %v matching realm(s) for user %v/%v", len(realms.Items), instance.Namespace, instance.Name))

	for _, realm := range realms.Items {
		if realm.Spec.Unmanaged {
			return r.ManageError(ctx, instance, errors.Errorf("users cannot be created for unmanaged keycloak realms"))
		}

		keycloaks, err := common.GetMatchingKeycloaks(ctx, r.Client, realm.Spec.InstanceSelector)
		if err != nil {
			return r.ManageError(ctx, instance, err)
		}

		for _, keycloak := range keycloaks.Items {
			if keycloak.Spec.Unmanaged {
				return r.ManageError(ctx, instance, errors.Errorf("users cannot be created for unmanaged keycloak instances"))
			}

			// Get an authenticated keycloak api client for the instance
			keycloakFactory := common.LocalConfigKeycloakFactory{}
			authenticated, err := keycloakFactory.AuthenticatedClient(keycloak)
			if err != nil {
				return r.ManageError(ctx, instance, err)
			}

			// Compute the current state of the realm
			log.Info(fmt.Sprintf("got authenticated client for keycloak at %v", keycloak.Status.InternalURL))
			userState := common.NewUserState(keycloak)

			log.Info(fmt.Sprintf("read state for keycloak %v/%v, realm %v/%v",
				keycloak.Namespace,
				keycloak.Name,
				instance.Namespace,
				realm.Spec.Realm.Realm))

			err = userState.Read(authenticated, r.Client, instance, realm)
			if err != nil {
				return r.ManageError(ctx, instance, err)
			}
			reconciler := NewKeycloakuserReconciler(keycloak, realm)
			desiredState := reconciler.Reconcile(userState, instance)

			actionRunner := common.NewClusterAndKeycloakActionRunner(ctx, r.Client, r.Scheme, instance, authenticated)
			err = actionRunner.RunAll(desiredState)
			if err != nil {
				return r.ManageError(ctx, instance, err)
			}
		}
	}

	return ctrl.Result{Requeue: false}, r.manageSuccess(ctx, instance, instance.DeletionTimestamp != nil)
}

func (r *KeycloakUserReconciler) manageSuccess(ctx context.Context, user *keycloakv1alpha1.KeycloakUser, deleted bool) error {
	user.Status.Phase = keycloakv1alpha1.UserPhaseReconciled

	err := r.Client.Status().Update(ctx, user)
	if err != nil {
		r.Log.Error(err, "unable to update status")
	}

	// Finalizer already set?
	finalizerExists := false
	for _, finalizer := range user.Finalizers {
		if finalizer == keycloakv1alpha1.UserFinalizer {
			finalizerExists = true
			break
		}
	}

	// Resource created and finalizer exists: nothing to do
	if !deleted && finalizerExists {
		return nil
	}

	// Resource created and finalizer does not exist: add finalizer
	if !deleted && !finalizerExists {
		user.Finalizers = append(user.Finalizers, keycloakv1alpha1.UserFinalizer)
		r.Log.Info(fmt.Sprintf("added finalizer to keycloak user %v/%v", user.Namespace, user.Name))
		return r.Client.Update(ctx, user)
	}

	// Otherwise remove the finalizer
	newFinalizers := []string{}
	for _, finalizer := range user.Finalizers {
		if finalizer == keycloakv1alpha1.UserFinalizer {
			r.Log.Info(fmt.Sprintf("removed finalizer from keycloak user %v/%v", user.Namespace, user.Name))
			continue
		}
		newFinalizers = append(newFinalizers, finalizer)
	}

	user.Finalizers = newFinalizers
	return r.Client.Update(ctx, user)
}

func (r *KeycloakUserReconciler) ManageError(ctx context.Context, user *keycloakv1alpha1.KeycloakUser, issue error) (ctrl.Result, error) {
	r.Recorder.Event(user, "Warning", "ProcessingError", issue.Error())

	user.Status.Phase = keycloakv1alpha1.UserPhaseFailing
	user.Status.Message = issue.Error()

	err := r.Client.Status().Update(ctx, user)
	if err != nil {
		r.Log.Error(err, "unable to update status")
	}

	return ctrl.Result{
		RequeueAfter: RequeueDelayError,
	}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KeycloakUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor(ControllerName)
	return ctrl.NewControllerManagedBy(mgr).
		For(&keycloakv1alpha1.KeycloakUser{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
