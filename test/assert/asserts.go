package assert

import (
	"reflect"
	"testing"

	"github.com/addreas/keycloak-operator/pkg/common"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func ReplicasCount(t *testing.T, state common.DesiredClusterState, expectedCount int32) {
	assert.Equal(t, &[]int32{expectedCount}[0], state[0].(common.GenericUpdateAction).Ref.(*v1.StatefulSet).Spec.Replicas)
}

func MatchingStateElem(t *testing.T, actions common.DesiredClusterState, matcher func(action common.ClusterAction) bool) common.ClusterAction {
	for _, elem := range actions {
		if matcher(elem) {
			return elem
		}
	}
	assert.Fail(t, "failed to match desired state elem")
	return nil
}

func FindGenericCreateAction(t *testing.T, desiredState common.DesiredClusterState, expected client.Object) common.GenericCreateAction {
	for _, action := range desiredState {
		action := action.(common.GenericCreateAction)
		ref := action.Ref

		if reflect.TypeOf(ref) == reflect.TypeOf(expected) && expected.GetName() == ref.GetName() && expected.GetNamespace() == ref.GetNamespace() {
			return action
		}
	}
	assert.Fail(t, "failed to match element in desired state", expected.GetNamespace(), expected.GetName(), expected.GetObjectKind())
	return common.GenericCreateAction{}
}

func FindGenericUpdateAction(t *testing.T, desiredState common.DesiredClusterState, expected client.Object) common.GenericUpdateAction {
	for _, action := range desiredState {
		action := action.(common.GenericUpdateAction)
		ref := action.Ref

		if reflect.TypeOf(ref) == reflect.TypeOf(expected) && expected.GetName() == ref.GetName() && expected.GetNamespace() == ref.GetNamespace() {
			return action
		}
	}
	assert.Fail(t, "failed to match element in desired state")
	return common.GenericUpdateAction{}
}

func Each(t *testing.T, items common.DesiredClusterState, tester func(t *testing.T, expected interface{}, actual interface{}), expected common.ClusterAction) {
	for _, elem := range items {
		tester(t, elem, expected)
	}
}
