package reconciliation

import (
	"fmt"
	"net/http"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	datastaxv1alpha1 "github.com/riptano/dse-operator/operator/pkg/apis/datastax/v1alpha1"
	"github.com/riptano/dse-operator/operator/pkg/dsereconciliation"
	"github.com/riptano/dse-operator/operator/pkg/dsereconciliation/reconcileriface"
	"github.com/riptano/dse-operator/operator/pkg/httphelper"
	"github.com/riptano/dse-operator/operator/pkg/utils"
)

// ReconcileRacks ...
type ReconcileRacks struct {
	ReconcileContext       *dsereconciliation.ReconciliationContext
	desiredRackInformation []*dsereconciliation.RackInformation
	statefulSets           []*appsv1.StatefulSet
}

// CalculateRackInformation determine how many nodes per rack are needed
func (r *ReconcileRacks) CalculateRackInformation() (reconcileriface.Reconciler, error) {

	r.ReconcileContext.ReqLogger.Info("reconcile_racks::calculateRackInformation")

	// Create RackInformation

	nodeCount := int(r.ReconcileContext.DseDatacenter.Spec.Size)
	racks := r.ReconcileContext.DseDatacenter.Spec.GetRacks()
	rackCount := len(racks)

	// TODO error if nodeCount < rackCount

	if r.ReconcileContext.DseDatacenter.Spec.Parked {
		nodeCount = 0
	}

	// 3 seeds per datacenter (this could be two, but we would like three seeds per cluster
	// and it's not easy for us to know if we're in a multi DC cluster in this part of the code)
	// OR all of the nodes, if there's less than 3
	// OR one per rack if there are four or more racks
	seedCount := 3
	if nodeCount < 3 {
		seedCount = nodeCount
	} else if rackCount > 3 {
		seedCount = rackCount
	}

	var desiredRackInformation []*dsereconciliation.RackInformation

	if rackCount < 1 {
		return nil, fmt.Errorf("assertion failed! rackCount should not possibly be zero here")
	}

	// nodes_per_rack = total_size / rack_count + 1 if rack_index < remainder

	nodesPerRack, extraNodes := nodeCount/rackCount, nodeCount%rackCount
	seedsPerRack, extraSeeds := seedCount/rackCount, seedCount%rackCount

	for rackIndex, dseRack := range racks {
		nodesForThisRack := nodesPerRack
		if rackIndex < extraNodes {
			nodesForThisRack++
		}
		seedsForThisRack := seedsPerRack
		if rackIndex < extraSeeds {
			seedsForThisRack++
		}
		nextRack := &dsereconciliation.RackInformation{}
		nextRack.RackName = dseRack.Name
		nextRack.NodeCount = nodesForThisRack
		nextRack.SeedCount = seedsForThisRack

		desiredRackInformation = append(desiredRackInformation, nextRack)
	}

	statefulSets := make([]*appsv1.StatefulSet, len(desiredRackInformation), len(desiredRackInformation))

	return &ReconcileRacks{
		ReconcileContext:       r.ReconcileContext,
		desiredRackInformation: desiredRackInformation,
		statefulSets:           statefulSets,
	}, nil
}

func (r *ReconcileRacks) CheckRackCreation() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckRackCreation")

	for idx, _ := range r.desiredRackInformation {
		rackInfo := r.desiredRackInformation[idx]

		// Does this rack have a statefulset?

		statefulSet, statefulSetFound, err := r.GetStatefulSetForRack(rackInfo)
		if err != nil {
			r.ReconcileContext.ReqLogger.Error(
				err,
				"Could not locate statefulSet for",
				"Rack", rackInfo.RackName)
			res := &reconcile.Result{Requeue: true}
			return res, err
		}

		if statefulSetFound == false {
			r.ReconcileContext.ReqLogger.Info(
				"Need to create new StatefulSet for",
				"Rack", rackInfo.RackName)
			res, err := r.ReconcileNextRack(statefulSet)
			return &res, err
		}

		r.statefulSets[idx] = statefulSet
	}

	return nil, nil
}

