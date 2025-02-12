package k8s

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/encoding/protojson"
	v1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	acsappsv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	acsv1 "k8s.io/client-go/applyconfigurations/core/v1"
	acsmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	acsnetv1 "k8s.io/client-go/applyconfigurations/networking/v1"

	"github.com/rigdev/rig-go-api/api/v1/capsule"
	"github.com/rigdev/rig/gen/go/proxy"
	"github.com/rigdev/rig/internal/build"
	"github.com/rigdev/rig/internal/gateway/cluster"
	"github.com/rigdev/rig/pkg/auth"
)

// UpsertCapsule implements cluster.Gateway.
func (c *Client) UpsertCapsule(ctx context.Context, capsuleName string, cc *cluster.Capsule) error {
	projectID, err := auth.GetProjectID(ctx)
	if err != nil {
		return err
	}
	ns := projectID.String()

	if err := c.reconcileProjectNamespace(ctx, ns); err != nil {
		return err
	}

	bs, err := c.ps.GetProjectDockerSecret(ctx)
	if err != nil {
		return err
	}
	if err := c.reconcilePullSecret(ctx, ns, bs); err != nil {
		return err
	}

	if err := c.reconcileProxyEnvSecret(ctx, capsuleName, ns, cc); err != nil {
		return err
	}
	if err := c.reconcileLoadBalancer(ctx, capsuleName, ns, cc); err != nil {
		return err
	}
	if err := c.reconcileIngress(ctx, capsuleName, ns, cc); err != nil {
		return err
	}
	if err := c.reconcileService(ctx, capsuleName, ns, cc); err != nil {
		return err
	}
	if err := c.reconcileEnvSecret(ctx, capsuleName, ns, cc); err != nil {
		return err
	}
	if err := c.reconcileDeployment(ctx, capsuleName, ns, len(bs) > 0, cc); err != nil {
		return err
	}

	return nil
}

func (c *Client) reconcileProjectNamespace(ctx context.Context, namespace string) error {
	ns := acsv1.Namespace(namespace)

	_, err := c.cs.CoreV1().Namespaces().Apply(ctx, ns, applyOpts())
	if err != nil {
		return fmt.Errorf("could not apply Namespace: %w", err)
	}
	return nil
}

func (c *Client) reconcilePullSecret(ctx context.Context, namespace string, data []byte) error {
	if len(data) == 0 {
		if err := c.deletePullSecret(ctx, namespace); err != nil {
			return err
		}
		return nil
	}

	s := acsv1.Secret(fmt.Sprintf("%s-pull", namespace), namespace).
		WithType(v1.SecretTypeDockerConfigJson).
		WithData(map[string][]byte{".dockerconfigjson": data})
	_, err := c.cs.CoreV1().Secrets(namespace).Apply(ctx, s, applyOpts())
	if err != nil {
		return fmt.Errorf("could not apply pull secret: %w", err)
	}
	return nil
}

func hashSecretData(data map[string]string) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%+v", data)))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func createProxyConfig(ctx context.Context, cc *cluster.Capsule) (map[string]string, error) {
	cfg, err := cluster.CreateProxyConfig(ctx, cc.Network, cc.JWTMethod)
	if err != nil {
		return nil, err
	}

	cfg.TargetHost = "localhost"

	pps, err := createProxyPorts(cc.Network.GetInterfaces())
	if err != nil {
		return nil, err
	}

	for i, inf := range cfg.GetInterfaces() {
		inf.SourcePort = pps[i]
	}

	bs, err := protojson.Marshal(cfg)
	if err != nil {
		return nil, err
	}

	return map[string]string{"RIG_PROXY_CONFIG": strconv.QuoteToASCII(string(bs))}, nil
}

func (c *Client) reconcileProxyEnvSecret(
	ctx context.Context,
	capsuleName,
	namespace string,
	cc *cluster.Capsule,
) error {
	if !hasInterfaces(cc) {
		return c.deleteProxyEnvSecret(ctx, capsuleName, namespace)
	}

	cfg, err := createProxyConfig(ctx, cc)
	if err != nil {
		return err
	}

	s := acsv1.Secret(fmt.Sprintf("%s-proxy", capsuleName), namespace).
		WithLabels(commonLabels(capsuleName, cc)).
		WithStringData(cfg)

	_, err = c.cs.CoreV1().
		Secrets(namespace).
		Apply(ctx, s, applyOpts())
	if err != nil {
		return fmt.Errorf("could not apply proxy ConfigMap: %w", err)
	}
	return nil
}

