package targetgroupbinding

import (
	"context"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	elbv2api "sigs.k8s.io/aws-load-balancer-controller/apis/elbv2/v1beta1"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/backend"
)

// AnnotationRegisterTargetsWithPodAZ opts a TargetGroupBinding into registering IP targets that sit
// outside the target group's VPC with the availability zone of the pod's node, instead of "all".
//
// AWS cannot detect the zone of an IP outside the target group's VPC, so the controller has to send
// one. Upstream sends "all", which makes every target eligible from every load balancer node and
// turns cross-zone=false into a no-op. Same-account only: zone names are randomized per account, so
// a cross-account target group would need the zone translated (upstream v3.6 does that).
const AnnotationRegisterTargetsWithPodAZ = "planetscale.com/register-targets-with-pod-az"

func registersTargetsWithPodAZ(tgb *elbv2api.TargetGroupBinding) bool {
	return tgb.Spec.IamRoleArnToAssume == "" && tgb.Annotations[AnnotationRegisterTargetsWithPodAZ] == "true"
}

// outOfVPCTargetAZ returns the availability zone to register an out-of-VPC pod target with.
//
// A failed lookup is an error, not a fallback to "all": targets are matched on IP:port, so a target
// registered with "all" is never corrected, and it would quietly reintroduce cross-zone routing.
// Failing requeues the reconcile instead.
func (m *defaultResourceManager) outOfVPCTargetAZ(ctx context.Context, tgb *elbv2api.TargetGroupBinding, endpoint backend.PodEndpoint) (*string, error) {
	if !registersTargetsWithPodAZ(tgb) {
		return awssdk.String("all"), nil
	}
	nodeName := endpoint.Pod.NodeName
	if nodeName == "" {
		return nil, errors.Errorf("pod %v has no node assigned", endpoint.Pod.Key)
	}
	// Nodes are already watched by the TargetGroupBinding controller, so this reads the informer cache.
	node := &corev1.Node{}
	if err := m.k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return nil, errors.Wrapf(err, "failed to get node %s for pod %v", nodeName, endpoint.Pod.Key)
	}
	zone, ok := node.Labels[corev1.LabelTopologyZone]
	if !ok || zone == "" {
		return nil, errors.Errorf("node %s has no %s label", nodeName, corev1.LabelTopologyZone)
	}
	return awssdk.String(zone), nil
}