func (r *ReconcileRacks) CheckRackConfiguration() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("Examining config of StatefulSet")

	for idx, _ := range r.desiredRackInformation {
		//rackInfo := r.desiredRackInformation[idx]
		statefulSet := r.statefulSets[idx]
		currentConfig, desiredConfig, err := getConfigsForRackResource(r.ReconcileContext.DseDatacenter, statefulSet)
		if err != nil {
			r.ReconcileContext.ReqLogger.Error(err, "Error examining config of StatefulSet")
			res := reconcile.Result{Requeue: false}
			return &res, err
		}

		if currentConfig != desiredConfig {
			r.ReconcileContext.ReqLogger.Info("Updating config",
				"statefulSet", statefulSet,
				"current", currentConfig,
				"desired", desiredConfig)

			// The first env var should be the config
			err = setConfigFileData(statefulSet, desiredConfig)
			if err != nil {
				r.ReconcileContext.ReqLogger.Error(
					err,
					"Unable to update statefulSet PodTemplate with config",
					"statefulSet", statefulSet)
				res := reconcile.Result{Requeue: false}
				return &res, err
			}

			err = r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, statefulSet)
			if err != nil {
				r.ReconcileContext.ReqLogger.Error(
					err,
					"Unable to perform update on statefulset for config",
					"statefulSet", statefulSet)
				res := reconcile.Result{Requeue: false}
				return &res, err
			}

			// we just updated k8s and pods will be knocked out of ready state,
			// so go back through the reconcilation loop
			res := reconcile.Result{Requeue: true}
			return &res, err
		}
	}

	return nil, nil
}

func (r *ReconcileRacks) CheckRackLabels() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckRackLabels")

	for idx, _ := range r.desiredRackInformation {
		rackInfo := r.desiredRackInformation[idx]
		statefulSet := r.statefulSets[idx]

		// Has this statefulset been reconciled?

		stsLabels := statefulSet.GetLabels()
		shouldUpdateLabels, updatedLabels := shouldUpdateLabelsForRackResource(stsLabels, r.ReconcileContext.DseDatacenter, rackInfo.RackName)

		if shouldUpdateLabels {
			r.ReconcileContext.ReqLogger.Info("Updating labels",
				"statefulSet", statefulSet,
				"current", stsLabels,
				"desired", updatedLabels)
			statefulSet.SetLabels(updatedLabels)

			if err := r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, statefulSet); err != nil {
				r.ReconcileContext.ReqLogger.Info("Unable to update statefulSet with labels",
					"statefulSet", statefulSet)
			}
		}
	}

	// FIXME we never return anything else
	return nil, nil
}

func (r *ReconcileRacks) CheckRackParkedState() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckRackParkedState")

	for idx, _ := range r.desiredRackInformation {
		rackInfo := r.desiredRackInformation[idx]
		statefulSet := r.statefulSets[idx]

		parked := r.ReconcileContext.DseDatacenter.Spec.Parked
		currentPodCount := *statefulSet.Spec.Replicas

		var desiredNodeCount int32
		if parked {
			// rackInfo.NodeCount should be passed in as zero for parked clusters
			desiredNodeCount = int32(rackInfo.NodeCount)
		} else if currentPodCount > 1 {
			// already gone through the first round of scaling seed nodes, now lets add the rest of the nodes
			desiredNodeCount = int32(rackInfo.NodeCount)
		} else {
			// not parked and we just want to get our first seed up fully
			desiredNodeCount = int32(1)
		}

		if parked && currentPodCount > 0 {
			r.ReconcileContext.ReqLogger.Info(
				"DseDatacenter is parked, setting rack to zero replicas",
				"Rack", rackInfo.RackName,
				"currentSize", currentPodCount,
				"desiredSize", desiredNodeCount,
			)

			// TODO we should call a more graceful stop node command here

			res, err := r.UpdateRackNodeCount(statefulSet, desiredNodeCount)
			return &res, err
		}
	}

	return nil, nil
}

