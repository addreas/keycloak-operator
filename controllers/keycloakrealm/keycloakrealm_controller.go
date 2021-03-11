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

package keycloakrealm

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
	RealmFinalizer    = "keycloak.org/realm-cleanup"
	RequeueDelayError = 5 * time.Second
	ControllerName    = "controller_keycloakrealm"
)

// KeycloakRealmReconciler reconciles a KeycloakRealm object
type KeycloakRealmReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	Keycloak keycloakv1alpha1.Keycloak
}

// +kubebuilder:rbac:groups=keycloak.org,resources=keycloakrealms,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keycloak.org,resources=keycloakrealms/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keycloak.org,resources=keycloakrealms/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the KeycloakRealm object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *KeycloakRealmReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("keycloakrealm", req.NamespacedName)
	instance := &keycloakv1alpha1.KeycloakRealm{}

	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if instance.Spec.Unmanaged {
		return ctrl.Result{Requeue: false}, nil
	}

	// If no selector is set we can't figure out which Keycloak instance this realm should
	// be added to. Skip reconcile until a selector has been set.
	if instance.Spec.InstanceSelector == nil {
		log.Info(fmt.Sprintf("realm %v/%v has no instance selector and will be ignored", instance.Namespace, instance.Name))
		return ctrl.Result{Requeue: false}, nil
	}

	keycloaks, err := common.GetMatchingKeycloaks(ctx, r.Client, instance.Spec.InstanceSelector)
	if err != nil {
		return ctrl.Result{}, err
	}
	log.Info(fmt.Sprintf("found %v matching keycloak(s) for realm %v/%v", len(keycloaks.Items), instance.Namespace, instance.Name))

	// The realm may be applicable to multiple keycloak instances,
	// process all of them
	for _, keycloak := range keycloaks.Items {
		// Get an authenticated keycloak api client for the instance
		keycloakFactory := common.LocalConfigKeycloakFactory{}

		if keycloak.Spec.Unmanaged {
			return ctrl.Result{}, errors.Errorf("realms cannot be created for unmanaged keycloak instances")
		}

		authenticated, err := keycloakFactory.AuthenticatedClient(keycloak)

		if err != nil {
			return ctrl.Result{}, err
		}

		// Compute the current state of the realm
		log.Info(fmt.Sprintf("got authenticated client for keycloak at %v", keycloak.Status.InternalURL))
		realmState := common.NewRealmState(ctx, keycloak)

		log.Info(fmt.Sprintf("read state for keycloak %v/%v, realm %v/%v",
			keycloak.Namespace,
			keycloak.Name,
			instance.Namespace,
			instance.Spec.Realm.Realm))

		err = realmState.Read(instance, authenticated, r.Client)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Figure out the actions to keep the realms up to date with
		// the desired state
		reconciler := NewKeycloakRealmReconciler(keycloak)
		desiredState := reconciler.InnerReconcile(realmState, instance)
		actionRunner := common.NewClusterAndKeycloakActionRunner(ctx, r.Client, r.Scheme, instance, authenticated)

		// Run all actions to keep the realms updated
		err = actionRunner.RunAll(desiredState)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{Requeue: false}, nil
}

func (r *KeycloakRealmReconciler) manageSuccess(ctx context.Context, realm *keycloakv1alpha1.KeycloakRealm, deleted bool) error {
	realm.Status.Ready = true
	realm.Status.Message = ""
	realm.Status.Phase = keycloakv1alpha1.PhaseReconciling

	err := r.Client.Status().Update(ctx, realm)
	if err != nil {
		r.Log.Error(err, "unable to update status")
	}

	// Finalizer already set?
	finalizerExists := false
	for _, finalizer := range realm.Finalizers {
		if finalizer == RealmFinalizer {
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
		realm.Finalizers = append(realm.Finalizers, RealmFinalizer)
		r.Log.Info(fmt.Sprintf("added finalizer to keycloak realm %v/%v",
			realm.Namespace,
			realm.Spec.Realm.Realm))

		return r.Client.Update(ctx, realm)
	}

	// Otherwise remove the finalizer
	newFinalizers := []string{}
	for _, finalizer := range realm.Finalizers {
		if finalizer == RealmFinalizer {
			r.Log.Info(fmt.Sprintf("removed finalizer from keycloak realm %v/%v",
				realm.Namespace,
				realm.Spec.Realm.Realm))

			continue
		}
		newFinalizers = append(newFinalizers, finalizer)
	}

	realm.Finalizers = newFinalizers
	return r.Client.Update(ctx, realm)
}

func (r *KeycloakRealmReconciler) ManageError(ctx context.Context, realm *keycloakv1alpha1.KeycloakRealm, issue error) (ctrl.Result, error) {
	r.Recorder.Event(realm, "Warning", "ProcessingError", issue.Error())

	realm.Status.Message = issue.Error()
	realm.Status.Ready = false
	realm.Status.Phase = keycloakv1alpha1.PhaseFailing

	err := r.Client.Status().Update(ctx, realm)
	if err != nil {
		r.Log.Error(err, "unable to update status")
	}

	return ctrl.Result{
		RequeueAfter: RequeueDelayError,
		Requeue:      true,
	}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KeycloakRealmReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor(ControllerName)
	return ctrl.NewControllerManagedBy(mgr).
		For(&keycloakv1alpha1.KeycloakRealm{}).
		Owns(&keycloakv1alpha1.KeycloakRealm{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
