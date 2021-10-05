package reconciler

import (
	"context"
	"os"

	"github.com/pkg/errors"
	"github.com/vmware-tanzu/cluster-api-provider-byoh/agent/cloudinit"
	"github.com/vmware-tanzu/cluster-api-provider-byoh/agent/registration"
	infrastructurev1alpha4 "github.com/vmware-tanzu/cluster-api-provider-byoh/apis/infrastructure/v1alpha4"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	"sigs.k8s.io/cluster-api/api/v1alpha4"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/kube-vip/kube-vip/pkg/vip"
)

type HostReconciler struct {
	Client         client.Client
	CmdRunner      cloudinit.ICmdRunner
	FileWriter     cloudinit.IFileWriter
	TemplateParser cloudinit.ITemplateParser
}

const (
	bootstrapSentinelFile = "/run/cluster-api/bootstrap-success.complete"
	KubeadmResetCommand   = "kubeadm reset --force"
)

func (r *HostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)
	log.WithValues("byoHost ", req.Name)
	log.Info("Reconciling byohost...")

	// Fetch the ByoHost instance.
	byoHost := &infrastructurev1alpha4.ByoHost{}
	err := r.Client.Get(ctx, req.NamespacedName, byoHost)
	if err != nil {
		klog.Errorf("error getting ByoHost %s in namespace %s, err=%v", req.NamespacedName.Namespace, req.NamespacedName.Name, err)
		return ctrl.Result{}, err
	}

	helper, _ := patch.NewHelper(byoHost, r.Client)
	defer func() {
		if err = helper.Patch(ctx, byoHost); err != nil && reterr == nil {
			klog.Errorf("failed to patch byohost, err=%v", err)
			reterr = err
		}
	}()

	// Check for host cleanup annotation
	hostAnnotations := byoHost.GetAnnotations()
	_, ok := hostAnnotations[infrastructurev1alpha4.HostCleanupAnnotation]
	if ok {
		err = r.hostCleanUp(ctx, byoHost)
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Handle deleted machines
	if !byoHost.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, byoHost)
	}
	return r.reconcileNormal(ctx, byoHost)
}

func (r *HostReconciler) reconcileNormal(ctx context.Context, byoHost *infrastructurev1alpha4.ByoHost) (ctrl.Result, error) {
	if byoHost.Status.MachineRef == nil {
		klog.Info("Machine ref not yet set")
		conditions.MarkFalse(byoHost, infrastructurev1alpha4.K8sNodeBootstrapSucceeded, infrastructurev1alpha4.WaitingForMachineRefReason, v1alpha4.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	if byoHost.Spec.BootstrapSecret == nil {
		klog.Info("BootstrapDataSecret not ready")
		conditions.MarkFalse(byoHost, infrastructurev1alpha4.K8sNodeBootstrapSucceeded, infrastructurev1alpha4.BootstrapDataSecretUnavailableReason, v1alpha4.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	if !conditions.IsTrue(byoHost, infrastructurev1alpha4.K8sNodeBootstrapSucceeded) {
		bootstrapScript, err := r.getBootstrapScript(ctx, byoHost.Spec.BootstrapSecret.Name, byoHost.Spec.BootstrapSecret.Namespace)
		if err != nil {
			klog.Errorf("error getting bootstrap script, err=%v", err)
			return ctrl.Result{}, err
		}
		err = r.bootstrapK8sNode(bootstrapScript, byoHost)
		if err != nil {
			klog.Errorf("error in bootstrapping k8s node, err=%v", err)
			_ = r.resetNode()
			conditions.MarkFalse(byoHost, infrastructurev1alpha4.K8sNodeBootstrapSucceeded, infrastructurev1alpha4.CloudInitExecutionFailedReason, v1alpha4.ConditionSeverityError, "")
			return ctrl.Result{}, err
		}
		klog.Info("k8s node successfully bootstrapped")

		conditions.MarkTrue(byoHost, infrastructurev1alpha4.K8sNodeBootstrapSucceeded)
	}

	return ctrl.Result{}, nil
}

func (r *HostReconciler) reconcileDelete(ctx context.Context, byoHost *infrastructurev1alpha4.ByoHost) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

func (r *HostReconciler) getBootstrapScript(ctx context.Context, dataSecretName, namespace string) (string, error) {
	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{Name: dataSecretName, Namespace: namespace}, secret)
	if err != nil {
		return "", err
	}

	bootstrapSecret := string(secret.Data["value"])
	return bootstrapSecret, nil
}

func (r *HostReconciler) SetupWithManager(ctx context.Context, mgr manager.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1alpha4.ByoHost{}).
		WithEventFilter(predicates.ResourceNotPaused(ctrl.LoggerFrom(ctx))).
		Complete(r)
}

func (r HostReconciler) hostCleanUp(ctx context.Context, byoHost *infrastructurev1alpha4.ByoHost) error {
	err := r.resetNode()
	if err != nil {
		return err
	}

	klog.Info("Removing the bootstrap sentinel file...")
	if _, err := os.Stat(bootstrapSentinelFile); !os.IsNotExist(err) {
		err := os.Remove(bootstrapSentinelFile)
		if err != nil {
			return errors.Wrapf(err, "failed to delete sentinel file %s", bootstrapSentinelFile)
		}
	}

	if IP, ok := byoHost.Annotations[infrastructurev1alpha4.EndPointIPAnnotation]; ok {
		network, err := vip.NewConfig(IP, registration.LocalHostRegistrar.ByoHostInfo.DefaultNetworkName, false)
		if err == nil {
			err := network.DeleteIP()
			if err != nil {
				return err
			}
		}
	}

	// Remove host reservation.
	byoHost.Status.MachineRef = nil

	// Remove cluster-name label
	delete(byoHost.Labels, v1alpha4.ClusterLabelName)

	// Remove the EndPointIP annotation
	delete(byoHost.Annotations, infrastructurev1alpha4.EndPointIPAnnotation)

	// Remove the cleanup annotation
	delete(byoHost.Annotations, infrastructurev1alpha4.HostCleanupAnnotation)

	// Remove the cluster version annotation
	delete(byoHost.Annotations, infrastructurev1alpha4.ClusterVersionAnnotation)

	conditions.MarkFalse(byoHost, infrastructurev1alpha4.K8sNodeBootstrapSucceeded, infrastructurev1alpha4.K8sNodeAbsentReason, v1alpha4.ConditionSeverityInfo, "")
	return nil
}

func (r *HostReconciler) resetNode() error {
	klog.Info("Running kubeadm reset...")

	err := r.CmdRunner.RunCmd(KubeadmResetCommand)
	if err != nil {
		return errors.Wrapf(err, "failed to exec kubeadm reset")
	}

	klog.Info("Kubernetes Node reset")
	return nil
}

func (r *HostReconciler) bootstrapK8sNode(bootstrapScript string, byoHost *infrastructurev1alpha4.ByoHost) error {
	return cloudinit.ScriptExecutor{
		WriteFilesExecutor:    r.FileWriter,
		RunCmdExecutor:        r.CmdRunner,
		ParseTemplateExecutor: r.TemplateParser}.Execute(bootstrapScript)
}