func (r *ReconcileRacks) CheckRackSeedsReady() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckRackSeedsReady")

	for idx, _ := range r.desiredRackInformation {
		rackInfo := r.desiredRackInformation[idx]
		statefulSet := r.statefulSets[idx]

		r.ReconcileContext.ReqLogger.Info(
			"StatefulSet found",
			"ResourceVersion", statefulSet.ResourceVersion)

		r.LabelSeedPods(statefulSet)

		desiredSeedCount := int32(rackInfo.SeedCount)
		maxReplicas := *statefulSet.Spec.Replicas
		readyReplicas := statefulSet.Status.ReadyReplicas

		// this is needed because we will be passing through here after all of our seeds are up
		// and the maxReplicas will be the full rack size, and we want the scaling up for
		// non-seeds to happen in CheckRackScaleReady below
		if readyReplicas >= desiredSeedCount {
			continue
		}

		if readyReplicas < maxReplicas {
			// We should do nothing but wait until all replicas are ready
			r.ReconcileContext.ReqLogger.Info(
				"Not all seeds for StatefulSet are ready.",
				"maxReplicas", maxReplicas,
				"readyCount", readyReplicas)

			res := reconcile.Result{Requeue: true}
			return &res, nil

		}
		if readyReplicas < desiredSeedCount {
			res, err := r.UpdateRackNodeCount(statefulSet, readyReplicas+1)
			return &res, err
		}
	}

	return nil, nil
}

func (r *ReconcileRacks) CheckRackScaleReady() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckRackScaleReady")

	for idx, _ := range r.desiredRackInformation {
		rackInfo := r.desiredRackInformation[idx]
		statefulSet := r.statefulSets[idx]

		// By the time we get here we know all the racks are ready for that particular size

		readyReplicas := statefulSet.Status.ReadyReplicas
		desiredNodeCount := int32(rackInfo.NodeCount)
		maxReplicas := *statefulSet.Spec.Replicas

		if readyReplicas < maxReplicas {
			// We should do nothing but wait until all replicas are ready
			r.ReconcileContext.ReqLogger.Info(
				"Not all replicas for StatefulSet are ready.",
				"maxReplicas", maxReplicas,
				"readyCount", readyReplicas)

			res := reconcile.Result{Requeue: true}
			return &res, nil
		}

		if maxReplicas < desiredNodeCount {
			if !isClusterHealthy(r.ReconcileContext) {
				res := reconcile.Result{Requeue: true}
				return &res, nil
			}

			// update it
			r.ReconcileContext.ReqLogger.Info(
				"Need to update the rack's node count by one",
				"Rack", rackInfo.RackName,
				"maxReplicas", maxReplicas,
				"desiredSize", desiredNodeCount,
			)

			res, err := r.UpdateRackNodeCount(statefulSet, maxReplicas+1)
			return &res, err
		}

		if readyReplicas > desiredNodeCount {
			// too many ready replicas, how did this happen?
			r.ReconcileContext.ReqLogger.Info(
				"Too many replicas for StatefulSet are ready",
				"desiredCount", desiredNodeCount,
				"readyCount", readyReplicas)
			res := reconcile.Result{Requeue: true}
			return &res, nil
		}

		r.ReconcileContext.ReqLogger.Info(
			"All replicas are ready for StatefulSet for",
			"rack", rackInfo.RackName)
	}

	return nil, nil
}

func (r *ReconcileRacks) CheckRackPodLabels() (*reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::CheckRackPodLabels")

	for idx, _ := range r.desiredRackInformation {
		statefulSet := r.statefulSets[idx]

		if err := r.ReconcilePods(statefulSet); err != nil {
			res := reconcile.Result{Requeue: true}
			return &res, nil
		}
	}

	return nil, nil
}