func (c *Client) reconcileLoadBalancer(ctx context.Context, capsuleName, namespace string, cc *cluster.Capsule) error {
	if !hasLoadBalancer(cc) {
		return c.deleteLoadBalancer(ctx, capsuleName, namespace)
	}

	var ports []*acsv1.ServicePortApplyConfiguration
	for _, inf := range cc.Network.GetInterfaces() {
		pub := inf.GetPublic()
		if pub.GetEnabled() {
			switch m := pub.GetMethod().GetKind().(type) {
			case *capsule.RoutingMethod_LoadBalancer_:
				ports = append(ports, acsv1.ServicePort().
					WithName(inf.GetName()).
					WithPort(int32(m.LoadBalancer.GetPort())).
					WithTargetPort(intstr.FromString(inf.GetName())),
				)
			}
		}
	}

	s := acsv1.Service(fmt.Sprintf("%s-lb", capsuleName), namespace).
		WithLabels(commonLabels(capsuleName, cc)).
		WithSpec(acsv1.ServiceSpec().
			WithSelector(selectorLabels(capsuleName)).
			WithPorts(ports...).
			WithType(v1.ServiceTypeLoadBalancer),
		)

	_, err := c.cs.CoreV1().Services(namespace).Apply(ctx, s, applyOpts())
	if err != nil {
		return fmt.Errorf("could not apply Service: %w", err)
	}

	return nil
}

func (c *Client) reconcileIngress(ctx context.Context, capsuleName, namespace string, cc *cluster.Capsule) error {
	if !hasIngress(cc) {
		return c.deleteIngress(ctx, capsuleName, namespace)
	}

	var rules []*acsnetv1.IngressRuleApplyConfiguration
	for _, inf := range cc.Network.GetInterfaces() {
		pub := inf.GetPublic()
		if pub.GetEnabled() {
			switch m := pub.GetMethod().GetKind().(type) {
			case *capsule.RoutingMethod_Ingress_:
				rules = append(rules, acsnetv1.IngressRule().
					WithHost(m.Ingress.GetHost()).
					WithHTTP(acsnetv1.HTTPIngressRuleValue().
						WithPaths(acsnetv1.HTTPIngressPath().
							WithPathType(netv1.PathTypePrefix).
							WithPath("/").
							WithBackend(acsnetv1.IngressBackend().
								WithService(acsnetv1.IngressServiceBackend().
									WithName(capsuleName).
									WithPort(acsnetv1.ServiceBackendPort().
										WithName(inf.GetName()),
									),
								),
							),
						),
					),
				)
			}
		}
	}

	ing := acsnetv1.Ingress(capsuleName, namespace).
		WithLabels(commonLabels(capsuleName, cc)).
		WithSpec(acsnetv1.IngressSpec().
			WithRules(rules...),
		)

	_, err := c.cs.NetworkingV1().Ingresses(namespace).Apply(ctx, ing, applyOpts())
	if err != nil {
		return fmt.Errorf("could not apply Ingress: %w", err)
	}
	return nil
}

func (c *Client) reconcileService(ctx context.Context, capsuleName, namespace string, cc *cluster.Capsule) error {
	if !hasInterfaces(cc) {
		return c.deleteService(ctx, capsuleName, namespace)
	}

	infs := cc.Network.GetInterfaces()
	ports := make([]*acsv1.ServicePortApplyConfiguration, len(infs))

	for i, inf := range infs {
		ports[i] = acsv1.ServicePort().
			WithName(inf.GetName()).
			WithPort(int32(inf.GetPort())).
			WithTargetPort(intstr.FromString(inf.GetName()))
	}

	s := acsv1.Service(capsuleName, namespace).
		WithLabels(commonLabels(capsuleName, cc)).
		WithSpec(acsv1.ServiceSpec().
			WithSelector(selectorLabels(capsuleName)).
			WithPorts(ports...).
			WithType(v1.ServiceTypeClusterIP),
		)

	_, err := c.cs.CoreV1().Services(namespace).Apply(ctx, s, applyOpts())
	if err != nil {
		return fmt.Errorf("could not apply Service: %w", err)
	}
	return nil
}

