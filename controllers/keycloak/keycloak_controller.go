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

package keycloak

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"

	grafanav1alpha1 "github.com/integr8ly/grafana-operator/api/integreatly/v1alpha1"
	routev1 "github.com/openshift/api/route/v1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	v1beta12 "k8s.io/api/policy/v1beta1"
	"k8s.io/client-go/tools/record"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keycloakv1alpha1 "github.com/addreas/keycloak-operator/api/v1alpha1"
	"github.com/addreas/keycloak-operator/pkg/common"
	"github.com/addreas/keycloak-operator/pkg/model"
	"github.com/addreas/keycloak-operator/version"
)

const (
	RequeueDelay      = 30 * time.Second
	RequeueDelayError = 5 * time.Second
	ControllerName    = "keycloak-controller"
)

// KeycloakReconciler reconciles a Keycloak object
type KeycloakReconciler struct {
	client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=keycloak.org,resources=keycloaks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=keycloak.org,resources=keycloaks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keycloak.org,resources=keycloaks/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=extensions,resources=ingresses,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=integreatly.org,resources=grafanadashboards,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Keycloak object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *KeycloakReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = r.Log.WithValues("keycloak", req.NamespacedName)
	instance := &keycloakv1alpha1.Keycloak{}

	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	currentState := common.NewClusterState()

	if instance.Spec.Unmanaged {
		return r.ManageSuccess(ctx, instance, currentState)
	}

	if instance.Spec.External.Enabled {
		return r.ManageError(ctx, instance, errors.Errorf("if external.enabled is true, unmanaged also needs to be true"))
	}

	// Read current state
	err = currentState.Read(ctx, instance, r.Client)
	if err != nil {
		return r.ManageError(ctx, instance, err)
	}

	// Get Action to reconcile current state into desired state
	reconciler := NewKeycloakReconciler()
	desiredState := reconciler.InnerReconcile(currentState, instance)

	// Perform migration if needed
	migrator, err := GetMigrator(instance)
	if err != nil {
		return r.ManageError(ctx, instance, err)
	}

	desiredState, err = migrator.Migrate(instance, currentState, desiredState)
	if err != nil {
		return r.ManageError(ctx, instance, err)
	}

	// Run the actions to reach the desired state
	actionRunner := common.NewClusterActionRunner(ctx, r.Client, r.Scheme, instance)
	err = actionRunner.RunAll(desiredState)
	if err != nil {
		return r.ManageError(ctx, instance, err)
	}

	return r.ManageSuccess(ctx, instance, currentState)
}

func (r *KeycloakReconciler) ManageError(ctx context.Context, instance *keycloakv1alpha1.Keycloak, issue error) (ctrl.Result, error) {
	r.Recorder.Event(instance, "Warning", "ProcessingError", issue.Error())

	instance.Status.Message = issue.Error()
	instance.Status.Ready = false
	instance.Status.Phase = keycloakv1alpha1.PhaseFailing

	r.setVersion(instance)

	err := r.Client.Status().Update(ctx, instance)
	if err != nil {
		r.Log.Error(err, "unable to update status")
	}

	return ctrl.Result{
		RequeueAfter: RequeueDelayError,
		Requeue:      true,
	}, nil
}

func (r *KeycloakReconciler) ManageSuccess(ctx context.Context, instance *keycloakv1alpha1.Keycloak, currentState *common.ClusterState) (ctrl.Result, error) {
	// Check if the resources are ready
	resourcesReady, err := currentState.IsResourcesReady(instance)
	if err != nil {
		return r.ManageError(ctx, instance, err)
	}

	instance.Status.Ready = resourcesReady
	instance.Status.Message = ""

	// If resources are ready and we have not errored before now, we are in a reconciling phase
	if resourcesReady {
		instance.Status.Phase = keycloakv1alpha1.PhaseReconciling
	} else {
		instance.Status.Phase = keycloakv1alpha1.PhaseInitialising
	}

	// Make this keycloaks url public to allow access via the client
	if currentState.KeycloakRoute != nil && currentState.KeycloakRoute.Spec.Host != "" { //nolint
		instance.Status.InternalURL = fmt.Sprintf("https://%v", currentState.KeycloakRoute.Spec.Host)
	} else if currentState.KeycloakIngress != nil && currentState.KeycloakIngress.Spec.Rules[0].Host != "" {
		instance.Status.InternalURL = fmt.Sprintf("https://%v", currentState.KeycloakIngress.Spec.Rules[0].Host)
	} else if currentState.KeycloakService != nil && currentState.KeycloakService.Spec.ClusterIP != "" {
		instance.Status.InternalURL = fmt.Sprintf("https://%v.%v.svc:%v",
			currentState.KeycloakService.Name,
			currentState.KeycloakService.Namespace,
			model.KeycloakServicePort)
	}

	// Let the clients know where the admin credentials are stored
	if currentState.KeycloakAdminSecret != nil {
		instance.Status.CredentialSecret = currentState.KeycloakAdminSecret.Name
	}

	r.setVersion(instance)

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

func (r *KeycloakReconciler) setVersion(instance *keycloakv1alpha1.Keycloak) {
	instance.Status.Version = version.Version
}

// SetupWithManager sets up the controller with the Manager.
func (r *KeycloakReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&keycloakv1alpha1.Keycloak{}).
		Owns(&corev1.Secret{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&v1beta1.Ingress{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&v1beta12.PodDisruptionBudget{}).
		Build(r)

	r.Recorder = mgr.GetEventRecorderFor(ControllerName)

	if err := common.WatchSecondaryResource(c, ControllerName, monitoringv1.PrometheusRuleKind, &monitoringv1.PrometheusRule{}, &keycloakv1alpha1.Keycloak{}); err != nil {
		return err
	}

	if err := common.WatchSecondaryResource(c, ControllerName, monitoringv1.ServiceMonitorsKind, &monitoringv1.ServiceMonitor{}, &keycloakv1alpha1.Keycloak{}); err != nil {
		return err
	}

	if err := common.WatchSecondaryResource(c, ControllerName, "GrafanaDashboard", &grafanav1alpha1.GrafanaDashboard{}, &keycloakv1alpha1.Keycloak{}); err != nil {
		return err
	}

	if err := common.WatchSecondaryResource(c, ControllerName, common.RouteKind, &routev1.Route{}, &keycloakv1alpha1.Keycloak{}); err != nil {
		return err
	}

	return err
}
