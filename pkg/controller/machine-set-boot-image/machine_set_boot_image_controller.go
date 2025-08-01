package machineset

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	opv1 "github.com/openshift/api/operator/v1"
	configinformersv1 "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configlistersv1 "github.com/openshift/client-go/config/listers/config/v1"
	mcopclientset "github.com/openshift/client-go/operator/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreinformersv1 "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/scheme"

	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	machineclientset "github.com/openshift/client-go/machine/clientset/versioned"
	mapimachineinformers "github.com/openshift/client-go/machine/informers/externalversions/machine/v1beta1"
	machinelisters "github.com/openshift/client-go/machine/listers/machine/v1beta1"

	mcopinformersv1 "github.com/openshift/client-go/operator/informers/externalversions/operator/v1"
	mcoplistersv1 "github.com/openshift/client-go/operator/listers/operator/v1"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
)

// Controller defines the machine-set-boot-image controller.
type Controller struct {
	kubeClient    clientset.Interface
	machineClient machineclientset.Interface
	mcopClient    mcopclientset.Interface
	eventRecorder record.EventRecorder

	mcoCmLister          corelisterv1.ConfigMapLister
	mapiMachineSetLister machinelisters.MachineSetLister
	infraLister          configlistersv1.InfrastructureLister
	mcopLister           mcoplistersv1.MachineConfigurationLister

	mcoCmListerSynced          cache.InformerSynced
	mapiMachineSetListerSynced cache.InformerSynced
	infraListerSynced          cache.InformerSynced
	mcopListerSynced           cache.InformerSynced

	mapiStats                  MachineResourceStats
	capiMachineSetStats        MachineResourceStats
	capiMachineDeploymentStats MachineResourceStats
	mapiBootImageState         map[string]BootImageState
	conditionMutex             sync.Mutex
	mapiSyncMutex              sync.Mutex

	fgHandler ctrlcommon.FeatureGatesHandler
}

// Stats structure for local bookkeeping of machine resources
type MachineResourceStats struct {
	inProgress   int
	erroredCount int
	totalCount   int
}

// State structure uses for detecting hot loops. Reset when cluster is opted
// out of boot image updates.
type BootImageState struct {
	value        []byte
	hotLoopCount int
}

// Helper function that checks if all resources have been evaluated
func (mrs MachineResourceStats) isFinished() bool {
	return mrs.totalCount == (mrs.inProgress + mrs.erroredCount)
}

const (
	// Name of machine api namespace
	MachineAPINamespace = "openshift-machine-api"

	// Key to access stream data from the boot images configmap
	StreamConfigMapKey = "stream"

	// Labels and Annotations required for determining architecture of a machineset
	MachineSetArchAnnotationKey = "capacity.cluster-autoscaler.kubernetes.io/labels"

	ArchLabelKey = "kubernetes.io/arch="
	OSLabelKey   = "machine.openshift.io/os-id"

	// Threshold for hot loop detection
	HotLoopLimit = 3
)

// New returns a new machine-set-boot-image controller.
func New(
	kubeClient clientset.Interface,
	machineClient machineclientset.Interface,
	mcoCmInfomer coreinformersv1.ConfigMapInformer,
	mapiMachineSetInformer mapimachineinformers.MachineSetInformer,
	infraInformer configinformersv1.InfrastructureInformer,
	mcopClient mcopclientset.Interface,
	mcopInformer mcopinformersv1.MachineConfigurationInformer,
	fgHandler ctrlcommon.FeatureGatesHandler,
) *Controller {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})

	ctrl := &Controller{
		kubeClient:    kubeClient,
		machineClient: machineClient,
		mcopClient:    mcopClient,
		eventRecorder: ctrlcommon.NamespacedEventRecorder(eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "machineconfigcontroller-machinesetbootimagecontroller"})),
	}

	ctrl.mcoCmLister = mcoCmInfomer.Lister()
	ctrl.mapiMachineSetLister = mapiMachineSetInformer.Lister()
	ctrl.infraLister = infraInformer.Lister()
	ctrl.mcopLister = mcopInformer.Lister()

	ctrl.mcoCmListerSynced = mcoCmInfomer.Informer().HasSynced
	ctrl.mapiMachineSetListerSynced = mapiMachineSetInformer.Informer().HasSynced
	ctrl.infraListerSynced = infraInformer.Informer().HasSynced
	ctrl.mcopListerSynced = mcopInformer.Informer().HasSynced

	mapiMachineSetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addMAPIMachineSet,
		UpdateFunc: ctrl.updateMAPIMachineSet,
		DeleteFunc: ctrl.deleteMAPIMachineSet,
	})

	mcoCmInfomer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addConfigMap,
		UpdateFunc: ctrl.updateConfigMap,
		DeleteFunc: ctrl.deleteConfigMap,
	})

	mcopInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addMachineConfiguration,
		UpdateFunc: ctrl.updateMachineConfiguration,
		DeleteFunc: ctrl.deleteMachineConfiguration,
	})

	ctrl.fgHandler = fgHandler

	ctrl.mapiBootImageState = map[string]BootImageState{}

	return ctrl
}

