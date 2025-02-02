// Copyright 2023 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"time"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
	goerrors "github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	capiv1beta1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kcpv1alpha1 "github.com/clastix/cluster-api-control-plane-provider-kamaji/api/v1alpha1"
)

// KamajiControlPlaneReconciler reconciles a KamajiControlPlane object.
type KamajiControlPlaneReconciler struct {
	client client.Client
}

//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kamajicontrolplanes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kamajicontrolplanes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kamajicontrolplanes/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch

func (r *KamajiControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) { //nolint:funlen,cyclop
	var err error

	var operation string

	now, log := time.Now(), ctrllog.FromContext(ctx)

	log.Info("reconciliation started")

	// Retrieving the KamajiControlPlane instance from the request
	kcp := kcpv1alpha1.KamajiControlPlane{}
	if err = r.client.Get(ctx, req.NamespacedName, &kcp); err != nil {
		if errors.IsNotFound(err) {
			log.Info("resource may have been deleted")

			return ctrl.Result{}, nil
		}

		log.Error(err, "unable to get kcpv1alpha1.KamajiControlPlane")

		return ctrl.Result{}, err //nolint:wrapcheck
	}
	// Updating the status of each reconciliation with the potential error status:
	// this is required to share with the user any potential error coming from Kamaji, or the Provider itself.
	defer func() {
		var failureMessage, failureReason string

		if err != nil {
			failureReason = operation
			failureMessage = err.Error()
		}

		_ = r.updateKamajiControlPlane(ctx, &kcp, func() {
			kcp.Status.FailureMessage = failureMessage
			kcp.Status.FailureReason = failureReason
		})
	}()
	// The ControlPlane must have an OwnerReference set from the Cluster controller, waiting for this condition:
	// https://cluster-api.sigs.k8s.io/developer/architecture/controllers/control-plane.html#relationship-to-other-cluster-api-types
	if len(kcp.GetOwnerReferences()) == 0 {
		log.Info("missing OwnerReference from the Cluster controller, waiting for it")

		return ctrl.Result{}, nil
	}
	// Retrieving the Cluster information
	cluster := capiv1beta1.Cluster{}
	cluster.SetName(kcp.GetOwnerReferences()[0].Name)
	cluster.SetNamespace(kcp.GetNamespace())

	if err = r.client.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, &cluster); err != nil {
		if errors.IsNotFound(err) {
			log.Info("capiv1beta1.Cluster resource may have been deleted, withdrawing reconciliation")

			return ctrl.Result{}, nil
		}

		operation = "GetCluster"

		log.Error(err, "unable to get capiv1beta1.Cluster")

		return ctrl.Result{}, err //nolint:wrapcheck
	}
	// Reconciling the Kamaji TenantControlPlane resource
	var tcp *kamajiv1alpha1.TenantControlPlane

	if tcp, err = r.createOrUpdateTenantControlPlane(ctx, cluster, kcp); err != nil {
		operation = "CreateOrUpdateTenantControlPlane"

		log.Error(err, "unable to create or update the TenantControlPlane instance")

		return ctrl.Result{}, err
	}
	// Waiting for the TenantControlPlane address: pay attention!
	//
	// This is still a work-in-progress and changing the Control Plane Controller contract.
	// Due to the given for granted concept that Control Plane and Worker nodes are on the same infrastructure,
	// we have to change the approach and wait for the advertised Control Plane endpoint, since Kamaji is offering a
	// Managed Kubernetes Service, although running as a regular pod.
	if len(tcp.Status.ControlPlaneEndpoint) == 0 {
		log.Info("Control Plane Endpoint is not yet available since unprocessed by Kamaji, enqueuing back")

		return ctrl.Result{}, nil
	}

	if err = r.patchCluster(ctx, cluster, tcp.Status.ControlPlaneEndpoint); err != nil {
		operation = "PatchCluster"

		log.Error(err, "cannot patch capiv1beta1.Cluster")

		return ctrl.Result{}, err
	}
	// Before continuing the Cluster object needs some validation, such as:
	// 1. an assigned Control Plane endpoint
	// 2. a ready infrastructure
	if len(cluster.Spec.ControlPlaneEndpoint.Host) == 0 {
		log.Info("capiv1beta1.Cluster Control Plane endpoint still unprocessed, enqueuing back")

		return ctrl.Result{Requeue: true}, nil
	}

	if !cluster.Status.InfrastructureReady {
		log.Info("capiv1beta1.Cluster infrastructure is not yet ready, enqueuing back")

		return ctrl.Result{Requeue: true}, nil
	}

	if tcp.Status.Kubernetes.Version.Status == nil {
		log.Info("kcpv1alpha1.KamajiControlPlane is not yet initialized, enqueuing back")

		return ctrl.Result{Requeue: true}, r.updateKamajiControlPlane(ctx, &kcp, func() {
			kcp.Status.Initialized = false
		})
	}
	// KamajiControlPlane has been initialized
	if err = r.updateKamajiControlPlane(ctx, &kcp, func() {
		kcp.Status.Initialized = true
	}); err != nil {
		operation = "UpdateKamajiControlPlaneAsInitialized"

		log.Error(err, "unable to set kcpv1alpha1.KamajiControlPlane as initialized")

		return ctrl.Result{}, err
	}
	// Updating KamajiControlPlane ready status, along with scaling values
	err = r.updateKamajiControlPlane(ctx, &kcp, func() {
		kcp.Status.Ready = *tcp.Status.Kubernetes.Version.Status == kamajiv1alpha1.VersionReady || *tcp.Status.Kubernetes.Version.Status == kamajiv1alpha1.VersionUpgrading
		kcp.Status.ReadyReplicas = tcp.Status.Kubernetes.Deployment.ReadyReplicas
		kcp.Status.Replicas = tcp.Status.Kubernetes.Deployment.Replicas
		kcp.Status.Selector = metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: kcp.GetLabels()})
		kcp.Status.UnavailableReplicas = tcp.Status.Kubernetes.Deployment.UnavailableReplicas
		kcp.Status.UpdatedReplicas = tcp.Status.Kubernetes.Deployment.UpdatedReplicas
		kcp.Status.Version = tcp.Status.Kubernetes.Version.Version
	})
	if err != nil {
		operation = "UpdateKamajiControlPlaneAsReady"

		log.Error(err, "unable to set kcpv1alpha1.KamajiControlPlane as ready")

		return ctrl.Result{}, err
	}
	// KamajiControlPlane must be considered ready before replicating required resources
	if !kcp.Status.Ready {
		log.Info("kcpv1alpha1.KamajiControlPlane is not yet ready, enqueuing back")

		return ctrl.Result{Requeue: true}, nil
	}

	var result ctrl.Result

	result, err = r.createRequiredResources(ctx, cluster, kcp, tcp)
	if err != nil {
		operation = "CreateRequiredResources"

		log.Error(err, "unable to satisfy Secrets contract")

		return ctrl.Result{}, err
	}

	log.Info("reconciliation completed", "duration", time.Since(now).String())

	return result, nil
}

func (r *KamajiControlPlaneReconciler) updateKamajiControlPlane(ctx context.Context, kcp *kcpv1alpha1.KamajiControlPlane, modifierFn func()) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.client.Get(ctx, types.NamespacedName{Name: kcp.Name, Namespace: kcp.Namespace}, kcp); err != nil {
			return err //nolint:wrapcheck
		}

		modifierFn()

		return r.client.Status().Update(ctx, kcp) //nolint:wrapcheck
	})
	if err != nil {
		return goerrors.Wrap(err, "cannot update KamajiControlPlane resource")
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KamajiControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.client = mgr.GetClient()
	//nolint:wrapcheck
	return ctrl.NewControllerManagedBy(mgr).
		For(&kcpv1alpha1.KamajiControlPlane{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
			return len(object.GetOwnerReferences()) > 0
		}))).
		Owns(&kamajiv1alpha1.TenantControlPlane{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
