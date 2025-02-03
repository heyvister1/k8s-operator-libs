/*
Copyright 2022 NVIDIA CORPORATION & AFFILIATES

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

package requestor

import (
	"context"
	"errors"
	"fmt"

	//nolint:depguard
	maintenancev1alpha1 "github.com/Mellanox/maintenance-operator/api/v1alpha1"
	"github.com/NVIDIA/k8s-operator-libs/api/upgrade/v1alpha1"
	"github.com/NVIDIA/k8s-operator-libs/pkg/consts"
	"github.com/NVIDIA/k8s-operator-libs/pkg/upgrade/base"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	MaintenanceOPFinalizerName      = "maintenance.nvidia.com/finalizer"
	MaintenanceOPDefaultRequestorID = "nvidia.network.operator"
	MaintenanceOPEvictionGPU        = "nvidia.com/gpu-*"
	MaintenanceOPEvictionRDMA       = "nvidia.com/rdma*"
)

var (
	MaintenanceOPControllerName       = "node-maintenance-requestor"
	ErrNodeMaintenanceUpgradeDisabled = errors.New("node maintenance upgrade mode is disabled")
)

type UpgradeRequestorQptions struct {
	// UseMaintenanceOperator enables requestor updrade mode
	UseMaintenanceOperator bool
	// TODO: Do we need this flag?
	MaintenanceOPRequestorCordon   bool
	MaintenanceOPRequestorID       string
	MaintenanceOPRequestorNS       string
	MaintenanceOPPodEvictionFilter []maintenancev1alpha1.PodEvictionFiterEntry
}

// UpgradeManagerImpl contains concrete implementations for distinct requestor
// (e.g. maintenance OP) upgrade mode
type UpgradeManagerImpl struct {
	*base.CommonUpgradeManagerImpl
	opts UpgradeRequestorQptions
}

// NewClusterUpgradeStateManager creates a new instance of UpgradeManagerImpl
func NewRequestorUpgradeManagerImpl(
	ctx context.Context,
	common *base.CommonUpgradeManagerImpl,
	opts UpgradeRequestorQptions) (base.ProcessNodeStateManager, error) {
	if !opts.UseMaintenanceOperator {
		common.Log.V(consts.LogLevelInfo).Info("node maintenance upgrade mode is disabled")
		return nil, ErrNodeMaintenanceUpgradeDisabled
	}
	manager := &UpgradeManagerImpl{
		opts:                     opts,
		CommonUpgradeManagerImpl: common,
	}

	return manager, nil
}

// ProcessUpgradeRequiredNodes processes UpgradeStateUpgradeRequired nodes and moves them to UpgradeStateCordonRequired
// until the limit on max parallel upgrades is reached.
func (m *UpgradeManagerImpl) ProcessUpgradeRequiredNodes(
	ctx context.Context, currentClusterState *base.ClusterUpgradeState,
	upgradePolicy *v1alpha1.DriverUpgradePolicySpec) error {
	m.Log.V(consts.LogLevelInfo).Info("ProcessUpgradeRequiredNodes")

	SetDefaultNodeMaintenance(m.opts, upgradePolicy)
	for _, nodeState := range currentClusterState.NodeStates[base.UpgradeStateUpgradeRequired] {
		err := m.CreateNodeMaintenance(ctx, nodeState)
		if err != nil {
			m.Log.V(consts.LogLevelError).Error(err, "failed to create nodeMaintenance")
			return err
		}

		err = m.SetUpgradeRequestorModeAnnotation(ctx, nodeState.Node.Name)
		if err != nil {
			return fmt.Errorf("failed annotate node for 'upgrade-requestor-mode'. %v", err)
		}
		// update node state to 'node-maintenance-required'
		err = m.NodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, nodeState.Node,
			base.UpgradeStateNodeMaintenanceRequired)
		if err != nil {
			return fmt.Errorf("failed to update node state. %v", err)
		}
	}

	return nil
}

// ProcessPostMaintenanceNodes processes UpgradeStatePostMaintenanceRequired
// by adding UpgradeStatePodRestartRequired under existing UpgradeStatePodRestartRequired nodes list.
// the motivation is later to replace ProcessPodRestartNodes to a generic post node operation
// while using maintenance operator (e.g. post-maintenance-required)
func (m *UpgradeManagerImpl) ProcessPostMaintenanceNodes(ctx context.Context,
	currentClusterState *base.ClusterUpgradeState) error {
	m.Log.V(consts.LogLevelInfo).Info("ProcessPostMaintenanceNodes")
	for _, nodeState := range currentClusterState.NodeStates[base.UpgradeStateNodeMaintenanceRequired] {
		if nodeState.NodeMaintenance == nil {
			if _, ok := nodeState.Node.Annotations[base.GetUpgradeRequestorModeAnnotationKey()]; !ok {
				m.Log.V(consts.LogLevelWarning).Info("missing node maintenance obj for node", nodeState.Node.Name)
			}
			continue
		}
		nm := &maintenancev1alpha1.NodeMaintenance{}
		err := runtime.DefaultUnstructuredConverter.FromUnstructured(nodeState.NodeMaintenance.Object, nm)
		if err != nil {
			return fmt.Errorf("failed to convert node mantenance obj. %v", err)
		}
		cond := meta.FindStatusCondition(nm.Status.Conditions, maintenancev1alpha1.ConditionReasonReady)
		if cond != nil {
			if cond.Reason == maintenancev1alpha1.ConditionReasonReady {
				m.Log.V(consts.LogLevelDebug).Info("node maintenance operation completed", nm.Spec.NodeName, cond.Reason)
				// update node state to 'pod-restart-required'
				err = m.NodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, nodeState.Node,
					base.UpgradeStatePodRestartRequired)
				if err != nil {
					return fmt.Errorf("failed to update node state. %v", err)
				}
			}
		}
	}
	currentClusterState.NodeStates[base.UpgradeStatePodRestartRequired] =
		append(currentClusterState.NodeStates[base.UpgradeStatePodRestartRequired],
			currentClusterState.NodeStates[base.UpgradeStateNodeMaintenanceRequired]...)
	// clear UpgradeStateNodeMaintenanceRequired list once copied to ProcessPostMaintenanceNodes
	currentClusterState.NodeStates[base.UpgradeStateNodeMaintenanceRequired] = nil

	return nil
}

func (m *UpgradeManagerImpl) ProcessUncordonRequiredNodes(
	ctx context.Context, currentClusterState *base.ClusterUpgradeState) error {
	m.Log.V(consts.LogLevelInfo).Info("ProcessUncordonRequiredNodes")

	for _, nodeState := range currentClusterState.NodeStates[base.UpgradeStateUncordonRequired] {
		m.Log.V(consts.LogLevelDebug).Info("deleting node maintenance",
			nodeState.NodeMaintenance.GetName(), nodeState.NodeMaintenance.GetNamespace())
		err := m.DeleteNodeMaintenance(ctx, nodeState)
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				m.Log.V(consts.LogLevelWarning).Error(
					err, "Node uncordon failed", "node", nodeState.Node)
				return err
			}
			// this means that node maintenance obj has been deleted
			err = m.NodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, nodeState.Node,
				base.UpgradeStateDone)
			if err != nil {
				return fmt.Errorf("failed to update node state. %v", err)
			}
			//TODO: remove requestor upgrade annotation
			err := m.NodeUpgradeStateProvider.ChangeNodeUpgradeAnnotation(ctx,
				nodeState.Node, base.GetUpgradeRequestorModeAnnotationKey(), "null")
			if err != nil {
				return fmt.Errorf("failed to remove '%s' annotation . %v", base.GetUpgradeRequestorModeAnnotationKey(), err)
			}
		}
	}
	return nil
}

// SetUpgradeRequestorModeAnnotation will set upgrade-requestor-mode for specifying node upgrade mode flow
func (m *UpgradeManagerImpl) SetUpgradeRequestorModeAnnotation(ctx context.Context, nodeName string) error {
	node := &corev1.Node{}
	nodeKey := client.ObjectKey{
		Name: nodeName,
	}
	if err := m.K8sClient.Get(context.Background(), nodeKey, node); err != nil {
		return err
	}
	patchString := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q: "true"}}}`,
		base.GetUpgradeRequestorModeAnnotationKey()))
	patch := client.RawPatch(types.MergePatchType, patchString)
	err := m.K8sClient.Patch(ctx, node, patch)
	if err != nil {
		return err
	}
	m.Log.V(consts.LogLevelDebug).Info("Node annotated with upgrade-requestor-mode", "name", nodeName)

	return nil
}

// convertV1Alpha1ToMaintenance explicitly converts v1alpha1.DriverUpgradePolicySpec
// to maintenancev1alpha1.DrainSpec and maintenancev1alpha1.WaitForPodCompletionSpec and
func convertV1Alpha1ToMaintenance(upgradePolicy *v1alpha1.DriverUpgradePolicySpec) (*maintenancev1alpha1.DrainSpec,
	*maintenancev1alpha1.WaitForPodCompletionSpec) {
	if upgradePolicy == nil {
		return nil, nil
	}
	drainSpec := &maintenancev1alpha1.DrainSpec{}
	if upgradePolicy.DrainSpec != nil {
		drainSpec.Force = upgradePolicy.DrainSpec.Force
		drainSpec.PodSelector = upgradePolicy.DrainSpec.PodSelector
		//nolint:gosec // G115: suppress potential integer overflow conversion warning
		drainSpec.TimeoutSecond = int32(upgradePolicy.DrainSpec.TimeoutSecond)
		drainSpec.DeleteEmptyDir = upgradePolicy.DrainSpec.DeleteEmptyDir
	}
	if upgradePolicy.PodDeletion != nil {
		//TODO: propagate
		drainSpec.PodEvictionFilters = nil
	}
	podComplition := &maintenancev1alpha1.WaitForPodCompletionSpec{}
	if upgradePolicy.WaitForCompletion != nil {
		podComplition.PodSelector = upgradePolicy.WaitForCompletion.PodSelector
		//nolint:gosec // G115: suppress potential integer overflow conversion warning
		podComplition.TimeoutSecond = int32(upgradePolicy.WaitForCompletion.TimeoutSecond)
	}

	return drainSpec, podComplition
}