// Run executes the machine-set-boot-image controller.
func (ctrl *Controller) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	if !cache.WaitForCacheSync(stopCh, ctrl.mcoCmListerSynced, ctrl.mapiMachineSetListerSynced, ctrl.infraListerSynced, ctrl.mcopListerSynced) {
		return
	}

	klog.Info("Starting MachineConfigController-MachineSetBootImageController")
	defer klog.Info("Shutting down MachineConfigController-MachineSetBootImageController")

	<-stopCh
}

func (ctrl *Controller) addMAPIMachineSet(obj interface{}) {

	machineSet := obj.(*machinev1beta1.MachineSet)

	klog.Infof("MAPI MachineSet %s added, reconciling enrolled machine resources", machineSet.Name)

	// Update/Check all machinesets instead of just this one. This prevents needing to maintain a local
	// store of machineset conditions. As this is using a lister, it is relatively inexpensive to do
	// this.
	go func() { ctrl.syncMAPIMachineSets("MAPIMachinesetAdded") }()
}

func (ctrl *Controller) updateMAPIMachineSet(oldMS, newMS interface{}) {

	oldMachineSet := oldMS.(*machinev1beta1.MachineSet)
	newMachineSet := newMS.(*machinev1beta1.MachineSet)

	// Don't take action if the there is no change in the MachineSet's ProviderSpec, labels, annotations and ownerreferences
	if reflect.DeepEqual(oldMachineSet.Spec.Template.Spec.ProviderSpec, newMachineSet.Spec.Template.Spec.ProviderSpec) &&
		reflect.DeepEqual(oldMachineSet.GetLabels(), newMachineSet.GetLabels()) &&
		reflect.DeepEqual(oldMachineSet.GetAnnotations(), newMachineSet.GetAnnotations()) &&
		reflect.DeepEqual(oldMachineSet.GetOwnerReferences(), newMachineSet.GetOwnerReferences()) {
		return
	}

	klog.Infof("MachineSet %s updated, reconciling enrolled machineset resources", oldMachineSet.Name)

	// Update all machinesets instead of just this one. This prevents needing to maintain a local
	// store of machineset conditions. As this is using a lister, it is relatively inexpensive to do
	// this.
	go func() { ctrl.syncMAPIMachineSets("MAPIMachinesetUpdated") }()
}

func (ctrl *Controller) deleteMAPIMachineSet(deletedMS interface{}) {

	deletedMachineSet := deletedMS.(*machinev1beta1.MachineSet)

	klog.Infof("MachineSet %s deleted, reconciling enrolled machineset resources", deletedMachineSet.Name)

	// Update all machinesets. This prevents needing to maintain a local
	// store of machineset conditions. As this is using a lister, it is relatively inexpensive to do
	// this.
	go func() { ctrl.syncMAPIMachineSets("MAPIMachinesetDeleted") }()
}

func (ctrl *Controller) addConfigMap(obj interface{}) {

	configMap := obj.(*corev1.ConfigMap)

	// Take no action if this isn't the "golden" config map
	if configMap.Name != ctrlcommon.BootImagesConfigMapName {
		return
	}

	klog.Infof("configMap %s added, reconciling enrolled machine resources", configMap.Name)

	// Update all machinesets since the "golden" configmap has been added
	// TODO: Add go routines for CAPI resources here
	go func() { ctrl.syncMAPIMachineSets("BootImageConfigMapAdded") }()
}

func (ctrl *Controller) updateConfigMap(oldCM, newCM interface{}) {

	oldConfigMap := oldCM.(*corev1.ConfigMap)
	newConfigMap := newCM.(*corev1.ConfigMap)

	// Take no action if this isn't the "golden" config map
	if oldConfigMap.Name != ctrlcommon.BootImagesConfigMapName {
		return
	}

	// Only take action if the there is an actual change in the configMap Object
	if oldConfigMap.ResourceVersion == newConfigMap.ResourceVersion {
		return
	}

	klog.Infof("configMap %s updated, reconciling enrolled machine resources", oldConfigMap.Name)

	// Update all machinesets since the "golden" configmap has been updated
	// TODO: Add go routines for CAPI resources here
	go func() { ctrl.syncMAPIMachineSets("BootImageConfigMapUpdated") }()
}