// Apply reconcileRacks determines if a rack needs to be reconciled.
func (r *ReconcileRacks) Apply() (reconcile.Result, error) {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::Apply")

	recResult, err := r.CheckRackCreation()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckRackLabels()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckRackParkedState()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckRackSeedsReady()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckRackScaleReady()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckRackConfiguration()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	recResult, err = r.CheckRackPodLabels()
	if recResult != nil || err != nil {
		return *recResult, err
	}

	if err := addOperatorProgressLabel(r.ReconcileContext, ready); err != nil {
		// this error is especially sad because we were just about to be done reconciling
		return reconcile.Result{Requeue: true}, err
	}

	r.ReconcileContext.ReqLogger.Info("All StatefulSets should now be reconciled.")

	return reconcile.Result{}, nil
}

func isClusterHealthy(rc *dsereconciliation.ReconciliationContext) bool {
	selector := map[string]string{
		datastaxv1alpha1.CLUSTER_LABEL: rc.DseDatacenter.Spec.ClusterName,
	}
	podList, err := listPods(rc, selector)
	if err != nil {
		rc.ReqLogger.Error(err, "no pods found for DseDatacenter")
		return false
	}

	for _, pod := range podList.Items {
		rc.ReqLogger.Info("requesting Cluster Health status from DSE Node Management API",
			"pod", pod.Name)

		request := httphelper.NodeMgmtRequest{
			Endpoint: fmt.Sprintf("/api/v0/probes/cluster?consistency_level=LOCAL_QUORUM&rf_per_dc=%d", len(rc.DseDatacenter.Spec.Racks)),
			Host:     httphelper.GetPodHost(pod.Name, rc.DseDatacenter.Spec.ClusterName, rc.DseDatacenter.Name, rc.DseDatacenter.Namespace),
			Client:   http.DefaultClient,
			Method:   http.MethodGet,
		}

		if err := httphelper.CallNodeMgmtEndpoint(rc.ReqLogger, request); err != nil {
			return false
		}
	}

	return true
}

// LabelSeedPods will iterate over all seed node pods for a datacenter and if the pod exists
// and is not already labeled will add the dse-seed=true label to the pod so that its picked
// up by the headless seed service
func (r *ReconcileRacks) LabelSeedPods(statefulSet *appsv1.StatefulSet) {
	seeds := r.ReconcileContext.DseDatacenter.GetSeedList()
	for _, seed := range seeds {
		podName := strings.Split(seed, ".")[0]
		pod := &corev1.Pod{}
		err := r.ReconcileContext.Client.Get(
			r.ReconcileContext.Ctx,
			types.NamespacedName{
				Name:      podName,
				Namespace: statefulSet.Namespace},
			pod)
		if err != nil {
			r.ReconcileContext.ReqLogger.Info("Unable to get seed pod",
				"pod",
				podName)
			return
		}

		podLabels := pod.GetLabels()

		if _, ok := podLabels[datastaxv1alpha1.SEED_NODE_LABEL]; !ok {
			podLabels[datastaxv1alpha1.SEED_NODE_LABEL] = "true"
			pod.SetLabels(podLabels)

			if err := r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, pod); err != nil {
				r.ReconcileContext.ReqLogger.Info("Unable to update pod with seed label",
					"pod",
					podName)
			}
		}
	}
}

