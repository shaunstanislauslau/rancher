package cluster

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	errorsutil "github.com/pkg/errors"
	"github.com/rancher/kontainer-engine/service"
	"github.com/rancher/kontainer-engine/types"
	"github.com/rancher/rancher/pkg/controllers/management/clusterprovisioner"
	"github.com/rancher/rke/cloudprovider/aws"
	"github.com/rancher/rke/cloudprovider/azure"
	v1 "github.com/rancher/types/apis/core/v1"
	v3 "github.com/rancher/types/apis/management.cattle.io/v3"
	"github.com/rancher/types/config"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	GoogleCloudLoadBalancer = "GCLB"
	ElasticLoadBalancer     = "ELB"
	AzureL4LB               = "Azure L4 LB"
	NginxIngressProvider    = "Nginx"
	DefaultNodePortRange    = "30000-32767"
)

type controller struct {
	clusterClient         v3.ClusterInterface
	nodeLister            v3.NodeLister
	kontainerDriverLister v3.KontainerDriverLister
	namespaces            v1.NamespaceInterface
	coreV1                v1.Interface
}

func Register(ctx context.Context, management *config.ManagementContext) {
	c := controller{
		clusterClient:         management.Management.Clusters(""),
		nodeLister:            management.Management.Nodes("").Controller().Lister(),
		kontainerDriverLister: management.Management.KontainerDrivers("").Controller().Lister(),
		namespaces:            management.Core.Namespaces(""),
		coreV1:                management.Core,
	}

	c.clusterClient.AddHandler(ctx, "clusterCreateUpdate", c.capsSync)
}

func (c *controller) capsSync(key string, cluster *v3.Cluster) (runtime.Object, error) {
	var err error
	if cluster == nil || cluster.DeletionTimestamp != nil {
		return nil, nil
	}

	if cluster.Spec.ImportedConfig != nil {
		return nil, nil
	}
	capabilities := v3.Capabilities{}
	capabilities.NodePortRange = DefaultNodePortRange

	if cluster.Spec.RancherKubernetesEngineConfig != nil {
		// taint support capability is set in provisioner and update cluster is called, so we should retain the capability here
		if cluster.Status.Capabilities.TaintSupport != nil && *cluster.Status.Capabilities.TaintSupport {
			supportsTaints := true
			capabilities.TaintSupport = &supportsTaints
		}
		if capabilities, err = c.RKECapabilities(capabilities, *cluster.Spec.RancherKubernetesEngineConfig, cluster.Name); err != nil {
			return nil, err
		}
	} else if cluster.Spec.GenericEngineConfig != nil {
		driverName, ok := (*cluster.Spec.GenericEngineConfig)["driverName"].(string)
		if !ok {
			logrus.Warnf("cluster %v had generic engine config but no driver name, k8s capabilities will "+
				"not be populated correctly", key)
			return nil, nil
		}

		kontainerDriver, err := c.kontainerDriverLister.Get("", driverName)
		if err != nil {
			if !errors.IsNotFound(err) {
				return nil, errorsutil.WithMessage(err, fmt.Sprintf("error getting kontainer driver: %v", driverName))
			}
			//do not return not found errors since the driver may have been deleted
			return nil, nil
		}

		driver := service.NewEngineService(
			clusterprovisioner.NewPersistentStore(c.namespaces, c.coreV1),
		)
		k8sCapabilities, err := driver.GetK8sCapabilities(context.Background(), kontainerDriver.Name, kontainerDriver,
			cluster.Spec)
		if err != nil {
			return nil, fmt.Errorf("error getting k8s capabilities: %v", err)
		}

		capabilities = toCapabilities(k8sCapabilities)
	} else {
		return nil, nil
	}

	if !reflect.DeepEqual(capabilities, cluster.Status.Capabilities) {
		toUpdateCluster := cluster.DeepCopy()
		toUpdateCluster.Status.Capabilities = capabilities
		if _, err := c.clusterClient.Update(toUpdateCluster); err != nil {
			return nil, err
		}
	}

	return nil, nil
}

func (c *controller) RKECapabilities(capabilities v3.Capabilities, rkeConfig v3.RancherKubernetesEngineConfig, clusterName string) (v3.Capabilities, error) {
	switch rkeConfig.CloudProvider.Name {
	case aws.AWSCloudProviderName:
		capabilities.LoadBalancerCapabilities = c.L4Capability(true, ElasticLoadBalancer, []string{"TCP"}, true)
	case azure.AzureCloudProviderName:
		capabilities.LoadBalancerCapabilities = c.L4Capability(true, AzureL4LB, []string{"TCP", "UDP"}, true)
	}
	// only if not custom, non custom clusters have nodepools set
	nodes, err := c.nodeLister.List(clusterName, labels.Everything())
	if err != nil {
		return capabilities, err
	}

	if len(nodes) > 0 {
		if nodes[0].Spec.NodePoolName != "" {
			capabilities.NodePoolScalingSupported = true
		}
	}

	ingressController := c.IngressCapability(true, rkeConfig.Ingress.Provider)
	capabilities.IngressCapabilities = []v3.IngressCapabilities{ingressController}
	if rkeConfig.Services.KubeAPI.ServiceNodePortRange != "" {
		capabilities.NodePortRange = rkeConfig.Services.KubeAPI.ServiceNodePortRange
	} else if rkeConfig.Services.KubeAPI.ExtraArgs["service-node-port-range"] != "" {
		capabilities.NodePortRange = rkeConfig.Services.KubeAPI.ExtraArgs["service-node-port-range"]
	}

	return capabilities, nil
}

func (c *controller) L4Capability(enabled bool, providerName string, protocols []string, healthCheck bool) v3.LoadBalancerCapabilities {
	l4lb := v3.LoadBalancerCapabilities{
		Enabled:              &enabled,
		Provider:             providerName,
		ProtocolsSupported:   protocols,
		HealthCheckSupported: healthCheck,
	}
	return l4lb
}

func (c *controller) IngressCapability(httpLBEnabled bool, providerName string) v3.IngressCapabilities {
	customDefaultBackendDisabled := false
	ing := v3.IngressCapabilities{
		IngressProvider: providerName,
	}
	if strings.EqualFold(providerName, NginxIngressProvider) {
		ing.CustomDefaultBackend = &customDefaultBackendDisabled
	}
	return ing
}

func toCapabilities(k8sCapabilities *types.K8SCapabilities) v3.Capabilities {
	var controllers []v3.IngressCapabilities

	for _, controller := range k8sCapabilities.IngressControllers {
		controllers = append(controllers, v3.IngressCapabilities{
			CustomDefaultBackend: &controller.CustomDefaultBackend,
			IngressProvider:      controller.IngressProvider,
		})
	}

	return v3.Capabilities{
		IngressCapabilities: controllers,
		LoadBalancerCapabilities: v3.LoadBalancerCapabilities{
			Enabled:              &k8sCapabilities.L4LoadBalancer.Enabled,
			HealthCheckSupported: k8sCapabilities.L4LoadBalancer.HealthCheckSupported,
			ProtocolsSupported:   k8sCapabilities.L4LoadBalancer.ProtocolsSupported,
			Provider:             k8sCapabilities.L4LoadBalancer.Provider,
		},
		NodePoolScalingSupported: k8sCapabilities.NodePoolScalingSupported,
		NodePortRange:            k8sCapabilities.NodePortRange,
	}
}