func (ctrl *Controller) deleteConfigMap(obj interface{}) {

	configMap := obj.(*corev1.ConfigMap)

	// Take no action if this isn't the "golden" config map
	if configMap.Name != ctrlcommon.BootImagesConfigMapName {
		return
	}

	klog.Infof("configMap %s deleted, reconciling enrolled machine resources", configMap.Name)

	// Update all machinesets since the "golden" configmap has been deleted
	go func() { ctrl.syncMAPIMachineSets("BootImageConfigMapDeleted") }()
}

func (ctrl *Controller) addMachineConfiguration(obj interface{}) {

	machineConfiguration := obj.(*opv1.MachineConfiguration)

	// Take no action if this isn't the "cluster" level MachineConfiguration object
	if machineConfiguration.Name != ctrlcommon.MCOOperatorKnobsObjectName {
		klog.V(4).Infof("MachineConfiguration %s updated, but does not match %s, skipping bootimage sync", machineConfiguration.Name, ctrlcommon.MCOOperatorKnobsObjectName)
		return
	}

	klog.Infof("Bootimages management configuration has been added, reconciling enrolled machine resources")

	// Update/Check machinesets since the boot images configuration knob was updated
	// TODO: Add go routines for CAPI resources here
	go func() { ctrl.syncMAPIMachineSets("BootImageUpdateConfigurationAdded") }()
}

func (ctrl *Controller) updateMachineConfiguration(oldMC, newMC interface{}) {

	oldMachineConfiguration := oldMC.(*opv1.MachineConfiguration)
	newMachineConfiguration := newMC.(*opv1.MachineConfiguration)

	// Take no action if this isn't the "cluster" level MachineConfiguration object
	if oldMachineConfiguration.Name != ctrlcommon.MCOOperatorKnobsObjectName {
		klog.V(4).Infof("MachineConfiguration %s updated, but does not match %s, skipping bootimage sync", oldMachineConfiguration.Name, ctrlcommon.MCOOperatorKnobsObjectName)
		return
	}

	// Only take action if the there is an actual change in the MachineConfiguration's ManagedBootImagesStatus
	if reflect.DeepEqual(oldMachineConfiguration.Status.ManagedBootImagesStatus, newMachineConfiguration.Status.ManagedBootImagesStatus) {
		return
	}

	klog.Infof("Bootimages management configuration has been updated, reconciling enrolled machine resources")

	// Update all machinesets since the boot images configuration knob was updated
	// TODO: Add go routines for CAPI resources here
	go func() { ctrl.syncMAPIMachineSets("BootImageUpdateConfigurationUpdated") }()
}

func (ctrl *Controller) deleteMachineConfiguration(obj interface{}) {

	machineConfiguration := obj.(*opv1.MachineConfiguration)

	// Take no action if this isn't the "cluster" level MachineConfiguration object
	if machineConfiguration.Name != ctrlcommon.MCOOperatorKnobsObjectName {
		klog.V(4).Infof("MachineConfiguration %s deleted, but does not match %s, skipping bootimage sync", machineConfiguration.Name, ctrlcommon.MCOOperatorKnobsObjectName)
		return
	}

	klog.Infof("Bootimages management configuration has been deleted, reconciling enrolled machine resources")

	// Update/Check machinesets since the boot images configuration knob was updated
	// TODO: Add go routines for CAPI resources here
	go func() { ctrl.syncMAPIMachineSets("BootImageUpdateConfigurationDeleted") }()
}

