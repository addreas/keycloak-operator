package model

import (
	"testing"

	"github.com/addreas/keycloak-operator/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
)

func TestDatabaseSecret_test_nil_map(t *testing.T) {
	// given
	cr := &v1alpha1.Keycloak{}
	secret := &v1.Secret{}

	// when
	reconciledSecret := DatabaseSecretReconciled(cr, secret)

	//then
	assert.Equal(t, string(reconciledSecret.Data[DatabaseSecretUsernameProperty]), PostgresqlUsername)
	assert.True(t, len(string(reconciledSecret.Data[DatabaseSecretPasswordProperty])) > 0)
	assert.Equal(t, string(reconciledSecret.Data[DatabaseSecretDatabaseProperty]), PostgresqlDatabase)
	assert.Equal(t, string(reconciledSecret.Data[DatabaseSecretHostProperty]), PostgresqlServiceName)
	assert.Equal(t, string(reconciledSecret.Data[DatabaseSecretVersionProperty]), "10")
}
