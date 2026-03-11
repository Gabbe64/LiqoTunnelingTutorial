// Copyright 2019-2026 The Liqo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package genericgateway

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/gateway"
	"github.com/liqotech/liqo/pkg/gateway/forge"
	enutils "github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/external-network/utils"
	mapsutil "github.com/liqotech/liqo/pkg/utils/maps"
	"github.com/liqotech/liqo/pkg/utils/resource"
)

// GenericGwClient defines the interface that a protocol-specific GenericGwClient must implement.
type GenericGwClient interface {
	client.Object
	GetDeploymentTemplate() *networkingv1beta1.DeploymentTemplate
	GetMetrics() *networkingv1beta1.Metrics
	GetSecretRef() corev1.LocalObjectReference

	SetSecretRefStatus(ref *corev1.ObjectReference)
	SetInternalEndpointStatus(endpoint *networkingv1beta1.InternalGatewayEndpoint)
}

// OptionClient is a collection of simple POSSIBLE callbacks used to manage protocol-specific data 
// that can be different for each protocol.
//
// These callbacks depends on the protocol implementation and on the involved custom resources.
//
// e.g. Wireguard uses the PublicKey CR to store the public key of the remote peer, so it needs
// a callback to access that field; for an example, check the func "EnsureKeysSecret" 
// in pkg/liqo-controller-manager/networking/external-network/wireguard/wggatewayclient_controller.go
// and implemented in /pkg/liqo-controller-manager/networking/external-network/wireguard/utils.go
type OptionClient struct {
	// NewResource returns a pointer to a new empty instance of the protocol-specific GenericGwClient.
	NewResource           func() GenericGwClient
	// EnsureSecret is a callback used to ensure the presence of the secret containing the protocol keys, if needed.
	EnsureSecret          func(context.Context, client.Client, GenericGwClient) error
	// CheckExistingSecret is a callback used to check the presence and correctness of the secret containing the protocol keys, if needed.
	CheckExistingSecret   func(context.Context, client.Client, string, string, metav1.Object) error
	// GetSecretRefStatus is a callback used to get the reference to the secret containing the protocol keys, if needed.
	GetSecretRefStatus    func(GenericGwClient) *corev1.ObjectReference
	// HandleSecretRefStatus is a callback used to handle the secret reference status, if needed.
	HandleSecretRefStatus func(context.Context, client.Client, GenericGwClient) error
	SecretVolumeName      string
	// CustomBuilderSetup is callback used to customize the builder of the controller, if needed (e.g. to watch additional resources).
	CustomBuilderSetup    func(*builder.Builder) *builder.Builder
}

// GenericGwClientReconciler manage GenericGwClient lifecycle.
type GenericGwClientReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	clusterRoleName string

	eventRecorder record.EventRecorder
	options       OptionClient
}

// NewGenericGwClientReconciler returns a new GenericGwClientReconciler.
func NewGenericGwClientReconciler(cl client.Client, s *runtime.Scheme,
	recorder record.EventRecorder,
	clusterRoleName string, opts OptionClient) *GenericGwClientReconciler {
	return &GenericGwClientReconciler{
		Client:          cl,
		Scheme:          s,
		clusterRoleName: clusterRoleName,

		eventRecorder: recorder,
		options:       opts,
	}
}

