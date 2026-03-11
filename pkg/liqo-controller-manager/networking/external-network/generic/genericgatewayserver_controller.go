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
	"github.com/liqotech/liqo/pkg/utils"
	mapsutil "github.com/liqotech/liqo/pkg/utils/maps"
	"github.com/liqotech/liqo/pkg/utils/resource"
)

// GenericGwServer defines the interface that a protocol-specific GenericGwServer must implement.
type GenericGwServer interface {
	client.Object
	GetDeploymentTemplate() *networkingv1beta1.DeploymentTemplate
	GetServiceTemplate() *networkingv1beta1.ServiceTemplate
	GetMetrics() *networkingv1beta1.Metrics
	GetSecretRef() corev1.LocalObjectReference

	SetSecretRefStatus(ref *corev1.ObjectReference)
	SetEndpointStatus(endpoint *networkingv1beta1.EndpointStatus)
	SetInternalEndpointStatus(endpoint *networkingv1beta1.InternalGatewayEndpoint)
}

// OptionServer is a collection of simple POSSIBLE callbacks used to manage protocol-specific data
// that can be different for each protocol.
//
// These callbacks depends on the protocol implementation and on the involved custom resources.
//
// e.g. Wireguard uses the PublicKey CR to store the public key of the remote peer, so it needs
// a callback to access that field; for an example, check the func "EnsureKeysSecret" 
// in pkg/liqo-controller-manager/networking/external-network/wireguard/wggatewayclient_controller.go
// and implemented in /pkg/liqo-controller-manager/networking/external-network/wireguard/utils.go
// for an example.
type OptionServer struct {
	// NewResource returns a pointer to a new empty instance of the protocol-specific GenericGwClient.
	NewResource           func() GenericGwServer
	// EnsureSecret is a callback used to ensure the presence of the secret containing the protocol keys, if needed.
	EnsureSecret          func(context.Context, client.Client, GenericGwServer) error
	// CheckExistingSecret is a callback used to check the presence and correctness of the secret containing the protocol keys, if needed.
	CheckExistingSecret   func(context.Context, client.Client, string, string, metav1.Object) error
	// GetSecretRefStatus is a callback used to get the reference to the secret containing the protocol keys, if needed.
	GetSecretRefStatus    func(GenericGwServer) *corev1.ObjectReference
	// HandleSecretRefStatus is a callback used to handle the status of the secret reference, if needed.
	HandleSecretRefStatus func(context.Context, client.Client, GenericGwServer) error
	SecretVolumeName      string
	// CustomBuilderSetup is callback used to customize the builder of the controller, if needed (e.g. to watch additional resources).
	CustomBuilderSetup    func(*builder.Builder) *builder.Builder
}

// GenericGwServerReconciler manage GenericGwServer lifecycle.
type GenericGwServerReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	clusterRoleName string

	eventRecorder record.EventRecorder
	options       OptionServer
}

// NewGenericGwServerReconciler returns a new GenericGwServerReconciler.
func NewGenericGwServerReconciler(cl client.Client, s *runtime.Scheme,
	recorder record.EventRecorder,
	clusterRoleName string, opts OptionServer) *GenericGwServerReconciler {
	return &GenericGwServerReconciler{
		Client:          cl,
		Scheme:          s,
		clusterRoleName: clusterRoleName,

		eventRecorder: recorder,
		options:       opts,
	}
}