func (c *Client) reconcileEnvSecret(ctx context.Context, capsuleName, namespace string, cc *cluster.Capsule) error {
	if len(cc.ContainerSettings.GetEnvironmentVariables()) == 0 {
		return c.deleteEnvSecret(ctx, capsuleName, namespace)
	}

	s := acsv1.Secret(capsuleName, namespace).
		WithLabels(commonLabels(capsuleName, cc)).
		WithStringData(cc.ContainerSettings.GetEnvironmentVariables())

	_, err := c.cs.CoreV1().
		Secrets(namespace).
		Apply(ctx, s, applyOpts())
	if err != nil {
		return fmt.Errorf("could not apply Secret: %w", err)
	}
	return nil
}

func (c *Client) reconcileDeployment(ctx context.Context, capsuleName, namespace string, usePullSecret bool, cc *cluster.Capsule) error {
	cons := []*acsv1.ContainerApplyConfiguration{
		createContainer(capsuleName, cc),
	}

	if hasInterfaces(cc) {
		con, err := createProxyContainer(capsuleName, cc)
		if err != nil {
			return err
		}
		cons = append(cons, con)
	}

	d := acsappsv1.Deployment(capsuleName, namespace).
		WithLabels(commonLabels(capsuleName, cc)).
		WithSpec(acsappsv1.DeploymentSpec().
			WithReplicas(int32(cc.Replicas)).
			WithSelector(acsmetav1.LabelSelector().
				WithMatchLabels(selectorLabels(capsuleName)),
			).
			WithTemplate(acsv1.PodTemplateSpec().
				WithLabels(commonLabels(capsuleName, cc)).
				WithSpec(acsv1.PodSpec().
					WithContainers(cons...),
				),
			),
		)

	if usePullSecret {
		d.Spec.Template.Spec.WithImagePullSecrets(acsv1.LocalObjectReference().
			WithName(fmt.Sprintf("%s-pull", namespace)),
		)
	}

	if hasInterfaces(cc) {
		cfg, err := createProxyConfig(ctx, cc)
		if err != nil {
			return err
		}

		h := hashSecretData(cfg)

		d.Spec.Template.WithAnnotations(map[string]string{
			"rig.dev/proxy-config-sha": h,
		})
	}

	if len(cc.ContainerSettings.GetEnvironmentVariables()) > 0 {
		h := hashSecretData(cc.ContainerSettings.GetEnvironmentVariables())
		d.Spec.Template.WithAnnotations(map[string]string{
			"rig.dev/config-sha": h,
		})
	}

	_, err := c.cs.AppsV1().
		Deployments(namespace).
		Apply(ctx, d, applyOpts())
	if err != nil {
		return fmt.Errorf("could not apply Deployment: %w", err)
	}
	return nil
}

func createContainer(capsuleName string, cc *cluster.Capsule) *acsv1.ContainerApplyConfiguration {
	rl := v1.ResourceList{
		v1.ResourceCPU:              resource.MustParse("500m"),
		v1.ResourceEphemeralStorage: resource.MustParse("512Mi"),
		v1.ResourceMemory:           resource.MustParse("512Mi"),
	}

	con := acsv1.Container().
		WithName(capsuleName).
		WithImage(cc.Image).
		WithArgs(cc.ContainerSettings.GetArgs()...).
		WithResources(
			acsv1.ResourceRequirements().
				WithRequests(rl).
				WithLimits(rl),
		).
		// TODO(anders): Get from configuration.
		WithEnv(acsv1.EnvVar().WithName("RIG_HOST").WithValue("http://rig.rig-system.svc.cluster.local:4747"))

	if cc.ContainerSettings.GetCommand() != "" {
		con.WithCommand(cc.ContainerSettings.GetCommand())
	}

	if hasEnvSecret(cc) {
		con.WithEnvFrom(acsv1.EnvFromSource().
			WithSecretRef(acsv1.SecretEnvSource().
				WithName(capsuleName),
			),
		)
	}

	infs := cc.Network.GetInterfaces()
	ports := make([]*acsv1.ContainerPortApplyConfiguration, len(infs))
	for i, inf := range infs {
		port := inf.GetPort()
		ports[i] = acsv1.ContainerPort().
			WithName(fmt.Sprintf("port-%d", port)).
			WithContainerPort(int32(port))
	}
	con.WithPorts(ports...)

	return con
}

