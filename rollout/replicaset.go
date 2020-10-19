package rollout

import (
	"context"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	patchtypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/controller"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	"github.com/argoproj/argo-rollouts/utils/defaults"
	replicasetutil "github.com/argoproj/argo-rollouts/utils/replicaset"
)

var controllerKind = v1alpha1.SchemeGroupVersion.WithKind("Rollout")

const (
	addScaleDownAtAnnotationsPatch    = `[{ "op": "add", "path": "/metadata/annotations/%s", "value": "%s"}]`
	removeScaleDownAtAnnotationsPatch = `[{ "op": "remove", "path": "/metadata/annotations/%s"}]`
)

func (c *rolloutContext) removeScaleDownDelay(rs *appsv1.ReplicaSet) error {
	ctx := context.TODO()
	c.log.Infof("Removing '%s' annotation on RS '%s'", v1alpha1.DefaultReplicaSetScaleDownDeadlineAnnotationKey, rs.Name)
	patch := fmt.Sprintf(removeScaleDownAtAnnotationsPatch, v1alpha1.DefaultReplicaSetScaleDownDeadlineAnnotationKey)
	_, err := c.kubeclientset.AppsV1().ReplicaSets(rs.Namespace).Patch(ctx, rs.Name, patchtypes.JSONPatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

func (c *rolloutContext) addScaleDownDelay(rs *appsv1.ReplicaSet) error {
	ctx := context.TODO()
	c.log.Infof("Adding '%s' annotation to RS '%s'", v1alpha1.DefaultReplicaSetScaleDownDeadlineAnnotationKey, rs.Name)
	scaleDownDelaySeconds := time.Duration(defaults.GetScaleDownDelaySecondsOrDefault(c.rollout))
	now := metav1.Now().Add(scaleDownDelaySeconds * time.Second).UTC().Format(time.RFC3339)
	patch := fmt.Sprintf(addScaleDownAtAnnotationsPatch, v1alpha1.DefaultReplicaSetScaleDownDeadlineAnnotationKey, now)
	_, err := c.kubeclientset.AppsV1().ReplicaSets(rs.Namespace).Patch(ctx, rs.Name, patchtypes.JSONPatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

func (c *Controller) getReplicaSetsForRollouts(r *v1alpha1.Rollout) ([]*appsv1.ReplicaSet, error) {
	ctx := context.TODO()
	// List all ReplicaSets to find those we own but that no longer match our
	// selector. They will be orphaned by ClaimReplicaSets().
	rsList, err := c.replicaSetLister.ReplicaSets(r.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}
	replicaSetSelector, err := metav1.LabelSelectorAsSelector(r.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("rollout %s/%s has invalid label selector: %v", r.Namespace, r.Name, err)
	}
	// If any adoptions are attempted, we should first recheck for deletion with
	// an uncached quorum read sometime after listing ReplicaSets (see #42639).
	canAdoptFunc := controller.RecheckDeletionTimestamp(func() (metav1.Object, error) {
		fresh, err := c.argoprojclientset.ArgoprojV1alpha1().Rollouts(r.Namespace).Get(ctx, r.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if fresh.UID != r.UID {
			return nil, fmt.Errorf("original Rollout %v/%v is gone: got uid %v, wanted %v", r.Namespace, r.Name, fresh.UID, r.UID)
		}
		return fresh, nil
	})
	cm := controller.NewReplicaSetControllerRefManager(c.replicaSetControl, r, replicaSetSelector, controllerKind, canAdoptFunc)
	return cm.ClaimReplicaSets(rsList)
}

// helper to handle the replica set scale down time
// if the scale down time has not passed, enqueue the rollout
func (c *rolloutContext) determineReplicaSetCountWhenScaling(
	targetRS *appsv1.ReplicaSet, defaultReplicaCount int32, desiredReplicaCount int32, scaleDownAtTime time.Time) int32 {
	logCtx := c.log
	rollout := c.rollout

	now := metav1.Now()
	scaleDownAt := metav1.NewTime(scaleDownAtTime)
	if scaleDownAt.After(now.Time) {
		remainingTime := scaleDownAt.Sub(now.Time)
		if remainingTime < c.resyncPeriod {
			// if the scale down time has not passed, enqueue the rollout
			logCtx.Infof("RS '%s' has not reached the scaleDownTime", targetRS.Name)
			c.enqueueRolloutAfter(rollout, remainingTime)
		}
		// set the replica count to be the desired replica count
		return desiredReplicaCount
	}
	// if it's after the scale down time
	return defaultReplicaCount
}

func (c *rolloutContext) reconcileNewReplicaSet() (bool, error) {
	logCtx := c.log
	rollout := c.rollout
	newRS := c.newRS
	if newRS == nil {
		return false, nil
	}

	newReplicasCount := int32(0)

	logCtx.Infof("Reconciling new ReplicaSet '%s'", newRS.Name)
	allRSs := c.allRSs

	scaleDownAtStr, scaleDownAnnotationAvailable := newRS.Annotations[v1alpha1.DefaultReplicaSetScaleDownDeadlineAnnotationKey]

	newIntendedReplicasCount, err := replicasetutil.NewRSNewReplicas(rollout, allRSs, newRS)
	if err != nil {
		return false, err
	}

	// if canary, use the new intended replica count
	// canary scales down downstream
	if rollout.Spec.Strategy.Canary != nil {
		newReplicasCount = newIntendedReplicasCount
		scaled, _, err := c.scaleReplicaSetAndRecordEvent(newRS, newReplicasCount)
		return scaled, err
	}

	// if blue green,

	// if the scale down annotation exists or the the rollout is an aborted state
	// will pass a replica count of 0 to the new rs (preview) to scale it down
	// if the annotation does not exist, or the rollout is not aborted,
	// use the intended replica number

	// if the rollout is paused and then aborted (this is done through the kubectl argo plugin)
	// use the intended replica number as the preview will be tagged with the
	// scale down annotation downstream
	if (!scaleDownAnnotationAvailable && !rollout.Status.Abort) || (rollout.Status.ControllerPause && rollout.Status.Abort) {
		newReplicasCount = newIntendedReplicasCount
	} else {
		// check the time interval / delay to scale down
		scaleDownAtTime, err := time.Parse(time.RFC3339, scaleDownAtStr)
		if err != nil {
			logCtx.Infof("No scaleDownAt label on new rs '%s'", newRS.Name)
		} else {
			newReplicasCount = c.determineReplicaSetCountWhenScaling(newRS, newReplicasCount, newIntendedReplicasCount, scaleDownAtTime)
		}
	}

	scaled, _, err := c.scaleReplicaSetAndRecordEvent(newRS, newReplicasCount)
	return scaled, err
}

func (c *rolloutContext) reconcileOldReplicaSets(oldRSs []*appsv1.ReplicaSet) (bool, error) {
	oldPodsCount := replicasetutil.GetReplicaCountForReplicaSets(oldRSs)
	if oldPodsCount == 0 {
		// Can't scale down further
		return false, nil
	}
	c.log.Infof("Reconciling old replica sets (count: %d)", oldPodsCount)

	var err error
	hasScaled := false
	if c.rollout.Spec.Strategy.Canary != nil {
		// Clean up unhealthy replicas first, otherwise unhealthy replicas will block rollout
		// and cause timeout. See https://github.com/kubernetes/kubernetes/issues/16737
		oldRSs, hasScaled, err = c.cleanupUnhealthyReplicas(oldRSs)
		if err != nil {
			return false, nil
		}
	}

	// Scale down old replica sets
	if c.rollout.Spec.Strategy.BlueGreen != nil {
		hasScaled, err = c.scaleDownOldReplicaSetsForBlueGreen(oldRSs)
		if err != nil {
			return false, nil
		}
	}

	return hasScaled, nil
}

// cleanupUnhealthyReplicas will scale down old replica sets with unhealthy replicas, so that all unhealthy replicas will be deleted.
func (c *rolloutContext) cleanupUnhealthyReplicas(oldRSs []*appsv1.ReplicaSet) ([]*appsv1.ReplicaSet, bool, error) {
	sort.Sort(controller.ReplicaSetsByCreationTimestamp(oldRSs))
	// Safely scale down all old replica sets with unhealthy replicas. Replica set will sort the pods in the order
	// such that not-ready < ready, unscheduled < scheduled, and pending < running. This ensures that unhealthy replicas will
	// been deleted first and won't increase unavailability.
	totalScaledDown := int32(0)
	for i, targetRS := range oldRSs {
		if *(targetRS.Spec.Replicas) == 0 {
			// cannot scale down this replica set.
			continue
		}
		c.log.Infof("Found %d available pods in old RS %s/%s", targetRS.Status.AvailableReplicas, targetRS.Namespace, targetRS.Name)
		if *(targetRS.Spec.Replicas) == targetRS.Status.AvailableReplicas {
			// no unhealthy replicas found, no scaling required.
			continue
		}

		scaledDownCount := *(targetRS.Spec.Replicas) - targetRS.Status.AvailableReplicas
		newReplicasCount := targetRS.Status.AvailableReplicas
		if newReplicasCount > *(targetRS.Spec.Replicas) {
			return nil, false, fmt.Errorf("when cleaning up unhealthy replicas, got invalid request to scale down %s/%s %d -> %d", targetRS.Namespace, targetRS.Name, *(targetRS.Spec.Replicas), newReplicasCount)
		}
		_, updatedOldRS, err := c.scaleReplicaSetAndRecordEvent(targetRS, newReplicasCount)
		if err != nil {
			return nil, totalScaledDown > 0, err
		}
		totalScaledDown += scaledDownCount
		oldRSs[i] = updatedOldRS
	}
	return oldRSs, totalScaledDown > 0, nil
}