// Reconcile manage GenericGwServer lifecycle.
func (r *GenericGwServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	genericGwServer := r.options.NewResource()
	if err = r.Get(ctx, req.NamespacedName, genericGwServer); err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("Gateway server %q not found", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		klog.Errorf("Unable to get the Gateway server %q: %v", req.NamespacedName, err)
		return ctrl.Result{}, err
	}

	if !genericGwServer.GetDeletionTimestamp().IsZero() {
		if controllerutil.ContainsFinalizer(genericGwServer, consts.ClusterRoleBindingFinalizer) {
			if err = enutils.DeleteClusterRoleBinding(ctx, r.Client, genericGwServer); err != nil {
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(genericGwServer, consts.ClusterRoleBindingFinalizer)
			if err = r.Update(ctx, genericGwServer); err != nil {
				klog.Errorf("Unable to remove finalizer %q from Gateway server %q: %v",
					consts.ClusterRoleBindingFinalizer, req.NamespacedName, err)
				return ctrl.Result{}, err
			}
		}

		// Resource is deleting and child resources are deleted as well by garbage collector. Nothing to do.
		return ctrl.Result{}, nil
	}

	originalGenericGwServer := genericGwServer.DeepCopyObject().(GenericGwServer)

	// Ensure ServiceAccount and ClusterRoleBinding (create or update)
	if err = enutils.EnsureServiceAccountAndClusterRoleBinding(ctx, r.Client, r.Scheme, genericGwServer.GetDeploymentTemplate(), genericGwServer,
		r.clusterRoleName); err != nil {
		return ctrl.Result{}, err
	}

	// update if the genericGwServer has been updated
	if !equality.Semantic.DeepEqual(originalGenericGwServer, genericGwServer) {
		if err := r.Update(ctx, genericGwServer); err != nil {
			return ctrl.Result{}, err
		}

		// we return here to avoid conflicts
		return ctrl.Result{}, nil
	}

	deployNsName := types.NamespacedName{Namespace: genericGwServer.GetNamespace(), Name: forge.GatewayResourceName(genericGwServer.GetName())}
	svcNsName := types.NamespacedName{Namespace: genericGwServer.GetNamespace(), Name: forge.GatewayResourceName(genericGwServer.GetName())}

	var deploy *appsv1.Deployment
	var d appsv1.Deployment
	err = r.Get(ctx, deployNsName, &d)
	switch {
	case apierrors.IsNotFound(err):
		deploy = nil
	case err != nil:
		klog.Errorf("Unable to get the deployment %q: %v", deployNsName, err)
		return ctrl.Result{}, err
	default:
		deploy = &d
	}

	// Handle status
	defer func() {
		newErr := r.Status().Update(ctx, genericGwServer)
		if newErr != nil {
			if err != nil {
				klog.Errorf("Error reconciling the Gateway server %q: %s", req.NamespacedName, err)
			}
			klog.Errorf("Unable to update the Gateway server status %q: %s", req.NamespacedName, newErr)
			err = newErr
			return
		}

		r.eventRecorder.Event(genericGwServer, corev1.EventTypeNormal, "Reconciled", "Gateway server reconciled")
	}()

	if err := r.handleEndpointStatus(ctx, genericGwServer, svcNsName, deploy); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.handleInternalEndpointStatus(ctx, genericGwServer, svcNsName, deploy); err != nil {
		klog.Errorf("Error while handling internal endpoint status: %v", err)
		r.eventRecorder.Event(genericGwServer, corev1.EventTypeWarning, "InternalEndpointStatusFailed",
			fmt.Sprintf("Failed to handle internal endpoint status: %s", err))
		return ctrl.Result{}, err
	}


	/************** SECRET MANAGEMENT LOGIC (if needed) **************

	// If a secret has not been provided in the gateway specification, the controller is in charge of generating a secret with the protocol keys.
	secretRef := genericGwServer.GetSecretRef()
	if secretRef.Name == "" {
		if r.options.EnsureSecret != nil {
			if err = r.options.EnsureSecret(ctx, r.Client, genericGwServer); err != nil {
				r.eventRecorder.Event(genericGwServer, corev1.EventTypeWarning, "KeysSecretEnforcedFailed", "Failed to enforce keys secret")
				return ctrl.Result{}, err
			}
			r.eventRecorder.Event(genericGwServer, corev1.EventTypeNormal, "KeysSecretEnforced", "Enforced keys secret")
		}
	} else {
		// Check that the secret exists and ensure is correctly labeled
		if r.options.CheckExistingSecret != nil {
			if err = r.options.CheckExistingSecret(ctx, r.Client, secretRef.Name, genericGwServer.GetNamespace(), genericGwServer); err != nil {
				r.eventRecorder.Event(genericGwServer, corev1.EventTypeWarning, "KeysSecretCheckFailed", fmt.Sprintf("Failed to check keys secret: %s", err))
				return ctrl.Result{}, err
			}
			r.eventRecorder.Event(genericGwServer, corev1.EventTypeNormal, "KeysSecretChecked", "Checked keys secret")
		}
	}

	if r.options.HandleSecretRefStatus != nil {
		if err := r.options.HandleSecretRefStatus(ctx, r.Client, genericGwServer); err != nil {
			klog.Errorf("Error while handling secret ref status: %v", err)
			r.eventRecorder.Event(genericGwServer, corev1.EventTypeWarning, "SecretRefStatusFailed",
				fmt.Sprintf("Failed to handle secret ref status: %s", err))
			return ctrl.Result{}, err
		}
	}

	*/

	// Ensure deployment (create or update)
	_, err = r.ensureDeployment(ctx, genericGwServer, deployNsName)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.eventRecorder.Event(genericGwServer, corev1.EventTypeNormal, "DeploymentEnforced", "Enforced deployment")

	// Ensure service (create or update)
	_, err = r.ensureService(ctx, genericGwServer, svcNsName)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.eventRecorder.Event(genericGwServer, corev1.EventTypeNormal, "ServiceEnforced", "Enforced service")

	// Ensure Metrics (if set)
	err = enutils.EnsureMetrics(ctx,
		r.Client, r.Scheme,
		genericGwServer.GetMetrics(), genericGwServer)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.eventRecorder.Event(genericGwServer, corev1.EventTypeNormal, "MetricsEnforced", "Enforced metrics")

	return ctrl.Result{}, nil
}

// SetupWithManager register the GenericGwServerReconciler to the manager.
func (r *GenericGwServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Base resources managed by all protocol implementations.
	b := ctrl.NewControllerManagedBy(mgr).
		For(r.options.NewResource()).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{})

	if r.options.CustomBuilderSetup != nil {
		b = r.options.CustomBuilderSetup(b)
	}

	return b.Complete(r)
}