// Reconcile manage GenericGwClient lifecycle.
func (r *GenericGwClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	genericGwClient := r.options.NewResource()
	if err = r.Get(ctx, req.NamespacedName, genericGwClient); err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("Gateway client %q not found", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		klog.Errorf("Unable to get the Gateway client %q: %v", req.NamespacedName, err)
		return ctrl.Result{}, err
	}

	if !genericGwClient.GetDeletionTimestamp().IsZero() {
		if controllerutil.ContainsFinalizer(genericGwClient, consts.ClusterRoleBindingFinalizer) {
			if err = enutils.DeleteClusterRoleBinding(ctx, r.Client, genericGwClient); err != nil {
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(genericGwClient, consts.ClusterRoleBindingFinalizer)
			if err = r.Update(ctx, genericGwClient); err != nil {
				klog.Errorf("Unable to remove finalizer %q from Gateway client %q: %v",
					consts.ClusterRoleBindingFinalizer, req.NamespacedName, err)
				return ctrl.Result{}, err
			}
		}

		// Resource is deleting and child resources are deleted as well by garbage collector. Nothing to do.
		return ctrl.Result{}, nil
	}

	originalGwClient := genericGwClient.DeepCopyObject().(GenericGwClient)

	// Ensure ServiceAccount and ClusterRoleBinding (create or update)
	if err = enutils.EnsureServiceAccountAndClusterRoleBinding(ctx, r.Client, r.Scheme, genericGwClient.GetDeploymentTemplate(), genericGwClient,
		r.clusterRoleName); err != nil {
		return ctrl.Result{}, err
	}

	// update if the genericGwClient has been updated
	if !equality.Semantic.DeepEqual(originalGwClient, genericGwClient) {
		if err := r.Update(ctx, genericGwClient); err != nil {
			return ctrl.Result{}, err
		}

		// we return here to avoid conflicts
		return ctrl.Result{}, nil
	}

	deployNsName := types.NamespacedName{Namespace: genericGwClient.GetNamespace(), Name: forge.GatewayResourceName(genericGwClient.GetName())}

	var deploy *appsv1.Deployment
	var d appsv1.Deployment
	err = r.Get(ctx, deployNsName, &d)
	switch {
	case apierrors.IsNotFound(err):
		deploy = nil
	case err != nil:
		klog.Errorf("error while getting deployment %q: %v", deployNsName, err)
		return ctrl.Result{}, err
	default:
		deploy = &d
	}

	// Handle status
	defer func() {
		newErr := r.Status().Update(ctx, genericGwClient)
		if newErr != nil {
			if err != nil {
				klog.Errorf("Error reconciling the Gateway client %q: %s", req.NamespacedName, err)
			}
			klog.Errorf("Unable to update the Gateway client status %q: %s", req.NamespacedName, newErr)
			err = newErr
			return
		}

		r.eventRecorder.Event(genericGwClient, corev1.EventTypeNormal, "Reconciled", "Gateway client reconciled")
	}()

	if err := r.handleInternalEndpointStatus(ctx, genericGwClient, deploy); err != nil {
		klog.Errorf("Error while handling internal endpoint status: %v", err)
		r.eventRecorder.Event(genericGwClient, corev1.EventTypeWarning, "InternalEndpointStatusFailed",
			fmt.Sprintf("Failed to handle internal endpoint status: %s", err))
		return ctrl.Result{}, err
	}

	
	/************** SECRET MANAGEMENT LOGIC (if needed) **************

	// If a secret has not been provided in the gateway specification, the controller is in charge of generating a secret with the protocol keys.
	secretRef := genericGwClient.GetSecretRef()
	if secretRef.Name == "" {
		// Ensure keys secret (create or update)
		if r.options.EnsureSecret != nil {
			if err = r.options.EnsureSecret(ctx, r.Client, genericGwClient); err != nil {
				r.eventRecorder.Event(genericGwClient, corev1.EventTypeWarning, "KeysSecretEnforcedFailed", "Failed to enforce keys secret")
				return ctrl.Result{}, err
			}
			r.eventRecorder.Event(genericGwClient, corev1.EventTypeNormal, "KeysSecretEnforced", "Enforced keys secret")
		}
	} else {
		// Check that the secret exists and ensure is correctly labeled
		if r.options.CheckExistingSecret != nil {
			if err = r.options.CheckExistingSecret(ctx, r.Client, secretRef.Name, genericGwClient.GetNamespace(), genericGwClient); err != nil {
				r.eventRecorder.Event(genericGwClient, corev1.EventTypeWarning, "KeysSecretCheckFailed", fmt.Sprintf("Failed to check keys secret: %s", err))
				return ctrl.Result{}, err
			}
			r.eventRecorder.Event(genericGwClient, corev1.EventTypeNormal, "KeysSecretChecked", "Checked keys secret")
		 }
	}

	if r.options.HandleSecretRefStatus != nil {
		if err := r.options.HandleSecretRefStatus(ctx, r.Client, genericGwClient); err != nil {
			klog.Errorf("Error while handling secret ref status: %v", err)
			r.eventRecorder.Event(genericGwClient, corev1.EventTypeWarning, "SecretRefStatusFailed",
				fmt.Sprintf("Failed to handle secret ref status: %s", err))
			return ctrl.Result{}, err
		}
	}

	*/

	// Ensure deployment (create or update)
	_, err = r.ensureDeployment(ctx, genericGwClient, deployNsName)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.eventRecorder.Event(genericGwClient, corev1.EventTypeNormal, "DeploymentEnforced", "Enforced deployment")

	// Ensure Metrics (if set)
	err = enutils.EnsureMetrics(ctx,
		r.Client, r.Scheme,
		genericGwClient.GetMetrics(), genericGwClient)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.eventRecorder.Event(genericGwClient, corev1.EventTypeNormal, "MetricsEnforced", "Enforced metrics")

	return ctrl.Result{}, nil
}

// SetupWithManager register the GenericGwClientReconciler to the manager.
func (r *GenericGwClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Base resources managed by all protocol implementations.
	b := ctrl.NewControllerManagedBy(mgr).
		For(r.options.NewResource()).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ServiceAccount{})

	if r.options.CustomBuilderSetup != nil {
		b = r.options.CustomBuilderSetup(b)
	}

	return b.Complete(r)
}