const proxyContainerName = "rig-proxy"

func createProxyContainer(capsuleName string, cc *cluster.Capsule) (*acsv1.ContainerApplyConfiguration, error) {
	rl := v1.ResourceList{
		v1.ResourceCPU: resource.MustParse("500m"),
		// TODO: validate that this limit is okay with regards to mounting configmaps and secrets as files.
		v1.ResourceEphemeralStorage: resource.MustParse("0"),
		v1.ResourceMemory:           resource.MustParse("128Mi"),
	}

	con := acsv1.Container().
		WithName(proxyContainerName).
		WithImage(fmt.Sprint("ghcr.io/rigdev/rig:", build.Version())).
		WithCommand("rig-proxy").
		WithEnvFrom(acsv1.EnvFromSource().
			WithSecretRef(acsv1.SecretEnvSource().
				WithName(fmt.Sprintf("%s-proxy", capsuleName)),
			),
		).
		WithResources(
			acsv1.ResourceRequirements().
				WithRequests(rl).
				WithLimits(rl),
		)

	infs := cc.Network.GetInterfaces()
	pps, err := createProxyPorts(infs)
	if err != nil {
		return nil, err
	}

	ports := make([]*acsv1.ContainerPortApplyConfiguration, len(infs))
	for i, inf := range infs {
		ports[i] = acsv1.ContainerPort().
			WithName(inf.GetName()).
			WithContainerPort(int32(pps[i]))
	}

	con.WithPorts(ports...)

	return con, nil
}

func applyOpts() metav1.ApplyOptions {
	return metav1.ApplyOptions{
		FieldManager: "rig",
		Force:        true,
	}
}

func hasEnvSecret(cc *cluster.Capsule) bool {
	return len(cc.ContainerSettings.GetEnvironmentVariables()) > 0
}

func hasInterfaces(cc *cluster.Capsule) bool {
	if cc == nil {
		return false
	}
	if cc.Network == nil {
		return false
	}
	return len(cc.Network.GetInterfaces()) > 0
}

func getLBInterfaces(interfaces []*proxy.Interface) []*proxy.Interface {
	var infs []*proxy.Interface

	for _, inf := range interfaces {
		if inf.GetLayer() == proxy.Layer_LAYER_4 {
			infs = append(infs, inf)
		}
	}

	return infs
}

func getIngInterfaces(interfaces []*proxy.Interface) []*proxy.Interface {
	var infs []*proxy.Interface

	for _, inf := range interfaces {
		if inf.GetLayer() == proxy.Layer_LAYER_7 {
			infs = append(infs, inf)
		}
	}

	return infs
}

func hasLoadBalancer(cc *cluster.Capsule) bool {
	if !hasInterfaces(cc) {
		return false
	}

	for _, inf := range cc.Network.GetInterfaces() {
		pinf := inf.GetPublic()
		if pinf.GetEnabled() {
			switch pinf.GetMethod().GetKind().(type) {
			case *capsule.RoutingMethod_LoadBalancer_:
				return true
			}
		}
	}

	return false
}

func hasIngress(cc *cluster.Capsule) bool {
	if !hasInterfaces(cc) {
		return false
	}

	for _, inf := range cc.Network.GetInterfaces() {
		pinf := inf.GetPublic()
		if pinf.GetEnabled() {
			switch pinf.GetMethod().GetKind().(type) {
			case *capsule.RoutingMethod_Ingress_:
				return true
			}
		}
	}

	return false
}