func (r *GenericGwServerReconciler) ensureDeployment(ctx context.Context, genericGwServer GenericGwServer,
	depNsName types.NamespacedName) (*appsv1.Deployment, error) {
	dep := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      depNsName.Name,
		Namespace: depNsName.Namespace,
	}}

	op, err := resource.CreateOrUpdate(ctx, r.Client, &dep, func() error {
		return r.mutateFnGenericGwServerDeployment(&dep, genericGwServer)
	})
	if err != nil {
		klog.Errorf("error while creating/updating deployment %q (operation: %s): %v", depNsName, op, err)
		return nil, err
	}

	klog.Infof("Deployment %q correctly enforced (operation: %s)", depNsName, op)
	return &dep, nil
}

func (r *GenericGwServerReconciler) ensureService(ctx context.Context, genericGwServer GenericGwServer,
	svcNsName types.NamespacedName) (*corev1.Service, error) {
	svc := corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      svcNsName.Name,
		Namespace: svcNsName.Namespace,
	}}

	op, err := resource.CreateOrUpdate(ctx, r.Client, &svc, func() error {
		return r.mutateFnGenericGwServerService(&svc, genericGwServer)
	})
	if err != nil {
		klog.Errorf("error while creating/updating service %q (operation: %s): %v", svcNsName, op, err)
		return nil, err
	}

	klog.Infof("Service %q correctly enforced (operation: %s)", svcNsName, op)
	return &svc, nil
}