func (r *GenericGwClientReconciler) ensureDeployment(ctx context.Context, gwClient GenericGwClient,
	depNsName types.NamespacedName) (*appsv1.Deployment, error) {
	dep := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      depNsName.Name,
		Namespace: depNsName.Namespace,
	}}

	op, err := resource.CreateOrUpdate(ctx, r.Client, &dep, func() error {
		return r.mutateFnGwClientDeployment(&dep, gwClient)
	})
	if err != nil {
		klog.Errorf("error while creating/updating deployment %q (operation: %s): %v", depNsName, op, err)
		return nil, err
	}

	klog.Infof("Deployment %q correctly enforced (operation: %s)", depNsName, op)
	return &dep, nil
}

func (r *GenericGwClientReconciler) mutateFnGwClientDeployment(deployment *appsv1.Deployment, gwClient GenericGwClient) error {
	// Forge metadata
	mapsutil.SmartMergeLabels(deployment, gwClient.GetDeploymentTemplate().Metadata.GetLabels())
	mapsutil.SmartMergeAnnotations(deployment, gwClient.GetDeploymentTemplate().Metadata.GetAnnotations())

	// Forge spec
	deployment.Spec = gwClient.GetDeploymentTemplate().Spec

	/************** SECRET MANAGEMENT LOGIC (if needed) **************

	if r.options.GetSecretRefStatus != nil && r.options.SecretVolumeName != "" {
		secretRef := r.options.GetSecretRefStatus(gwClient)
		if secretRef != nil {
			for i := range deployment.Spec.Template.Spec.Volumes {
				if deployment.Spec.Template.Spec.Volumes[i].Name == r.options.SecretVolumeName {
					deployment.Spec.Template.Spec.Volumes[i].Secret = &corev1.SecretVolumeSource{
						SecretName: secretRef.Name,
					}
					break
				}
			}
		} else {
			r.eventRecorder.Event(gwClient, corev1.EventTypeWarning, "MissingSecretRef", "Protocol keys secret not found")
		}
	}
	*/

	// Set Gateway client as owner of the deployment
	return controllerutil.SetControllerReference(gwClient, deployment, r.Scheme)
}

func (r *GenericGwClientReconciler) handleInternalEndpointStatus(ctx context.Context,
	gwClient GenericGwClient, dep *appsv1.Deployment) error {
	if dep == nil {
		gwClient.SetInternalEndpointStatus(nil)
		return nil
	}

	podsSelector := client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(gateway.ForgeActiveGatewayPodLabels())}
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(dep.Namespace), podsSelector); err != nil {
		klog.Errorf("Unable to list pods of deployment %s/%s: %v", dep.Namespace, dep.Name, err)
		return err
	}

	if len(podList.Items) != 1 {
		err := fmt.Errorf("wrong number of pods for deployment %s/%s: %d (must be 1)", dep.Namespace, dep.Name, len(podList.Items))
		klog.Error(err)
		return err
	}

	if podList.Items[0].Status.PodIP == "" {
		err := fmt.Errorf("pod %s/%s has no IP", podList.Items[0].Namespace, podList.Items[0].Name)
		klog.Error(err)
		return err
	}

	gwClient.SetInternalEndpointStatus(&networkingv1beta1.InternalGatewayEndpoint{
		IP:   ptr.To(networkingv1beta1.IP(podList.Items[0].Status.PodIP)),
		Node: &podList.Items[0].Spec.NodeName,
	})
	return nil
}
