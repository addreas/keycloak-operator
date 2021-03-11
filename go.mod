module github.com/addreas/keycloak-operator

go 1.15

require (
	cloud.google.com/go v0.56.0 // indirect
	github.com/go-logr/logr v0.4.0
	github.com/integr8ly/grafana-operator v1.4.1-0.20210305130532-56f3db9c9987
	github.com/json-iterator/go v1.1.10
	github.com/onsi/ginkgo v1.14.1
	github.com/onsi/gomega v1.10.2
	github.com/openshift/api v3.9.0+incompatible
	github.com/pkg/errors v0.9.1
	github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring v0.46.0
	github.com/sirupsen/logrus v1.6.0
	github.com/stretchr/testify v1.6.1
	k8s.io/api v0.20.4
	k8s.io/apimachinery v0.20.4
	k8s.io/client-go v12.0.0+incompatible
	k8s.io/utils v0.0.0-20210111153108-fddb29f9d009
	sigs.k8s.io/controller-runtime v0.8.3
)

replace (
	github.com/integr8ly/grafana-operator => github.com/HubertStefanski/grafana-operator v1.4.1-0.20210305130532-56f3db9c9987
	k8s.io/client-go => k8s.io/client-go v0.20.2
)