func (ctrl *Controller) updateConditions(newReason string, syncError error, targetConditionType string) {
	ctrl.conditionMutex.Lock()
	defer ctrl.conditionMutex.Unlock()
	mcop, err := ctrl.mcopClient.OperatorV1().MachineConfigurations().Get(context.TODO(), ctrlcommon.MCOOperatorKnobsObjectName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error updating progressing condition: %s", err)
		return
	}
	newConditions := mcop.Status.DeepCopy().Conditions
	// If no conditions exist, populate some sane defaults
	if newConditions == nil {
		newConditions = getDefaultConditions()
	}

	for i, condition := range newConditions {
		if condition.Type == targetConditionType {
			if condition.Type == opv1.MachineConfigurationBootImageUpdateProgressing {
				newConditions[i].Message = fmt.Sprintf("Reconciled %d of %d MAPI MachineSets | Reconciled %d of %d CAPI MachineSets | Reconciled %d of %d CAPI MachineDeployments", ctrl.mapiStats.inProgress, ctrl.mapiStats.totalCount, ctrl.capiMachineSetStats.inProgress, ctrl.capiMachineSetStats.totalCount, ctrl.capiMachineDeploymentStats.inProgress, ctrl.capiMachineDeploymentStats.totalCount)
				newConditions[i].Reason = newReason
				// If all machine resources have been processed, then the controller is no longer progressing.
				if ctrl.mapiStats.isFinished() && ctrl.capiMachineSetStats.isFinished() && ctrl.capiMachineDeploymentStats.isFinished() {
					newConditions[i].Status = metav1.ConditionFalse
				} else {
					newConditions[i].Status = metav1.ConditionTrue
				}
			} else if condition.Type == opv1.MachineConfigurationBootImageUpdateDegraded {
				if syncError == nil {
					newConditions[i].Message = fmt.Sprintf("%d Degraded MAPI MachineSets | %d Degraded CAPI MachineSets | %d CAPI MachineDeployments", ctrl.mapiStats.erroredCount, ctrl.capiMachineSetStats.erroredCount, ctrl.capiMachineDeploymentStats.erroredCount)
				} else {
					newConditions[i].Message = fmt.Sprintf("%d Degraded MAPI MachineSets | %d Degraded CAPI MachineSets | %d CAPI MachineDeployments | Error(s): %s", ctrl.mapiStats.erroredCount, ctrl.capiMachineSetStats.erroredCount, ctrl.capiMachineDeploymentStats.erroredCount, syncError.Error())
				}
				newConditions[i].Reason = newReason
				if syncError != nil {
					newConditions[i].Status = metav1.ConditionTrue
				} else {
					newConditions[i].Status = metav1.ConditionFalse
				}
			}
			// Check if there is a change in the condition before updating LastTransitionTime
			if len(mcop.Status.Conditions) == 0 || !reflect.DeepEqual(newConditions[i], mcop.Status.Conditions[i]) {
				newConditions[i].LastTransitionTime = metav1.Now()
			}
			break
		}
	}
	// Only make an API call if there is an update to the Conditions field
	if !reflect.DeepEqual(newConditions, mcop.Status.Conditions) {
		ctrl.updateMachineConfigurationStatus(mcop, newConditions)
	}
}

func (ctrl *Controller) updateMachineConfigurationStatus(mcop *opv1.MachineConfiguration, newConditions []metav1.Condition) {

	// Using a retry here as there may be concurrent reconiliation loops updating conditions for multiple
	// resources at the same time and their local stores may be out of date
	if !reflect.DeepEqual(mcop.Status.Conditions, newConditions) {
		klog.V(4).Infof("%v", newConditions)
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			mcop, err := ctrl.mcopClient.OperatorV1().MachineConfigurations().Get(context.TODO(), ctrlcommon.MCOOperatorKnobsObjectName, metav1.GetOptions{})
			if err != nil {
				return err
			}
			mcop.Status.Conditions = newConditions
			_, err = ctrl.mcopClient.OperatorV1().MachineConfigurations().UpdateStatus(context.TODO(), mcop, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
			return nil
		}); err != nil {
			klog.Errorf("error updating MachineConfiguration status: %v", err)
		}
	}
}

func getDefaultConditions() []metav1.Condition {
	// These are boilerplate conditions, with no machine resources enrolled.
	return []metav1.Condition{
		{
			Type:               opv1.MachineConfigurationBootImageUpdateProgressing,
			Message:            "Reconciled 0 of 0 MAPI MachineSets | Reconciled 0 of 0 CAPI MachineSets | Reconciled 0 of 0 CAPI MachineDeployments",
			Reason:             "NA",
			LastTransitionTime: metav1.Now(),
			Status:             metav1.ConditionFalse,
		},
		{
			Type:               opv1.MachineConfigurationBootImageUpdateDegraded,
			Message:            "0 Degraded MAPI MachineSets | 0 Degraded CAPI MachineSets | 0 CAPI MachineDeployments",
			Reason:             "NA",
			LastTransitionTime: metav1.Now(),
			Status:             metav1.ConditionFalse,
		}}

}