func (r *GenericGwServerReconciler) mutateFnGenericGwServerDeployment(deployment *appsv1.Deployment, genericGwServer GenericGwServer) error {
	// Forge metadata
	mapsutil.SmartMergeLabels(deployment, genericGwServer.GetDeploymentTemplate().Metadata.GetLabels())
	mapsutil.SmartMergeAnnotations(deployment, genericGwServer.GetDeploymentTemplate().Metadata.GetAnnotations())

	// Forge spec
	deployment.Spec = genericGwServer.GetDeploymentTemplate().Spec

	if r.options.GetSecretRefStatus != nil && r.options.SecretVolumeName != "" {
		secretRef := r.options.GetSecretRefStatus(genericGwServer)
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
			r.eventRecorder.Event(genericGwServer, corev1.EventTypeWarning, "MissingSecretRef", "Protocol keys secret not found")
		}
	}

	// Set Gateway server as owner of the deployment
	return controllerutil.SetControllerReference(genericGwServer, deployment, r.Scheme)
}

func (r *GenericGwServerReconciler) mutateFnGenericGwServerService(service *corev1.Service, genericGwServer GenericGwServer) error {
	// Forge metadata
	mapsutil.SmartMergeLabels(service, genericGwServer.GetServiceTemplate().Metadata.GetLabels())
	mapsutil.SmartMergeAnnotations(service, genericGwServer.GetServiceTemplate().Metadata.GetAnnotations())

	// Forge spec
	serviceClassName := service.Spec.LoadBalancerClass
	service.Spec = genericGwServer.GetServiceTemplate().Spec
	if genericGwServer.GetServiceTemplate().Spec.LoadBalancerClass == nil {
		service.Spec.LoadBalancerClass = serviceClassName
	}

	// Set Gateway server as owner of the service
	return controllerutil.SetControllerReference(genericGwServer, service, r.Scheme)
}

func (r *GenericGwServerReconciler) handleEndpointStatus(ctx context.Context, genericGwServer GenericGwServer,
	svcNsName types.NamespacedName, dep *appsv1.Deployment) error {
	if dep == nil {
		genericGwServer.SetEndpointStatus(nil)
		return nil
	}

	// Handle Gateway server Service
	var service corev1.Service
	err := r.Get(ctx, svcNsName, &service)
	if err != nil {
		klog.Error(err) // raise an error also if service NotFound
		return err
	}

	// Put service endpoint in Gateway server status
	var endpointStatus *networkingv1beta1.EndpointStatus
	switch service.Spec.Type {
	case corev1.ServiceTypeClusterIP:
		endpointStatus, err = r.forgeEndpointStatusClusterIP(&service)
	case corev1.ServiceTypeNodePort:
		endpointStatus, _, err = r.forgeEndpointStatusNodePort(ctx, &service, dep)
	case corev1.ServiceTypeLoadBalancer:
		endpointStatus, err = r.forgeEndpointStatusLoadBalancer(&service)
	default:
		err = fmt.Errorf("service type %q not supported for Gateway server Service %q", service.Spec.Type, svcNsName)
		klog.Error(err)
		genericGwServer.SetEndpointStatus(nil) // we empty the endpoint status to avoid misaligned spec and status
	}

	if err != nil {
		return err
	}

	genericGwServer.SetEndpointStatus(endpointStatus)

	return nil
}

func (r *GenericGwServerReconciler) forgeEndpointStatusClusterIP(service *corev1.Service) (*networkingv1beta1.EndpointStatus, error) {
	if len(service.Spec.Ports) == 0 {
		err := fmt.Errorf("service %s/%s has no ports", service.Namespace, service.Name)
		klog.Error(err)
		return nil, err
	}

	port := service.Spec.Ports[0].Port
	protocol := &service.Spec.Ports[0].Protocol
	addresses := service.Spec.ClusterIPs

	return &networkingv1beta1.EndpointStatus{
		Protocol:  protocol,
		Port:      port,
		Addresses: addresses,
	}, nil
}

