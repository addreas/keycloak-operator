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

package keycloakclient

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	keycloakv1alpha1 "github.com/addreas/keycloak-operator/api/v1alpha1"
	"github.com/addreas/keycloak-operator/pkg/common"
)

const (
	ClientFinalizer   = "client.cleanup"
	RequeueDelayError = 5 * time.Second
	ControllerName    = "keycloakclient-controller"
)

// KeycloakClientReconciler reconciles a KeycloakClient object
type KeycloakClientReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	Keycloak keycloakv1alpha1.Keycloak
}

// +kubebuilder:rbac:groups=keycloak.org,resources=keycloakclients,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keycloak.org,resources=keycloakclients/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keycloak.org,resources=keycloakclients/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the KeycloakClient object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *KeycloakClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("keycloakclient", req.NamespacedName)

	instance := &keycloakv1alpha1.KeycloakClient{}

	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	r.adjustCrDefaults(instance)

	// The client may be applicable to multiple keycloak instances,
	// process all of them
	realms, err := common.GetMatchingRealms(ctx, r.Client, instance.Spec.RealmSelector)
	if err != nil {
		return r.ManageError(ctx, instance, err)
	}
	log.Info(fmt.Sprintf("found %v matching realm(s) for client %v/%v", len(realms.Items), instance.Namespace, instance.Name))
	for _, realm := range realms.Items {
		keycloaks, err := common.GetMatchingKeycloaks(ctx, r.Client, realm.Spec.InstanceSelector)
		if err != nil {
			return r.ManageError(ctx, instance, err)
		}
		log.Info(fmt.Sprintf("found %v matching keycloak(s) for realm %v/%v", len(keycloaks.Items), realm.Namespace, realm.Name))

		for _, keycloak := range keycloaks.Items {
			// Get an authenticated keycloak api client for the instance
			keycloakFactory := common.LocalConfigKeycloakFactory{}
			authenticated, err := keycloakFactory.AuthenticatedClient(keycloak)
			if err != nil {
				return r.ManageError(ctx, instance, err)
			}

			// Compute the current state of the realm
			log.Info(fmt.Sprintf("got authenticated client for keycloak at %v", authenticated.Endpoint()))
			clientState := common.NewClientState(ctx, realm.DeepCopy())

			log.Info(fmt.Sprintf("read client state for keycloak %v/%v, realm %v/%v, client %v/%v",
				keycloak.Namespace,
				keycloak.Name,
				realm.Namespace,
				realm.Name,
				instance.Namespace,
				instance.Name))

			err = clientState.Read(ctx, instance, authenticated, r.Client)
			if err != nil {
				return r.ManageError(ctx, instance, err)
			}

			// Figure out the actions to keep the realms up to date with
			// the desired state
			reconciler := NewKeycloakClientReconciler(keycloak)
			desiredState := reconciler.InnerReconcile(clientState, instance)
			actionRunner := common.NewClusterAndKeycloakActionRunner(ctx, r.Client, r.Scheme, instance, authenticated)

			// Run all actions to keep the realms updated
			err = actionRunner.RunAll(desiredState)
			if err != nil {
				return r.ManageError(ctx, instance, err)
			}
		}
	}

	return ctrl.Result{Requeue: false}, r.manageSuccess(ctx, instance, instance.DeletionTimestamp != nil)
}

// Fills the CR with default values. Nils are not acceptable for Kubernetes.
func (r *KeycloakClientReconciler) adjustCrDefaults(cr *keycloakv1alpha1.KeycloakClient) {
	if cr.Spec.Client.Attributes == nil {
		cr.Spec.Client.Attributes = make(map[string]string)
	}
	if cr.Spec.Client.Access == nil {
		cr.Spec.Client.Access = make(map[string]bool)
	}
}

func (r *KeycloakClientReconciler) manageSuccess(ctx context.Context, client *keycloakv1alpha1.KeycloakClient, deleted bool) error {
	client.Status.Ready = true
	client.Status.Message = ""
	client.Status.Phase = keycloakv1alpha1.PhaseReconciling

	err := r.Client.Status().Update(ctx, client)
	if err != nil {
		r.Log.Error(err, "unable to update status")
	}

	// Finalizer already set?
	finalizerExists := false
	for _, finalizer := range client.Finalizers {
		if finalizer == ClientFinalizer {
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
		client.Finalizers = append(client.Finalizers, ClientFinalizer)
		r.Log.Info(fmt.Sprintf("added finalizer to keycloak client %v/%v",
			client.Namespace,
			client.Spec.Client.ClientID))

		return r.Client.Update(ctx, client)
	}

	// Otherwise remove the finalizer
	newFinalizers := []string{}
	for _, finalizer := range client.Finalizers {
		if finalizer == ClientFinalizer {
			r.Log.Info(fmt.Sprintf("removed finalizer from keycloak client %v/%v",
				client.Namespace,
				client.Spec.Client.ClientID))

			continue
		}
		newFinalizers = append(newFinalizers, finalizer)
	}

	client.Finalizers = newFinalizers
	return r.Client.Update(ctx, client)
}

func (r *KeycloakClientReconciler) ManageError(ctx context.Context, realm *keycloakv1alpha1.KeycloakClient, issue error) (ctrl.Result, error) {
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
func (r *KeycloakClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor(ControllerName)
	return ctrl.NewControllerManagedBy(mgr).
		For(&keycloakv1alpha1.KeycloakClient{}).
		Owns(&corev1.Secret{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}
