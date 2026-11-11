package targetgroupbinding

import (
	"context"
	"net/netip"
	"testing"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	elbv2types "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	elbv2api "sigs.k8s.io/aws-load-balancer-controller/apis/elbv2/v1beta1"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/backend"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/k8s"
	testclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func Test_prepareRegistrationCall_podAZ(t *testing.T) {
	zonedNode := func(name, zone string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{corev1.LabelTopologyZone: zone},
		}}
	}
	endpoint := func(ip, node string) backend.PodEndpoint {
		return backend.PodEndpoint{
			IP:   ip,
			Port: 6432,
			Pod: k8s.PodInfo{
				Key:      types.NamespacedName{Namespace: "exosphere", Name: "pod-" + ip},
				NodeName: node,
			},
		}
	}
	tgbWith := func(annotations map[string]string, assumeRole string) *elbv2api.TargetGroupBinding {
		return &elbv2api.TargetGroupBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: "exosphere", Name: "tgb", Annotations: annotations},
			Spec:       elbv2api.TargetGroupBindingSpec{IamRoleArnToAssume: assumeRole},
		}
	}
	optIn := map[string]string{AnnotationRegisterTargetsWithPodAZ: "true"}
	// 10.0.0.0/8 stands in for the target group's VPC; 172.16.0.0/12 is a peered VPC.
	outOfVPC := func(addr netip.Addr) bool { return !netip.MustParsePrefix("10.0.0.0/8").Contains(addr) }

	tests := []struct {
		name      string
		tgb       *elbv2api.TargetGroupBinding
		nodes     []*corev1.Node
		endpoints []backend.PodEndpoint
		want      []elbv2types.TargetDescription
		wantErr   bool
	}{
		{
			name:      "no annotation keeps upstream behavior",
			tgb:       tgbWith(nil, ""),
			nodes:     []*corev1.Node{zonedNode("node-a", "us-east-1a")},
			endpoints: []backend.PodEndpoint{endpoint("172.16.0.1", "node-a")},
			want:      []elbv2types.TargetDescription{{Id: awssdk.String("172.16.0.1"), Port: awssdk.Int32(6432), AvailabilityZone: awssdk.String("all")}},
		},
		{
			name:  "annotation registers out-of-VPC targets with the node's zone",
			tgb:   tgbWith(optIn, ""),
			nodes: []*corev1.Node{zonedNode("node-a", "us-east-1a"), zonedNode("node-b", "us-east-1b")},
			endpoints: []backend.PodEndpoint{
				endpoint("172.16.0.1", "node-a"),
				endpoint("172.16.0.2", "node-b"),
			},
			want: []elbv2types.TargetDescription{
				{Id: awssdk.String("172.16.0.1"), Port: awssdk.Int32(6432), AvailabilityZone: awssdk.String("us-east-1a")},
				{Id: awssdk.String("172.16.0.2"), Port: awssdk.Int32(6432), AvailabilityZone: awssdk.String("us-east-1b")},
			},
		},
		{
			name:      "in-VPC targets are left for AWS to detect",
			tgb:       tgbWith(optIn, ""),
			nodes:     []*corev1.Node{zonedNode("node-a", "us-east-1a")},
			endpoints: []backend.PodEndpoint{endpoint("10.0.0.1", "node-a")},
			want:      []elbv2types.TargetDescription{{Id: awssdk.String("10.0.0.1"), Port: awssdk.Int32(6432)}},
		},
		{
			name:      "annotation is ignored for cross-account target groups",
			tgb:       tgbWith(optIn, "arn:aws:iam::123456789012:role/other"),
			nodes:     []*corev1.Node{zonedNode("node-a", "us-east-1a")},
			endpoints: []backend.PodEndpoint{endpoint("172.16.0.1", "node-a")},
			want:      []elbv2types.TargetDescription{{Id: awssdk.String("172.16.0.1"), Port: awssdk.Int32(6432), AvailabilityZone: awssdk.String("all")}},
		},
		{
			name:      "missing node is an error, not a fallback to all",
			tgb:       tgbWith(optIn, ""),
			endpoints: []backend.PodEndpoint{endpoint("172.16.0.1", "node-a")},
			wantErr:   true,
		},
		{
			name:      "node without a zone label is an error",
			tgb:       tgbWith(optIn, ""),
			nodes:     []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}},
			endpoints: []backend.PodEndpoint{endpoint("172.16.0.1", "node-a")},
			wantErr:   true,
		},
		{
			name:      "unscheduled pod is an error",
			tgb:       tgbWith(optIn, ""),
			endpoints: []backend.PodEndpoint{endpoint("172.16.0.1", "")},
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := testclient.NewClientBuilder()
			for _, node := range tt.nodes {
				builder = builder.WithObjects(node)
			}
			m := &defaultResourceManager{k8sClient: builder.Build(), logger: logr.Discard()}

			got, err := m.prepareRegistrationCall(context.Background(), tt.endpoints, tt.tgb, outOfVPC)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