// GetStatefulSetForRack returns the statefulset for the rack
// and whether it currently exists and whether an error occured
func (r *ReconcileRacks) GetStatefulSetForRack(
	nextRack *dsereconciliation.RackInformation) (*appsv1.StatefulSet, bool, error) {

	r.ReconcileContext.ReqLogger.Info("reconcile_racks::getStatefulSetForRack")

	// Check if the desiredStatefulSet already exists
	currentStatefulSet := &appsv1.StatefulSet{}
	err := r.ReconcileContext.Client.Get(
		r.ReconcileContext.Ctx,
		newNamespacedNameForStatefulSet(r.ReconcileContext.DseDatacenter, nextRack.RackName),
		currentStatefulSet)

	if err == nil {
		return currentStatefulSet, true, nil
	}

	if !errors.IsNotFound(err) {
		return nil, false, err
	}

	desiredStatefulSet, err := newStatefulSetForDseDatacenter(
		nextRack.RackName,
		r.ReconcileContext.DseDatacenter,
		0)
	if err != nil {
		return nil, false, err
	}

	// Set dseDatacenter dseDatacenter as the owner and controller
	err = setControllerReference(
		r.ReconcileContext.DseDatacenter,
		desiredStatefulSet,
		r.ReconcileContext.Scheme)
	if err != nil {
		return nil, false, err
	}

	return desiredStatefulSet, false, nil
}

// ReconcileNextRack ensures that the resources for a dse rack have been properly created
// Note that each statefulset is using OrderedReadyPodManagement,
// so it will bring up one node at a time.
func (r *ReconcileRacks) ReconcileNextRack(statefulSet *appsv1.StatefulSet) (reconcile.Result, error) {

	r.ReconcileContext.ReqLogger.Info("reconcile_racks::reconcileNextRack")

	if err := addOperatorProgressLabel(r.ReconcileContext, updating); err != nil {
		return reconcile.Result{Requeue: true}, err
	}

	// Create the StatefulSet
	r.ReconcileContext.ReqLogger.Info(
		"Creating a new StatefulSet.",
		"statefulSetNamespace",
		statefulSet.Namespace,
		"statefulSetName",
		statefulSet.Name)
	err := r.ReconcileContext.Client.Create(
		r.ReconcileContext.Ctx,
		statefulSet)
	if err != nil {
		return reconcile.Result{Requeue: true}, err
	}

	//
	// Create a PodDisruptionBudget for the StatefulSet
	//

	desiredBudget := newPodDisruptionBudgetForStatefulSet(
		r.ReconcileContext.DseDatacenter,
		statefulSet)

	// Set DseDatacenter dseDatacenter as the owner and controller
	err = setControllerReference(
		r.ReconcileContext.DseDatacenter,
		desiredBudget,
		r.ReconcileContext.Scheme)
	if err != nil {
		return reconcile.Result{Requeue: true}, err
	}

	// Check if the budget already exists
	currentBudget := &policyv1beta1.PodDisruptionBudget{}
	err = r.ReconcileContext.Client.Get(
		r.ReconcileContext.Ctx,
		types.NamespacedName{
			Name:      desiredBudget.Name,
			Namespace: desiredBudget.Namespace},
		currentBudget)

	if err != nil && errors.IsNotFound(err) {
		// Create the Budget
		r.ReconcileContext.ReqLogger.Info(
			"Creating a new PodDisruptionBudget.",
			"podDisruptionBudgetNamespace:",
			desiredBudget.Namespace,
			"podDisruptionBudgetName:",
			desiredBudget.Name)
		err = r.ReconcileContext.Client.Create(
			r.ReconcileContext.Ctx,
			desiredBudget)
		if err != nil {
			return reconcile.Result{Requeue: true}, err
		}
	}

	return reconcile.Result{}, nil
}

// UpdateRackNodeCount ...
func (r *ReconcileRacks) UpdateRackNodeCount(statefulSet *appsv1.StatefulSet, newNodeCount int32) (reconcile.Result, error) {

	r.ReconcileContext.ReqLogger.Info("reconcile_racks::updateRack")

	r.ReconcileContext.ReqLogger.Info(
		"updating StatefulSet node count",
		"statefulSetNamespace", statefulSet.Namespace,
		"statefulSetName", statefulSet.Name,
		"newNodeCount", newNodeCount,
	)

	if err := addOperatorProgressLabel(r.ReconcileContext, updating); err != nil {
		return reconcile.Result{Requeue: true}, err
	}

	statefulSet.Spec.Replicas = &newNodeCount

	err := r.ReconcileContext.Client.Update(
		r.ReconcileContext.Ctx,
		statefulSet)

	return reconcile.Result{Requeue: true}, err
}