func (r *GenericGwServerReconciler) forgeEndpointStatusNodePort(ctx context.Context, service *corev1.Service,
	dep *appsv1.Deployment) (*networkingv1beta1.EndpointStatus, *networkingv1beta1.InternalGatewayEndpoint, error) {
	if len(service.Spec.Ports) == 0 {
		err := fmt.Errorf("service %s/%s has no ports", service.Namespace, service.Name)
		klog.Error(err)
		return nil, nil, err
	}

	port := service.Spec.Ports[0].NodePort
	protocol := &service.Spec.Ports[0].Protocol

	podsSelector := client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(gateway.ForgeActiveGatewayPodLabels())}
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(dep.Namespace), podsSelector); err != nil {
		klog.Errorf("Unable to list pods of deployment %s/%s: %v", dep.Namespace, dep.Name, err)
		return nil, nil, err
	}

	if len(podList.Items) != 1 {
		err := fmt.Errorf("wrong number of pods for deployment %s/%s: %d (must be 1)", dep.Namespace, dep.Name, len(podList.Items))
		klog.Error(err)
		return nil, nil, err
	}

	pod := &podList.Items[0]

	node := &corev1.Node{}
	err := r.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node)
	if err != nil && !apierrors.IsNotFound(err) {
		klog.Errorf("Unable to get node %q: %v", pod.Spec.NodeName, err)
		return nil, nil, err
	}

	addresses := make([]string, 1)
	if utils.IsNodeReady(node) {
		if addresses[0], err = utils.GetAddress(node); err != nil {
			klog.Errorf("Unable to get address of node %q: %v", pod.Spec.NodeName, err)
			return nil, nil, err
		}
	}

	internalAddress := pod.Status.PodIP
	if internalAddress == "" {
		err := fmt.Errorf("pod %s/%s has no IP", pod.Namespace, pod.Name)
		klog.Error(err)
		return nil, nil, err
	}

	return &networkingv1beta1.EndpointStatus{
			Protocol:  protocol,
			Port:      port,
			Addresses: addresses,
		}, &networkingv1beta1.InternalGatewayEndpoint{
			IP:   ptr.To(networkingv1beta1.IP(internalAddress)),
			Node: &pod.Spec.NodeName,
		}, nil
}

func (r *GenericGwServerReconciler) forgeEndpointStatusLoadBalancer(service *corev1.Service) (*networkingv1beta1.EndpointStatus, error) {
	if len(service.Spec.Ports) == 0 {
		err := fmt.Errorf("service %s/%s has no ports", service.Namespace, service.Name)
		klog.Error(err)
		return nil, err
	}

	port := service.Spec.Ports[0].Port
	protocol := &service.Spec.Ports[0].Protocol

	var addresses []string
	for i := range service.Status.LoadBalancer.Ingress {
		if hostName := service.Status.LoadBalancer.Ingress[i].Hostname; hostName != "" {
			addresses = append(addresses, hostName)
		}
		if ip := service.Status.LoadBalancer.Ingress[i].IP; ip != "" {
			addresses = append(addresses, ip)
		}
	}

	return &networkingv1beta1.EndpointStatus{
		Protocol:  protocol,
		Port:      port,
		Addresses: addresses,
	}, nil
}

func (r *GenericGwServerReconciler) handleInternalEndpointStatus(ctx context.Context, genericGwServer GenericGwServer,
	svcNsName types.NamespacedName, dep *appsv1.Deployment) error {
	if dep == nil {
		genericGwServer.SetInternalEndpointStatus(nil)
		return nil
	}

	var service corev1.Service
	err := r.Get(ctx, svcNsName, &service)
	if err != nil {
		klog.Error(err) // raise an error also if service NotFound
		return err
	}

	_, ige, err := r.forgeEndpointStatusNodePort(ctx, &service, dep)
	if err != nil {
		return err
	}

	genericGwServer.SetInternalEndpointStatus(ige)
	return nil
}
