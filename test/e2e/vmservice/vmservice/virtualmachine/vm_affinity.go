// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package virtualmachine

import (
	"context"
	"crypto/sha1" //nolint:gosec // used only to derive a stable resource name, not for security
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2eframework "k8s.io/kubernetes/test/e2e/framework"
	capiutil "sigs.k8s.io/cluster-api/util"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vspherepolv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-policy/api/v1alpha1"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	"github.com/vmware-tanzu/vm-operator/test/e2e/framework"
	"github.com/vmware-tanzu/vm-operator/test/e2e/infrastructure/vsphere/testbed"
	"github.com/vmware-tanzu/vm-operator/test/e2e/infrastructure/vsphere/vcenter"
	"github.com/vmware-tanzu/vm-operator/test/e2e/infrastructure/vsphere/wcp"
	"github.com/vmware-tanzu/vm-operator/test/e2e/manifestbuilders"
	"github.com/vmware-tanzu/vm-operator/test/e2e/utils"
	"github.com/vmware-tanzu/vm-operator/test/e2e/vmservice/common"
	e2eConfig "github.com/vmware-tanzu/vm-operator/test/e2e/vmservice/config"
	"github.com/vmware-tanzu/vm-operator/test/e2e/vmservice/consts"
	"github.com/vmware-tanzu/vm-operator/test/e2e/vmservice/lib/vmoperator"
	"github.com/vmware-tanzu/vm-operator/test/e2e/vmservice/skipper"
	"github.com/vmware-tanzu/vm-operator/test/e2e/vmservice/vmservice"
	"github.com/vmware-tanzu/vm-operator/test/e2e/wcpframework"
)

// vCenterTagName returns the vCenter tag name for a label key/value pair:
// "<key>:<value>". Byte-for-byte what
// pkg/providers/vsphere/virtualmachine.VCenterTagName computes; reimplemented
// here rather than imported so this e2e module does not pick up that
// package's production dependency graph for two one-line derivations.
func vCenterTagName(key, value string) string {
	return key + ":" + value
}

// tagResourceName returns the derived name of the Tag resource for a label
// key/value pair: "tag-" followed by the first 17 hex characters of the
// SHA-1 sum of "<key>:<value>". Byte-for-byte what
// pkg/providers/vsphere/virtualmachine.TagResourceName computes (see the
// doc comment on vCenterTagName for why this is reimplemented rather than
// imported).
func tagResourceName(key, value string) string {
	h := sha1.New() //nolint:gosec // used only to derive a stable resource name, not for security
	_, _ = io.WriteString(h, vCenterTagName(key, value))
	return "tag-" + hex.EncodeToString(h.Sum(nil))[:17]
}

// VMAffinityTagSpecInput is the input to VMAffinityTagSpec.
type VMAffinityTagSpecInput struct {
	ClusterProxy     wcpframework.WCPClusterProxyInterface
	Config           *e2eConfig.E2EConfig
	WCPClient        wcp.WorkloadManagementAPI
	ArtifactFolder   string
	WCPNamespaceName string
}