// ReconcilePods ...
func (r *ReconcileRacks) ReconcilePods(statefulSet *appsv1.StatefulSet) error {
	r.ReconcileContext.ReqLogger.Info("reconcile_racks::ReconcilePods")

	for i := int32(0); i < statefulSet.Status.Replicas; i++ {
		podName := fmt.Sprintf("%s-%v", statefulSet.Name, i)

		pod := &corev1.Pod{}
		err := r.ReconcileContext.Client.Get(
			r.ReconcileContext.Ctx,
			types.NamespacedName{
				Name:      podName,
				Namespace: statefulSet.Namespace},
			pod)
		if err != nil {
			r.ReconcileContext.ReqLogger.Info("Unable to get pod",
				"Pod",
				podName)
			return err
		}

		podLabels := pod.GetLabels()
		shouldUpdateLabels, updatedLabels := shouldUpdateLabelsForRackResource(podLabels, r.ReconcileContext.DseDatacenter, statefulSet.GetLabels()[datastaxv1alpha1.RACK_LABEL])
		if shouldUpdateLabels {
			r.ReconcileContext.ReqLogger.Info("Updating labels",
				"Pod", podName,
				"current", podLabels,
				"desired", updatedLabels)
			pod.SetLabels(updatedLabels)

			if err := r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, pod); err != nil {
				r.ReconcileContext.ReqLogger.Info("Unable to update pod with label",
					"Pod",
					podName)
			}
		}

		if pod.Spec.Volumes == nil || len(pod.Spec.Volumes) == 0 || pod.Spec.Volumes[0].PersistentVolumeClaim == nil {
			continue
		}

		pvcName := pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName
		pvc := &corev1.PersistentVolumeClaim{
			TypeMeta: metav1.TypeMeta{
				Kind:       "PersistentVolumeClaim",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: statefulSet.Namespace,
			},
		}
		err = r.ReconcileContext.Client.Get(
			r.ReconcileContext.Ctx,
			types.NamespacedName{
				Name:      pvcName,
				Namespace: statefulSet.Namespace},
			pvc)
		if err != nil {
			r.ReconcileContext.ReqLogger.Info("Unable to get pvc",
				"PVC",
				pvcName)
			return err
		}

		pvcLabels := pvc.GetLabels()
		shouldUpdateLabels, updatedLabels = shouldUpdateLabelsForRackResource(pvcLabels, r.ReconcileContext.DseDatacenter, statefulSet.GetLabels()[datastaxv1alpha1.RACK_LABEL])
		if shouldUpdateLabels {
			r.ReconcileContext.ReqLogger.Info("Updating labels",
				"PVC", pvc,
				"current", pvcLabels,
				"desired", updatedLabels)

			pvc.SetLabels(updatedLabels)

			if err := r.ReconcileContext.Client.Update(r.ReconcileContext.Ctx, pvc); err != nil {
				r.ReconcileContext.ReqLogger.Info("Unable to update pvc with labels",
					"PVC",
					pvc)
			}
		}
	}

	return nil
}

// shouldUpdateLabelsForClusterResource will compare the labels passed in with what the labels should be for a cluster level
// resource. It will return the updated map and a boolean denoting whether the resource needs to be updated with the new labels.
func shouldUpdateLabelsForClusterResource(resourceLabels map[string]string, dseDatacenter *datastaxv1alpha1.DseDatacenter) (bool, map[string]string) {
	labelsUpdated := false

	if resourceLabels == nil {
		resourceLabels = make(map[string]string)
	}

	if _, ok := resourceLabels[datastaxv1alpha1.CLUSTER_LABEL]; !ok {
		labelsUpdated = true
	} else if resourceLabels[datastaxv1alpha1.CLUSTER_LABEL] != dseDatacenter.Spec.ClusterName {
		labelsUpdated = true
	}

	if labelsUpdated {
		utils.MergeMap(&resourceLabels, dseDatacenter.GetClusterLabels())
	}

	return labelsUpdated, resourceLabels
}

// shouldUpdateLabelsForRackResource will compare the labels passed in with what the labels should be for a rack level
// resource. It will return the updated map and a boolean denoting whether the resource needs to be updated with the new labels.
func shouldUpdateLabelsForRackResource(resourceLabels map[string]string, dseDatacenter *datastaxv1alpha1.DseDatacenter, rackName string) (bool, map[string]string) {
	labelsUpdated, resourceLabels := shouldUpdateLabelsForDatacenterResource(resourceLabels, dseDatacenter)

	if _, ok := resourceLabels[datastaxv1alpha1.RACK_LABEL]; !ok {
		labelsUpdated = true
	} else if resourceLabels[datastaxv1alpha1.RACK_LABEL] != rackName {
		labelsUpdated = true
	}

	if labelsUpdated {
		utils.MergeMap(&resourceLabels, dseDatacenter.GetRackLabels(rackName))
	}

	return labelsUpdated, resourceLabels
}

// shouldUpdateLabelsForDatacenterResource will compare the labels passed in with what the labels should be for a datacenter level
// resource. It will return the updated map and a boolean denoting whether the resource needs to be updated with the new labels.
func shouldUpdateLabelsForDatacenterResource(resourceLabels map[string]string, dseDatacenter *datastaxv1alpha1.DseDatacenter) (bool, map[string]string) {
	labelsUpdated, resourceLabels := shouldUpdateLabelsForClusterResource(resourceLabels, dseDatacenter)

	if _, ok := resourceLabels[datastaxv1alpha1.DATACENTER_LABEL]; !ok {
		labelsUpdated = true
	} else if resourceLabels[datastaxv1alpha1.DATACENTER_LABEL] != dseDatacenter.Name {
		labelsUpdated = true
	}

	if labelsUpdated {
		utils.MergeMap(&resourceLabels, dseDatacenter.GetDatacenterLabels())
	}

	return labelsUpdated, resourceLabels
}

// getConfigsForRackResource return the desired and current configs for a statefulset
func getConfigsForRackResource(dseDatacenter *datastaxv1alpha1.DseDatacenter, statefulSet *appsv1.StatefulSet) (string, string, error) {
	currentConfig, err := getConfigFileData(statefulSet)
	if err != nil {
		return "", "", err
	}

	desiredConfig, err := dseDatacenter.GetConfigAsJSON()
	if err != nil {
		return "", "", err
	}

	return currentConfig, desiredConfig, nil
}

// getConfigFileData returns the current CONFIG_FILE_DATA or an error
func getConfigFileData(statefulSet *appsv1.StatefulSet) (string, error) {
	if "CONFIG_FILE_DATA" == statefulSet.Spec.Template.Spec.InitContainers[0].Env[0].Name {
		return statefulSet.Spec.Template.Spec.InitContainers[0].Env[0].Value, nil
	}
	return "", fmt.Errorf("CONFIG_FILE_DATA environment variable not available in StatefulSet")
}

// setConfigFileData updates the CONFIG_FILE_DATA in a statefulset.
func setConfigFileData(statefulSet *appsv1.StatefulSet, desiredConfig string) error {
	if "CONFIG_FILE_DATA" == statefulSet.Spec.Template.Spec.InitContainers[0].Env[0].Name {
		statefulSet.Spec.Template.Spec.InitContainers[0].Env[0].Value = desiredConfig
		return nil
	}
	return fmt.Errorf("CONFIG_FILE_DATA environment variable not available in StatefulSet")
}