// VMAffinityTagSpec exercises the Tag CRD + Tag controller for affinity:
// a VM whose spec.affinity references a label it carries gets a vCenter
// Tag applied and a Tag resource created in its namespace, with the VM
// recorded as an owner; and a pre-existing label-only VM — one that
// carries the label but never references it via spec.affinity itself —
// gets tagged once some other VM's affinity references that label, driven
// by the VM controller's Tag watch rather than this VM's own reconcile.
//
// The feature is gated on Features.TaggingAPI, which has no Supervisor
// capability of its own. Tag CR ownership includes every pair
// spec.affinity references regardless of topologyKey, so this suite does
// not need a capability for that. It still gates on
// VMAffinityDuringExecutionCapabilityName because affinityFor and
// affinityForValues reference labels via a required term whose
// topologyKey is kubernetes.io/hostname, which the admission webhook only
// accepts once that capability is enabled (without it, only
// topology.kubernetes.io/zone is accepted).
func VMAffinityTagSpec(ctx context.Context, inputGetter func() VMAffinityTagSpecInput) {
	const (
		specName    = "vm-affinity-tag"
		vmKind      = "VirtualMachine"
		vmgKind     = "VirtualMachineGroup"
		affinityKey = "tier"
	)

	var (
		input           VMAffinityTagSpecInput
		config          *e2eConfig.E2EConfig
		clusterProxy    *common.VMServiceClusterProxy
		wcpClient       wcp.WorkloadManagementAPI
		svClusterClient ctrlclient.Client
		vCenterClient   *vim25.Client
		tagMgr          *tags.Manager

		linuxImageDisplayName string
		linuxVMIName          string

		vmNames []string

		vmgRootName    string
		vmgRootYaml    []byte
		vmgMemberNames []string
	)

	// affinityFor returns a spec.affinity referencing key:value via a
	// required VM-affinity term on the host topology, mirroring
	// createHostVMWithAffinityAndAntiAffinityFunc in vm_group.go. The
	// admission webhook only accepts kubernetes.io/hostname or
	// topology.kubernetes.io/zone as topologyKey (see the doc comment
	// above for why hostname, specifically, is what this suite needs).
	affinityFor := func(key, value string) *vmopv1.AffinitySpec {
		return &vmopv1.AffinitySpec{
			VMAffinity: &vmopv1.VMAffinitySpec{
				RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{key: value},
						},
						TopologyKey: corev1.LabelHostname,
					},
				},
			},
		}
	}

	// antiAffinityFor returns a spec.affinity referencing key:value via a
	// required VM-anti-affinity term on the zone topology — the
	// "VmToVmGroupsAntiAffinity-style" shape, which lets the declaring VM
	// reference a label pair it never carries itself. Tag CR ownership
	// includes every pair spec.affinity references regardless of topology
	// key; zone is used here only because the admission webhook only
	// accepts kubernetes.io/hostname or topology.kubernetes.io/zone.
	antiAffinityFor := func(key, value string) *vmopv1.AffinitySpec {
		return &vmopv1.AffinitySpec{
			VMAntiAffinity: &vmopv1.VMAntiAffinitySpec{
				RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{key: value},
						},
						TopologyKey: corev1.LabelTopologyZone,
					},
				},
			},
		}
	}

	// ensureVMInGroup adds vmName as a member of the suite's root
	// VirtualMachineGroup, creating the group on the first call and
	// applying the updated member list on every subsequent call —
	// mirroring the "adopt an existing VM" workflow in vm_group.go.
	// spec.affinity requires a non-empty spec.groupName (webhook rule
	// validateVMAffinity, independent of this feature), so every VM this
	// suite creates must belong to a group.
	ensureVMInGroup := func(vmName string) {
		GinkgoHelper()

		vmgMemberNames = append(vmgMemberNames, vmName)

		vmgParameters := manifestbuilders.VirtualMachineGroupYaml{
			Namespace: input.WCPNamespaceName,
			Name:      vmgRootName,
		}
		for _, m := range vmgMemberNames {
			vmgParameters.Members = append(vmgParameters.Members, vmopv1.GroupMember{Kind: vmKind, Name: m})
		}
		vmgRootYaml = manifestbuilders.GetVirtualMachineGroupYaml(vmgParameters)

		if len(vmgMemberNames) == 1 {
			Expect(clusterProxy.CreateWithArgs(ctx, vmgRootYaml)).To(Succeed(), "failed to create VirtualMachineGroup %q", vmgRootName)
		} else {
			Expect(clusterProxy.ApplyWithArgs(ctx, vmgRootYaml)).To(Succeed(), "failed to update VirtualMachineGroup %q", vmgRootName)
		}
	}

	// createVM creates a powered-on VM in the target namespace carrying
	// labels and, when affinity is non-nil, referencing it via
	// spec.affinity. Tag application happens during config reconciliation
	// regardless of power state, but every VM in this suite is powered on
	// to exercise tagging against a running VM rather than only a
	// powered-off one.
	createVM := func(vmName string, labels map[string]string, affinity *vmopv1.AffinitySpec) {
		GinkgoHelper()

		ensureVMInGroup(vmName)

		clusterResources := config.InfraConfig.ManagementClusterConfig.Resources
		vmParameters := manifestbuilders.VirtualMachineYaml{
			Namespace:        input.WCPNamespaceName,
			Name:             vmName,
			GroupName:        vmgRootName,
			Labels:           labels,
			Affinity:         affinity,
			ImageName:        linuxVMIName,
			VMClassName:      clusterResources.VMClassName,
			StorageClassName: clusterResources.StorageClassName,
			PowerState:       "PoweredOn",
		}
		vmYAML := manifestbuilders.GetVirtualMachineYamlA5(vmParameters)
		e2eframework.Logf("VM YAML:\n%s", string(vmYAML))
		Expect(clusterProxy.CreateWithArgs(ctx, vmYAML)).To(Succeed(), "failed to create VM %q:\n %s", vmName, string(vmYAML))

		vmNames = append(vmNames, vmName)

		vmoperator.WaitForVirtualMachineToExist(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)
		vmoperator.WaitForVirtualMachineConditionCreated(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)
		vmoperator.WaitForVirtualMachinePowerState(ctx, config, svClusterClient, input.WCPNamespaceName, vmName, "PoweredOn")
	}

	// deleteVM deletes a VM created by createVM and waits for it to be
	// gone, removing it from vmNames so AfterEach does not try again.
	deleteVM := func(vmName string) {
		GinkgoHelper()

		vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
		Expect(err).ToNot(HaveOccurred(), "failed to get VM %q before deleting it", vmName)
		Expect(svClusterClient.Delete(ctx, vm)).To(Succeed(), "failed to delete VM %q", vmName)

		vmoperator.WaitForVirtualMachineToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)

		for i, n := range vmNames {
			if n == vmName {
				vmNames = append(vmNames[:i], vmNames[i+1:]...)
				break
			}
		}
	}

	// getTag returns the Tag resource with the given derived name, or nil
	// if it does not exist (or is not yet observable).
	getTag := func(name string) *vspherepolv1alpha1.Tag {
		tag := &vspherepolv1alpha1.Tag{}
		if err := svClusterClient.Get(
			ctx,
			ctrlclient.ObjectKey{Namespace: input.WCPNamespaceName, Name: name},
			tag); err != nil {

			return nil
		}
		return tag
	}

	// waitForNoTag asserts that the Tag CR derived from key/value never
	// appears within the wait interval, so a regression that creates one
	// anyway is caught rather than racing to a false pass.
	waitForNoTag := func(key, value string) {
		GinkgoHelper()

		name := tagResourceName(key, value)
		Consistently(func() bool {
			return getTag(name) == nil
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			BeTrue(), "expected no Tag %q to be created in namespace %q", name, input.WCPNamespaceName)
	}

	// waitForTagOwnerCount waits for the Tag CR derived from key/value to
	// have exactly count owner references.
	waitForTagOwnerCount := func(key, value string, count int) *vspherepolv1alpha1.Tag {
		GinkgoHelper()

		name := tagResourceName(key, value)
		var tag *vspherepolv1alpha1.Tag

		Eventually(func(g Gomega) {
			tag = getTag(name)
			g.Expect(tag).ToNot(BeNil(), "Tag %q should exist", name)
			g.Expect(tag.OwnerReferences).To(HaveLen(count))
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			Succeed(), "timed out waiting for Tag %q to have %d owner(s)", name, count)

		return tag
	}

	// waitForTagDeleted waits for the Tag CR derived from key/value to be
	// removed from the namespace.
	waitForTagDeleted := func(key, value string) {
		GinkgoHelper()

		name := tagResourceName(key, value)
		Eventually(func() bool {
			return getTag(name) == nil
		}, config.GetIntervals("default", "wait-tag-cr-deleted")...).Should(
			BeTrue(), "timed out waiting for Tag %q to be deleted from namespace %q", name, input.WCPNamespaceName)
	}

	// hasAttachedVCenterTag reports whether the vCenter VM with the given
	// MoID has a tag named tagName in category categoryName attached,
	// resolving each attached tag's category by ID since GetAttachedTags
	// only returns the category ID.
	hasAttachedVCenterTag := func(vmMoID, tagName, categoryName string) (bool, error) {
		vmMoRef := types.ManagedObjectReference{Type: "VirtualMachine", Value: vmMoID}

		attached, err := tagMgr.GetAttachedTags(ctx, vmMoRef)
		if err != nil {
			return false, fmt.Errorf("failed to get attached tags for VM %q: %w", vmMoID, err)
		}

		for _, t := range attached {
			if t.Name != tagName {
				continue
			}

			category, err := tagMgr.GetCategory(ctx, t.CategoryID)
			if err != nil {
				return false, fmt.Errorf("failed to get category %q: %w", t.CategoryID, err)
			}

			if category.Name == categoryName {
				return true, nil
			}
		}

		return false, nil
	}

	// waitForVCenterTag waits for the vCenter tag "key:value" in category
	// input.WCPNamespaceName to be attached to the given VM.
	waitForVCenterTag := func(vmName, key, value string) {
		GinkgoHelper()

		vmMoID := vmoperator.GetVirtualMachineMOID(ctx, svClusterClient, input.WCPNamespaceName, vmName)
		tagName := vCenterTagName(key, value)

		Eventually(func(g Gomega) {
			has, err := hasAttachedVCenterTag(vmMoID, tagName, input.WCPNamespaceName)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(has).To(BeTrue())
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			Succeed(), "timed out waiting for vCenter tag %q to be attached to VM %q", tagName, vmName)
	}

	// consistentlyNoVCenterTag asserts the vCenter tag "key:value" in
	// category input.WCPNamespaceName is never attached to the given VM
	// for the duration of the wait interval.
	consistentlyNoVCenterTag := func(vmName, key, value string) {
		GinkgoHelper()

		vmMoID := vmoperator.GetVirtualMachineMOID(ctx, svClusterClient, input.WCPNamespaceName, vmName)
		tagName := vCenterTagName(key, value)

		Consistently(func(g Gomega) {
			has, err := hasAttachedVCenterTag(vmMoID, tagName, input.WCPNamespaceName)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(has).To(BeFalse())
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			Succeed(), "expected vCenter tag %q to never be attached to VM %q", tagName, vmName)
	}

	// waitForVCenterTagRemoved waits for the vCenter tag "key:value" in
	// category input.WCPNamespaceName to be detached from the given VM —
	// the converse of waitForVCenterTag, used when a fan-out reconcile is
	// expected to remove a tag that was previously attached.
	waitForVCenterTagRemoved := func(vmName, key, value string) {
		GinkgoHelper()

		vmMoID := vmoperator.GetVirtualMachineMOID(ctx, svClusterClient, input.WCPNamespaceName, vmName)
		tagName := vCenterTagName(key, value)

		Eventually(func(g Gomega) {
			has, err := hasAttachedVCenterTag(vmMoID, tagName, input.WCPNamespaceName)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(has).To(BeFalse())
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			Succeed(), "timed out waiting for vCenter tag %q to be removed from VM %q", tagName, vmName)
	}

	// consistentlyHasVCenterTag asserts the vCenter tag "key:value" in
	// category input.WCPNamespaceName stays attached to the given VM for the
	// duration of the wait interval — the converse of
	// consistentlyNoVCenterTag, used to prove a regression does not untag a
	// VM that should keep carrying the tag.
	consistentlyHasVCenterTag := func(vmName, key, value string) {
		GinkgoHelper()

		vmMoID := vmoperator.GetVirtualMachineMOID(ctx, svClusterClient, input.WCPNamespaceName, vmName)
		tagName := vCenterTagName(key, value)

		Consistently(func(g Gomega) {
			has, err := hasAttachedVCenterTag(vmMoID, tagName, input.WCPNamespaceName)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(has).To(BeTrue())
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			Succeed(), "expected vCenter tag %q to remain attached to VM %q", tagName, vmName)
	}

	// patchVM mutates an existing VM's spec via Get/mutate/Update, retrying
	// on conflict — the same pattern vm_networking.go uses for spec.network.
	patchVM := func(vmName string, mutate func(vm *vmopv1.VirtualMachine)) {
		GinkgoHelper()

		key := ctrlclient.ObjectKey{Namespace: input.WCPNamespaceName, Name: vmName}
		Eventually(func() error {
			vm := &vmopv1.VirtualMachine{}
			if err := svClusterClient.Get(ctx, key, vm); err != nil {
				return err
			}

			mutate(vm)

			return svClusterClient.Update(ctx, vm)
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			Succeed(), "failed to patch VM %q", vmName)
	}

	BeforeEach(func() {
		input = inputGetter()
		Expect(input.Config).ToNot(BeNil(), "Invalid argument. input.Config can't be nil when calling %s spec", specName)
		Expect(input.Config.InfraConfig).ToNot(BeNil(), "Invalid argument. input.Config.InfraConfig can't be nil when calling %s spec", specName)
		skipper.SkipUnlessInfraIs(input.Config.InfraConfig.InfraName, consts.WCP)

		Expect(input.ClusterProxy).ToNot(BeNil(), "Invalid argument. input.ClusterProxy can't be nil when calling %s spec", specName)
		Expect(input.WCPNamespaceName).ToNot(BeEmpty(), "Invalid argument. input.WCPNamespaceName can't be empty when calling %s spec", specName)
		Expect(os.MkdirAll(input.ArtifactFolder, 0755)).To(Succeed(), "Invalid argument. input.ArtifactFolder can't be created for %s spec", specName)

		config = input.Config
		clusterProxy = input.ClusterProxy.(*common.VMServiceClusterProxy)
		wcpClient = input.WCPClient
		cancelPodWatches := framework.WatchPodLogsAndEventsInNamespaces(ctx, []string{config.GetVariable("VMOPNamespace")}, clusterProxy.GetClientSet(), filepath.Join(input.ArtifactFolder, specName))
		DeferCleanup(cancelPodWatches)

		svClusterClient = clusterProxy.GetClient()

		// The feature has no capability of its own. It is gated here
		// because affinityFor and affinityForValues reference labels via a
		// required term whose topologyKey is kubernetes.io/hostname, which
		// the admission webhook only accepts once this capability is
		// enabled (without it, only topology.kubernetes.io/zone is
		// accepted). See VMAffinityTagSpec's doc comment.
		skipper.SkipUnlessSupervisorCapabilityEnabled(ctx, clusterProxy, consts.VMAffinityDuringExecutionCapabilityName)

		// spec.affinity requires a non-empty spec.groupName (see
		// ensureVMInGroup's doc comment), and spec.groupName itself is
		// rejected unless VM Groups is enabled — so this suite also needs
		// that capability, exactly as vm_group.go does.
		skipper.SkipUnlessSupervisorCapabilityEnabled(ctx, clusterProxy, consts.VMGroupsCapabilityName)

		// The whole suite depends on the Tag-driven path, which is gated on
		// Features.TaggingAPI.
		skipper.SkipUnlessTaggingAPIFSSEnabled(ctx, svClusterClient, config)

		// Skip entirely when the Tag CRD is not registered on this
		// Supervisor at all. This is a distinct condition from the FSS being
		// off: the flag defaults to on, so a Supervisor that never installed
		// the CRD reports the FSS as enabled while the API is absent.
		if err := svClusterClient.List(ctx, &vspherepolv1alpha1.TagList{}, ctrlclient.InNamespace(input.WCPNamespaceName)); err != nil {
			if apimeta.IsNoMatchError(err) {
				framework.SkipInternalf(1, "skip the test as the Tag CRD is not registered on this Supervisor")
			}
			Expect(err).ToNot(HaveOccurred(), "failed to list Tags")
		}

		vCenterClient = vcenter.NewVimClientFromKubeconfig(ctx, clusterProxy.GetKubeconfigPath())
		restClient, err := vcenter.NewRestClient(ctx, vCenterClient, testbed.AdminUsername, testbed.AdminPassword)
		Expect(err).ToNot(HaveOccurred(), "failed to create REST client")
		tagMgr = tags.NewManager(restClient)

		clusterResources := config.InfraConfig.ManagementClusterConfig.Resources
		linuxImageDisplayName = vmservice.GetDefaultImageDisplayName(clusterResources)
		linuxVMIName = vmoperator.WaitForVirtualMachineImageName(ctx, &config.Config, svClusterClient, input.WCPNamespaceName, linuxImageDisplayName)

		vmNames = nil

		vmgRootName = fmt.Sprintf("%s-%s-root", specName, capiutil.RandomString(4))
		vmgRootYaml = nil
		vmgMemberNames = nil
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			for _, vmName := range vmNames {
				vmoperator.DescribeResourceIfExists(ctx, svClusterClient, clusterProxy.GetKubeconfigPath(), input.WCPNamespaceName, vmName, vmKind)
			}
			if len(vmgRootYaml) > 0 {
				vmoperator.DescribeResourceIfExists(ctx, svClusterClient, clusterProxy.GetKubeconfigPath(), input.WCPNamespaceName, vmgRootName, vmgKind)
			}
		}

		for _, vmName := range vmNames {
			By(fmt.Sprintf("Deleting VM %s", vmName))
			vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
			if err == nil {
				Expect(svClusterClient.Delete(ctx, vm)).To(Succeed(), "failed to delete VM %q", vmName)
				vmoperator.WaitForVirtualMachineToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)
			}
		}
		vmNames = nil

		if len(vmgRootYaml) > 0 {
			By(fmt.Sprintf("Deleting the root VirtualMachineGroup %s", vmgRootName))
			Expect(clusterProxy.DeleteWithArgs(ctx, vmgRootYaml)).To(Succeed(), "failed to delete VirtualMachineGroup %q", vmgRootName)
			vmoperator.WaitForVirtualMachineGroupToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, vmgRootName)
		}

		if vCenterClient != nil {
			vcenter.LogoutVimClient(vCenterClient)
		}
	})

	Context("Label referenced by a new VM's affinity becomes a vCenter tag", func() {
		It("creates a Tag CR and applies the vCenter tag for a VM whose affinity references its own label",
			Label("core-functional", "extended-functional", "experimental"), func() {
				vmName := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s with label %s=blue and affinity referencing %s=blue", vmName, affinityKey, affinityKey))
				createVM(vmName, map[string]string{affinityKey: "blue"}, affinityFor(affinityKey, "blue"))

				By("Waiting for the Tag CR to be created with the derived name, label mirror, and this VM as sole owner")
				vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, "blue", 1)
				Expect(tag.Spec.Key).To(Equal(affinityKey))
				Expect(tag.Spec.Value).To(Equal("blue"))
				Expect(tag.Labels).To(HaveKeyWithValue(affinityKey, "blue"))
				Expect(tag.OwnerReferences[0].UID).To(Equal(vm.UID))
				Expect(tag.OwnerReferences[0].Name).To(Equal(vmName))

				By("Verifying the vCenter tag is attached to the VM")
				waitForVCenterTag(vmName, affinityKey, "blue")
			})

		It("appends a second VM as an owner of the same Tag CR rather than replacing the first",
			Label("core-functional", "extended-functional", "experimental"), func() {
				vm1Name := fmt.Sprintf("%s-%s-1", specName, capiutil.RandomString(6))
				vm2Name := fmt.Sprintf("%s-%s-2", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM1 (%s) with label %s=blue and affinity referencing %s=blue", vm1Name, affinityKey, affinityKey))
				createVM(vm1Name, map[string]string{affinityKey: "blue"}, affinityFor(affinityKey, "blue"))
				waitForTagOwnerCount(affinityKey, "blue", 1)
				waitForVCenterTag(vm1Name, affinityKey, "blue")

				By(fmt.Sprintf("Creating VM2 (%s) with the same label and affinity", vm2Name))
				createVM(vm2Name, map[string]string{affinityKey: "blue"}, affinityFor(affinityKey, "blue"))

				By("Verifying exactly one Tag CR exists with both VMs recorded as owners")
				vm1, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm1Name)
				Expect(err).ToNot(HaveOccurred())
				vm2, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm2Name)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, "blue", 2)
				Expect([]string{
					string(tag.OwnerReferences[0].UID),
					string(tag.OwnerReferences[1].UID),
				}).To(ConsistOf(string(vm1.UID), string(vm2.UID)))

				By("Verifying both VMs carry the vCenter tag")
				waitForVCenterTag(vm1Name, affinityKey, "blue")
				waitForVCenterTag(vm2Name, affinityKey, "blue")
			})

		It("creates a Tag CR only for the label pair referenced by affinity, not the unreferenced one",
			Label("core-functional", "extended-functional", "experimental"), func() {
				vmName := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s with labels %s=blue and env=prod, affinity referencing only %s=blue", vmName, affinityKey, affinityKey))
				createVM(vmName,
					map[string]string{affinityKey: "blue", "env": "prod"},
					affinityFor(affinityKey, "blue"))

				By("Verifying a Tag CR exists for the referenced pair")
				waitForTagOwnerCount(affinityKey, "blue", 1)
				waitForVCenterTag(vmName, affinityKey, "blue")

				By("Verifying no Tag CR and no vCenter tag exist for the unreferenced pair")
				waitForNoTag("env", "prod")
				consistentlyNoVCenterTag(vmName, "env", "prod")
			})

		It("keeps the Tag CR and the surviving VM's vCenter tag when one of two owning VMs is deleted",
			Label("core-functional", "extended-functional", "experimental"), func() {
				vm1Name := fmt.Sprintf("%s-%s-1", specName, capiutil.RandomString(6))
				vm2Name := fmt.Sprintf("%s-%s-2", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM1 (%s) and VM2 (%s), both with label %s=blue and affinity referencing it", vm1Name, vm2Name, affinityKey))
				createVM(vm1Name, map[string]string{affinityKey: "blue"}, affinityFor(affinityKey, "blue"))
				createVM(vm2Name, map[string]string{affinityKey: "blue"}, affinityFor(affinityKey, "blue"))
				waitForTagOwnerCount(affinityKey, "blue", 2)

				By(fmt.Sprintf("Deleting VM1 (%s)", vm1Name))
				deleteVM(vm1Name)

				By("Verifying the Tag CR survives with exactly one owner, VM2")
				vm2, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm2Name)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, "blue", 1)
				Expect(tag.OwnerReferences[0].UID).To(Equal(vm2.UID))

				By("Verifying VM2 keeps the vCenter tag")
				waitForVCenterTag(vm2Name, affinityKey, "blue")
			})

		It("deletes the Tag CR once its only owning VM is deleted",
			Label("core-functional", "extended-functional", "experimental"), func() {
				vmName := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s with label %s=blue and affinity referencing it", vmName, affinityKey))
				createVM(vmName, map[string]string{affinityKey: "blue"}, affinityFor(affinityKey, "blue"))
				waitForTagOwnerCount(affinityKey, "blue", 1)
				waitForVCenterTag(vmName, affinityKey, "blue")

				By(fmt.Sprintf("Deleting the sole owning VM %s", vmName))
				deleteVM(vmName)

				By("Verifying the Tag CR is deleted from the namespace")
				waitForTagDeleted(affinityKey, "blue")
			})

		// Pending: auto tag creation requires the tag being specified in
		// the affinity policy. In this scenario, zonal policies are not
		// sent during create, so the tag is not created and the second
		// VM's creation fails. Remove pending once we start persisting
		// zonal policies during VM create.
		PIt("creates a Tag CR with a VM as sole owner when its affinity references a label it does not itself carry",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const (
					ownValue    = "us1-4-own"
					targetValue = "us1-4-target"
				)

				vmName := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s with label %s=%s and an anti-affinity term referencing %s=%s, which it does not carry", vmName, affinityKey, ownValue, affinityKey, targetValue))
				createVM(vmName, map[string]string{affinityKey: ownValue}, antiAffinityFor(affinityKey, targetValue))

				By("Verifying a Tag CR for the referenced pair is created with this VM as sole owner")
				vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, targetValue, 1)
				Expect(tag.OwnerReferences[0].UID).To(Equal(vm.UID))

				By("Verifying the referencing VM is not itself given the vCenter tag, since it does not carry that label")
				consistentlyNoVCenterTag(vmName, affinityKey, targetValue)

				By("Verifying a VM that does carry the referenced label is tagged once the Tag CR exists")
				carryingVMName := fmt.Sprintf("%s-%s-carrying", specName, capiutil.RandomString(6))
				createVM(carryingVMName, map[string]string{affinityKey: targetValue}, nil)
				waitForVCenterTag(carryingVMName, affinityKey, targetValue)

				By("Verifying the carrying VM was not added as an owner of the Tag CR")
				tagAfter := waitForTagOwnerCount(affinityKey, targetValue, 1)
				Expect(tagAfter.OwnerReferences[0].UID).To(Equal(vm.UID))
			})
	})

	Context("Pre-existing labeled VM is tagged when a later VM references the label", func() {
		// Each It below uses a label value unique to that scenario rather
		// than reusing a value shared across another Context's Its.
		// tagResourceName derives one fixed Tag name per key/value pair, so
		// two Its racing on the same pair can have a new VM's
		// ensureOwnedTag Get-and-adopt read a cached copy of the *previous*
		// It's Tag after that Tag was already deleted on the server
		// (AfterEach waits only for the VMs it created to be deleted, not
		// for the Tag their deletion frees up). The reconcile recovers on
		// its own — the ownership patch fails under its optimistic lock and
		// is retried — but the retries make this Context's own waits flaky
		// for reasons that have nothing to do with what it asserts. A
		// unique value per It makes every Tag name here unique for the life
		// of the suite, independent of spec ordering or AfterEach timing.
		It("tags a pre-existing label-only VM once a later VM references the label",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "us2-preexisting"

				labelOnlyVMName := fmt.Sprintf("%s-%s-label-only", specName, capiutil.RandomString(6))
				referencingVMName := fmt.Sprintf("%s-%s-ref", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating label-only VM %s with %s=%s and no affinity", labelOnlyVMName, affinityKey, value))
				createVM(labelOnlyVMName, map[string]string{affinityKey: value}, nil)

				By("Verifying the label-only VM is not tagged before anything references its label")
				waitForNoTag(affinityKey, value)
				consistentlyNoVCenterTag(labelOnlyVMName, affinityKey, value)

				By(fmt.Sprintf("Creating VM %s whose affinity references %s=%s", referencingVMName, affinityKey, value))
				createVM(referencingVMName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))

				By("Verifying the Tag CR records only the referencing VM as owner")
				refVM, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, referencingVMName)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, value, 1)
				Expect(tag.OwnerReferences[0].UID).To(Equal(refVM.UID))
				Expect(tag.OwnerReferences[0].Name).To(Equal(referencingVMName))

				By("Verifying both the referencing VM and the label-only VM carry the vCenter tag")
				waitForVCenterTag(referencingVMName, affinityKey, value)
				waitForVCenterTag(labelOnlyVMName, affinityKey, value)
			})

		It("untags the label-only VM when the referencing VM is deleted",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "us2-untag-on-delete"

				labelOnlyVMName := fmt.Sprintf("%s-%s-label-only", specName, capiutil.RandomString(6))
				referencingVMName := fmt.Sprintf("%s-%s-ref", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating label-only VM %s and referencing VM %s, both with %s=%s", labelOnlyVMName, referencingVMName, affinityKey, value))
				createVM(labelOnlyVMName, map[string]string{affinityKey: value}, nil)
				createVM(referencingVMName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))

				tag := waitForTagOwnerCount(affinityKey, value, 1)
				Expect(tag.OwnerReferences[0].Name).To(Equal(referencingVMName))
				waitForVCenterTag(referencingVMName, affinityKey, value)
				waitForVCenterTag(labelOnlyVMName, affinityKey, value)

				By(fmt.Sprintf("Deleting the referencing VM %s", referencingVMName))
				deleteVM(referencingVMName)

				By("Verifying the Tag CR is deleted, driven by the VM controller's Tag watch")
				waitForTagDeleted(affinityKey, value)

				By("Verifying the vCenter tag is removed from the label-only VM")
				waitForVCenterTagRemoved(labelOnlyVMName, affinityKey, value)
			})

		It("does not tag a same-labeled VM in a different namespace",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "us2-ns-isolation"

				clusterResources := config.InfraConfig.ManagementClusterConfig.Resources
				secondNamespaceName := fmt.Sprintf("%s-%s-ns2", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating a second namespace %s", secondNamespaceName))
				clID := vmservice.GetContentLibraryUUIDByName(consts.VMServiceCLName, wcpClient)
				vmsvcSpecs := wcp.NewVMServiceSpecDetails([]string{clusterResources.VMClassName}, []string{clID})

				secondNamespaceCtx, err := clusterProxy.CreateWCPNamespace(
					ctx, config, vmsvcSpecs,
					clusterResources.StorageClassName, clusterResources.WorkerStorageClassName,
					secondNamespaceName, input.ArtifactFolder)
				Expect(err).ToNot(HaveOccurred(), "failed to create a second test WCP namespace")
				wcp.WaitForNamespaceReady(wcpClient, secondNamespaceName)
				DeferCleanup(func() {
					clusterProxy.DeleteWCPNamespace(secondNamespaceCtx)
				})

				By("Waiting for the Linux VM Image to be available in the second namespace")
				linuxVMIName2 := vmoperator.WaitForVirtualMachineImageName(ctx, &config.Config, svClusterClient, secondNamespaceName, linuxImageDisplayName)

				otherNSVMName := fmt.Sprintf("%s-%s-other-ns", specName, capiutil.RandomString(6))
				By(fmt.Sprintf("Creating label-only VM %s with %s=%s in namespace %s", otherNSVMName, affinityKey, value, secondNamespaceName))
				vmYAML := manifestbuilders.GetVirtualMachineYamlA5(manifestbuilders.VirtualMachineYaml{
					Namespace:        secondNamespaceName,
					Name:             otherNSVMName,
					Labels:           map[string]string{affinityKey: value},
					ImageName:        linuxVMIName2,
					VMClassName:      clusterResources.VMClassName,
					StorageClassName: clusterResources.StorageClassName,
					PowerState:       "PoweredOn",
				})
				Expect(clusterProxy.CreateWithArgs(ctx, vmYAML)).To(Succeed(), "failed to create VM %q:\n %s", otherNSVMName, string(vmYAML))
				DeferCleanup(func() {
					vm, err := utils.GetVirtualMachine(ctx, svClusterClient, secondNamespaceName, otherNSVMName)
					if err == nil {
						Expect(svClusterClient.Delete(ctx, vm)).To(Succeed(), "failed to delete VM %q", otherNSVMName)
						vmoperator.WaitForVirtualMachineToBeDeleted(ctx, config, svClusterClient, secondNamespaceName, otherNSVMName)
					}
				})
				vmoperator.WaitForVirtualMachineToExist(ctx, config, svClusterClient, secondNamespaceName, otherNSVMName)
				vmoperator.WaitForVirtualMachineConditionCreated(ctx, config, svClusterClient, secondNamespaceName, otherNSVMName)
				vmoperator.WaitForVirtualMachinePowerState(ctx, config, svClusterClient, secondNamespaceName, otherNSVMName, "PoweredOn")

				referencingVMName := fmt.Sprintf("%s-%s-ref", specName, capiutil.RandomString(6))
				By(fmt.Sprintf("Creating VM %s in namespace %s whose affinity references %s=%s", referencingVMName, input.WCPNamespaceName, affinityKey, value))
				createVM(referencingVMName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))

				By("Verifying the Tag CR is created in the referencing VM's namespace")
				waitForTagOwnerCount(affinityKey, value, 1)

				By(fmt.Sprintf("Verifying no Tag CR exists in namespace %s", secondNamespaceName))
				var tagList vspherepolv1alpha1.TagList
				Expect(svClusterClient.List(ctx, &tagList, ctrlclient.InNamespace(secondNamespaceName))).To(Succeed())
				Expect(tagList.Items).To(BeEmpty())

				By("Verifying the other namespace's VM never gets the vCenter tag")
				otherVMMoID := vmoperator.GetVirtualMachineMOID(ctx, svClusterClient, secondNamespaceName, otherNSVMName)
				tagName := vCenterTagName(affinityKey, value)
				Consistently(func(g Gomega) {
					has, err := hasAttachedVCenterTag(otherVMMoID, tagName, secondNamespaceName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(has).To(BeFalse())
				}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
					Succeed(), "expected vCenter tag %q to never be attached to VM %q in namespace %q", tagName, otherNSVMName, secondNamespaceName)
			})

		It("tags a label-only VM created after the Tag CR already exists",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "us2-late-arrival"

				referencingVMName := fmt.Sprintf("%s-%s-ref", specName, capiutil.RandomString(6))
				labelOnlyVMName := fmt.Sprintf("%s-%s-label-only", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s whose affinity references %s=%s, creating the Tag CR", referencingVMName, affinityKey, value))
				createVM(referencingVMName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))
				tagBefore := waitForTagOwnerCount(affinityKey, value, 1)
				waitForVCenterTag(referencingVMName, affinityKey, value)

				By(fmt.Sprintf("Creating label-only VM %s after the Tag CR already exists", labelOnlyVMName))
				createVM(labelOnlyVMName, map[string]string{affinityKey: value}, nil)

				By("Verifying the new VM picks up the vCenter tag on its first reconcile")
				waitForVCenterTag(labelOnlyVMName, affinityKey, value)

				By("Verifying the new VM was not added as an owner of the Tag CR")
				tagAfter := waitForTagOwnerCount(affinityKey, value, 1)
				Expect(tagAfter.OwnerReferences[0].UID).To(Equal(tagBefore.OwnerReferences[0].UID))
			})
	})

	Context("Label participation changes and tagging converges", func() {
		// Each It below uses label values unique to that scenario, for the
		// same reason the label-only Context does (see its doc comment): a Tag
		// deleted by one It could still be terminating when another It's VM
		// tries to adopt the same derived name.
		//
		// spec.affinity is immutable on update (webhook rule
		// validateImmutableVMAffinity), so the only way a live VM's
		// participation in a label pair changes is by changing its labels.
		// Ownership follows the affinity reference alone, not carriage, so
		// dropping the label never drops ownership — the VM's own
		// spec.affinity still references the pair, and that can't change.
		// Only the VM's own vCenter tag carriage reacts to the label change.

		It("keeps the Tag CR — ownership survives a label drop — and only removes the vCenter tag from the VM that dropped the label",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "us3-sole-owner-cleanup"

				vmName := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s with label %s=%s and affinity referencing it", vmName, affinityKey, value))
				createVM(vmName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))
				tagBefore := waitForTagOwnerCount(affinityKey, value, 1)
				waitForVCenterTag(vmName, affinityKey, value)

				By(fmt.Sprintf("Patching VM %s to drop the label while keeping the (immutable) affinity reference", vmName))
				patchVM(vmName, func(vm *vmopv1.VirtualMachine) {
					delete(vm.Labels, affinityKey)
				})

				By("Verifying the Tag CR survives with the VM still recorded as owner, since its spec.affinity still references the pair")
				vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
				Expect(err).ToNot(HaveOccurred())

				tagAfter := waitForTagOwnerCount(affinityKey, value, 1)
				Expect(tagAfter.UID).To(Equal(tagBefore.UID))
				Expect(tagAfter.OwnerReferences[0].UID).To(Equal(vm.UID))

				By("Verifying the vCenter tag is removed from the VM, since it no longer carries the label")
				waitForVCenterTagRemoved(vmName, affinityKey, value)
			})

		It("keeps the Tag CR for both owners and the label-only VM when one owner drops the label",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "us3-two-owner-partial-cleanup"

				prefix := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))
				vm1Name := fmt.Sprintf("%s-1", prefix)
				vm2Name := fmt.Sprintf("%s-2", prefix)
				labelOnlyVMName := fmt.Sprintf("%s-label-only", prefix)

				By(fmt.Sprintf("Creating owner VMs %s and %s, plus label-only VM %s, all with %s=%s", vm1Name, vm2Name, labelOnlyVMName, affinityKey, value))
				createVM(vm1Name, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))
				createVM(vm2Name, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))
				createVM(labelOnlyVMName, map[string]string{affinityKey: value}, nil)

				waitForTagOwnerCount(affinityKey, value, 2)
				waitForVCenterTag(vm1Name, affinityKey, value)
				waitForVCenterTag(vm2Name, affinityKey, value)
				waitForVCenterTag(labelOnlyVMName, affinityKey, value)

				By(fmt.Sprintf("Patching VM1 (%s) to drop the label while keeping the (immutable) affinity reference", vm1Name))
				patchVM(vm1Name, func(vm *vmopv1.VirtualMachine) {
					delete(vm.Labels, affinityKey)
				})

				By("Verifying the Tag CR still records both VM1 and VM2 as owners, since VM1's ownership is unaffected by dropping the label")
				vm1, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm1Name)
				Expect(err).ToNot(HaveOccurred())
				vm2, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm2Name)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, value, 2)
				ownerUIDs := make([]string, len(tag.OwnerReferences))
				for i, ref := range tag.OwnerReferences {
					ownerUIDs[i] = string(ref.UID)
				}
				Expect(ownerUIDs).To(ConsistOf(string(vm1.UID), string(vm2.UID)))

				By("Verifying VM1 loses the vCenter tag because it no longer carries the label")
				waitForVCenterTagRemoved(vm1Name, affinityKey, value)

				By("Verifying VM2 keeps the vCenter tag, since it still carries the label")
				consistentlyHasVCenterTag(vm2Name, affinityKey, value)

				By("Verifying the label-only VM keeps the vCenter tag while the Tag CR survives")
				consistentlyHasVCenterTag(labelOnlyVMName, affinityKey, value)
			})
	})

	Context("Edge cases", func() {
		It("leaves a VM with the same label key but a different value untagged",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const (
					referencedValue = "edge-samekey-referenced"
					otherValue      = "edge-samekey-other"
				)

				referencingVMName := fmt.Sprintf("%s-%s-ref", specName, capiutil.RandomString(6))
				otherValueVMName := fmt.Sprintf("%s-%s-other-value", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s carrying %s=%s with no affinity", otherValueVMName, affinityKey, otherValue))
				createVM(otherValueVMName, map[string]string{affinityKey: otherValue}, nil)

				By(fmt.Sprintf("Creating VM %s whose affinity references %s=%s", referencingVMName, affinityKey, referencedValue))
				createVM(referencingVMName, map[string]string{affinityKey: referencedValue}, affinityFor(affinityKey, referencedValue))

				By("Verifying the Tag CR and vCenter tag exist only for the referenced value")
				waitForTagOwnerCount(affinityKey, referencedValue, 1)
				waitForVCenterTag(referencingVMName, affinityKey, referencedValue)

				By("Verifying no Tag CR is created for the other value, and the other VM never carries the referenced tag")
				waitForNoTag(affinityKey, otherValue)
				consistentlyNoVCenterTag(otherValueVMName, affinityKey, referencedValue)
			})

		It("tags VMs identically regardless of VirtualMachineGroup membership",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "edge-sc004-cross-group"

				clusterResources := config.InfraConfig.ManagementClusterConfig.Resources

				vm1Name := fmt.Sprintf("%s-%s-group1", specName, capiutil.RandomString(6))
				vm2Name := fmt.Sprintf("%s-%s-group2", specName, capiutil.RandomString(6))
				secondGroupName := fmt.Sprintf("%s-%s-group2", specName, capiutil.RandomString(4))

				By(fmt.Sprintf("Creating VM1 (%s) with label %s=%s and affinity referencing it, as a member of the suite's shared VirtualMachineGroup", vm1Name, affinityKey, value))
				createVM(vm1Name, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))

				By(fmt.Sprintf("Creating a second, unrelated VirtualMachineGroup and VM2 (%s) with the same label and affinity, as its sole member", vm2Name))
				secondGroupYaml := manifestbuilders.GetVirtualMachineGroupYaml(manifestbuilders.VirtualMachineGroupYaml{
					Namespace: input.WCPNamespaceName,
					Name:      secondGroupName,
					Members:   []vmopv1.GroupMember{{Kind: vmKind, Name: vm2Name}},
				})
				Expect(clusterProxy.CreateWithArgs(ctx, secondGroupYaml)).To(Succeed(), "failed to create VirtualMachineGroup %q", secondGroupName)
				DeferCleanup(func() {
					Expect(clusterProxy.DeleteWithArgs(ctx, secondGroupYaml)).To(Succeed(), "failed to delete VirtualMachineGroup %q", secondGroupName)
					vmoperator.WaitForVirtualMachineGroupToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, secondGroupName)
				})

				vm2YAML := manifestbuilders.GetVirtualMachineYamlA5(manifestbuilders.VirtualMachineYaml{
					Namespace:        input.WCPNamespaceName,
					Name:             vm2Name,
					GroupName:        secondGroupName,
					Labels:           map[string]string{affinityKey: value},
					Affinity:         affinityFor(affinityKey, value),
					ImageName:        linuxVMIName,
					VMClassName:      clusterResources.VMClassName,
					StorageClassName: clusterResources.StorageClassName,
					PowerState:       "PoweredOn",
				})
				Expect(clusterProxy.CreateWithArgs(ctx, vm2YAML)).To(Succeed(), "failed to create VM %q:\n %s", vm2Name, string(vm2YAML))
				DeferCleanup(func() {
					vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm2Name)
					if err == nil {
						Expect(svClusterClient.Delete(ctx, vm)).To(Succeed(), "failed to delete VM %q", vm2Name)
						vmoperator.WaitForVirtualMachineToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, vm2Name)
					}
				})
				vmoperator.WaitForVirtualMachineToExist(ctx, config, svClusterClient, input.WCPNamespaceName, vm2Name)
				vmoperator.WaitForVirtualMachineConditionCreated(ctx, config, svClusterClient, input.WCPNamespaceName, vm2Name)
				vmoperator.WaitForVirtualMachinePowerState(ctx, config, svClusterClient, input.WCPNamespaceName, vm2Name, "PoweredOn")

				By("Verifying one Tag CR with both VMs as owners, despite belonging to different VirtualMachineGroups")
				vm1, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm1Name)
				Expect(err).ToNot(HaveOccurred())
				vm2, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm2Name)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, value, 2)
				ownerUIDs := make([]string, len(tag.OwnerReferences))
				for i, ref := range tag.OwnerReferences {
					ownerUIDs[i] = string(ref.UID)
				}
				Expect(ownerUIDs).To(ConsistOf(string(vm1.UID), string(vm2.UID)))

				By("Verifying both VMs carry the same vCenter tag")
				waitForVCenterTag(vm1Name, affinityKey, value)
				waitForVCenterTag(vm2Name, affinityKey, value)
			})

		It("converges a re-created VM back to the same Tag CR and the same vCenter tag",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "edge-recreate-cycle"

				clusterResources := config.InfraConfig.ManagementClusterConfig.Resources

				recreatedVMName := fmt.Sprintf("%s-%s-recreate", specName, capiutil.RandomString(6))
				survivorVMName := fmt.Sprintf("%s-%s-survivor", specName, capiutil.RandomString(6))
				groupName := fmt.Sprintf("%s-%s-recreate-group", specName, capiutil.RandomString(4))

				// buildVMYaml and waitForVMReady are local rather than the
				// suite's createVM, because createVM always adds its VM to
				// the suite's single shared VirtualMachineGroup — appending
				// the same name to that group's member list a second time
				// on the re-create below would either duplicate the entry
				// or race the group patch against other running Its. This
				// It uses its own dedicated group instead, whose member
				// names never change across the delete/re-create cycle.
				buildVMYaml := func(vmName string) []byte {
					return manifestbuilders.GetVirtualMachineYamlA5(manifestbuilders.VirtualMachineYaml{
						Namespace:        input.WCPNamespaceName,
						Name:             vmName,
						GroupName:        groupName,
						Labels:           map[string]string{affinityKey: value},
						Affinity:         affinityFor(affinityKey, value),
						ImageName:        linuxVMIName,
						VMClassName:      clusterResources.VMClassName,
						StorageClassName: clusterResources.StorageClassName,
						PowerState:       "PoweredOn",
					})
				}
				waitForVMReady := func(vmName string) {
					GinkgoHelper()
					vmoperator.WaitForVirtualMachineToExist(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)
					vmoperator.WaitForVirtualMachineConditionCreated(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)
					vmoperator.WaitForVirtualMachinePowerState(ctx, config, svClusterClient, input.WCPNamespaceName, vmName, "PoweredOn")
				}

				By(fmt.Sprintf("Creating a dedicated VirtualMachineGroup naming %s and %s as members", recreatedVMName, survivorVMName))
				groupYaml := manifestbuilders.GetVirtualMachineGroupYaml(manifestbuilders.VirtualMachineGroupYaml{
					Namespace: input.WCPNamespaceName,
					Name:      groupName,
					Members: []vmopv1.GroupMember{
						{Kind: vmKind, Name: recreatedVMName},
						{Kind: vmKind, Name: survivorVMName},
					},
				})
				Expect(clusterProxy.CreateWithArgs(ctx, groupYaml)).To(Succeed(), "failed to create VirtualMachineGroup %q", groupName)
				DeferCleanup(func() {
					Expect(clusterProxy.DeleteWithArgs(ctx, groupYaml)).To(Succeed(), "failed to delete VirtualMachineGroup %q", groupName)
					vmoperator.WaitForVirtualMachineGroupToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, groupName)
				})

				// The group above names both VMs as members while neither
				// exists yet, so both Creates must land before waiting on
				// either's readiness: a VM's own reconcile can't get a
				// group placement result until every declared member
				// exists, so waiting on one before creating the other
				// would deadlock.
				By(fmt.Sprintf("Creating VM %s and VM %s, both with label %s=%s and affinity referencing it", recreatedVMName, survivorVMName, affinityKey, value))
				Expect(clusterProxy.CreateWithArgs(ctx, buildVMYaml(recreatedVMName))).To(Succeed(), "failed to create VM %q", recreatedVMName)
				DeferCleanup(func() {
					vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, recreatedVMName)
					if err == nil {
						Expect(svClusterClient.Delete(ctx, vm)).To(Succeed(), "failed to delete VM %q", recreatedVMName)
						vmoperator.WaitForVirtualMachineToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, recreatedVMName)
					}
				})

				Expect(clusterProxy.CreateWithArgs(ctx, buildVMYaml(survivorVMName))).To(Succeed(), "failed to create VM %q", survivorVMName)
				DeferCleanup(func() {
					vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, survivorVMName)
					if err == nil {
						Expect(svClusterClient.Delete(ctx, vm)).To(Succeed(), "failed to delete VM %q", survivorVMName)
						vmoperator.WaitForVirtualMachineToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, survivorVMName)
					}
				})

				waitForVMReady(recreatedVMName)
				waitForVMReady(survivorVMName)

				tagBefore := waitForTagOwnerCount(affinityKey, value, 2)
				waitForVCenterTag(recreatedVMName, affinityKey, value)
				waitForVCenterTag(survivorVMName, affinityKey, value)

				By(fmt.Sprintf("Deleting VM %s while the survivor VM %s keeps the Tag CR alive", recreatedVMName, survivorVMName))
				deleteVM(recreatedVMName)

				tagAfterDelete := waitForTagOwnerCount(affinityKey, value, 1)
				Expect(tagAfterDelete.UID).To(Equal(tagBefore.UID), "expected the Tag CR to survive the delete since the survivor VM still owns it")

				By(fmt.Sprintf("Re-creating a new VM named %s with the same label and affinity, under the same VirtualMachineGroup member entry", recreatedVMName))
				Expect(clusterProxy.CreateWithArgs(ctx, buildVMYaml(recreatedVMName))).To(Succeed(), "failed to re-create VM %q", recreatedVMName)
				waitForVMReady(recreatedVMName)

				By("Verifying the re-created VM converges back onto the same Tag CR as a second owner")
				recreatedVM, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, recreatedVMName)
				Expect(err).ToNot(HaveOccurred())
				survivorVM, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, survivorVMName)
				Expect(err).ToNot(HaveOccurred())

				tagAfterRecreate := waitForTagOwnerCount(affinityKey, value, 2)
				Expect(tagAfterRecreate.UID).To(Equal(tagBefore.UID), "expected the same Tag CR object to persist through the delete/re-create cycle")
				ownerUIDs := make([]string, len(tagAfterRecreate.OwnerReferences))
				for i, ref := range tagAfterRecreate.OwnerReferences {
					ownerUIDs[i] = string(ref.UID)
				}
				Expect(ownerUIDs).To(ConsistOf(string(survivorVM.UID), string(recreatedVM.UID)))

				By("Verifying the re-created VM carries the same vCenter tag again")
				waitForVCenterTag(recreatedVMName, affinityKey, value)
			})

		It("converges two VMs created at once on a single Tag CR",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "edge-concurrent-create"

				clusterResources := config.InfraConfig.ManagementClusterConfig.Resources

				vm1Name := fmt.Sprintf("%s-%s-concurrent1", specName, capiutil.RandomString(6))
				vm2Name := fmt.Sprintf("%s-%s-concurrent2", specName, capiutil.RandomString(6))
				groupName := fmt.Sprintf("%s-%s-concurrent-group", specName, capiutil.RandomString(4))

				// The suite's createVM waits for each VM to become ready
				// before returning, which would serialize the two creates
				// and leave the second VM adopting a Tag the first had long
				// since committed. Creating both from raw YAML back to back
				// instead lets their first reconciles overlap, so both race
				// to create the Tag for the same pair and the loser has to
				// adopt the winner's resource. A dedicated group also keeps
				// the suite's shared group's member list untouched.
				buildVMYaml := func(vmName string) []byte {
					return manifestbuilders.GetVirtualMachineYamlA5(manifestbuilders.VirtualMachineYaml{
						Namespace:        input.WCPNamespaceName,
						Name:             vmName,
						GroupName:        groupName,
						Labels:           map[string]string{affinityKey: value},
						Affinity:         affinityFor(affinityKey, value),
						ImageName:        linuxVMIName,
						VMClassName:      clusterResources.VMClassName,
						StorageClassName: clusterResources.StorageClassName,
						PowerState:       "PoweredOn",
					})
				}

				By(fmt.Sprintf("Creating a dedicated VirtualMachineGroup naming %s and %s as members", vm1Name, vm2Name))
				groupYaml := manifestbuilders.GetVirtualMachineGroupYaml(manifestbuilders.VirtualMachineGroupYaml{
					Namespace: input.WCPNamespaceName,
					Name:      groupName,
					Members: []vmopv1.GroupMember{
						{Kind: vmKind, Name: vm1Name},
						{Kind: vmKind, Name: vm2Name},
					},
				})
				Expect(clusterProxy.CreateWithArgs(ctx, groupYaml)).To(Succeed(), "failed to create VirtualMachineGroup %q", groupName)
				DeferCleanup(func() {
					Expect(clusterProxy.DeleteWithArgs(ctx, groupYaml)).To(Succeed(), "failed to delete VirtualMachineGroup %q", groupName)
					vmoperator.WaitForVirtualMachineGroupToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, groupName)
				})

				By(fmt.Sprintf("Creating VM %s and VM %s back to back, both with label %s=%s and affinity referencing it", vm1Name, vm2Name, affinityKey, value))
				for _, vmName := range []string{vm1Name, vm2Name} {
					Expect(clusterProxy.CreateWithArgs(ctx, buildVMYaml(vmName))).To(Succeed(), "failed to create VM %q", vmName)
					DeferCleanup(func() {
						vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
						if err == nil {
							Expect(svClusterClient.Delete(ctx, vm)).To(Succeed(), "failed to delete VM %q", vmName)
							vmoperator.WaitForVirtualMachineToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)
						}
					})
				}

				for _, vmName := range []string{vm1Name, vm2Name} {
					vmoperator.WaitForVirtualMachineToExist(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)
					vmoperator.WaitForVirtualMachineConditionCreated(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)
					vmoperator.WaitForVirtualMachinePowerState(ctx, config, svClusterClient, input.WCPNamespaceName, vmName, "PoweredOn")
				}

				By("Verifying one Tag CR carries both VMs as owners rather than one create failing or overwriting the other")
				vm1, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm1Name)
				Expect(err).ToNot(HaveOccurred())
				vm2, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm2Name)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, value, 2)
				Expect(tag.Spec.Key).To(Equal(affinityKey))
				Expect(tag.Spec.Value).To(Equal(value))
				ownerUIDs := make([]string, len(tag.OwnerReferences))
				for i, ref := range tag.OwnerReferences {
					ownerUIDs[i] = string(ref.UID)
				}
				Expect(ownerUIDs).To(ConsistOf(string(vm1.UID), string(vm2.UID)))

				By("Verifying both VMs carry the vCenter tag")
				waitForVCenterTag(vm1Name, affinityKey, value)
				waitForVCenterTag(vm2Name, affinityKey, value)
			})
	})
}
