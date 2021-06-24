// +build !agent
// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xds

import (
	"context"
	"net"
	"strings"
	"time"

	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/pkg/config/kube/ingress"
	"istio.io/istio/pilot/pkg/controller/workloadentry"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/core/v1alpha3"
	"istio.io/istio/pilot/pkg/networking/plugin"
	kubesecrets "istio.io/istio/pilot/pkg/secrets/kube"
	"istio.io/istio/pilot/pkg/serviceregistry"
	kube "istio.io/istio/pilot/pkg/serviceregistry/kube/controller"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	"istio.io/istio/pilot/test/xdstest"
	"istio.io/istio/pkg/adsc"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/keepalive"
	kubelib "istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/retry"
)

type FakeOptions struct {
	// If provided, a service registry with the name of each map key will be created with the given objects.
	KubernetesObjectsByCluster map[cluster.ID][]runtime.Object
	// If provided, these objects will be used directly for the default cluster ("Kubernetes")
	KubernetesObjects []runtime.Object
	// If provided, the yaml string will be parsed and used as objects for the default cluster ("Kubernetes")
	KubernetesObjectString string
	// Endpoint mode for the Kubernetes service registry
	KubernetesEndpointMode kube.EndpointMode
	// If provided, these configs will be used directly
	Configs []config.Config
	// If provided, the yaml string will be parsed and used as configs
	ConfigString string
	// If provided, the ConfigString will be treated as a go template, with this as input params
	ConfigTemplateInput interface{}
	// If provided, this mesh config will be used
	MeshConfig      *meshconfig.MeshConfig
	NetworksWatcher mesh.NetworksWatcher

	// Callback to modify the server before it is started
	DiscoveryServerModifier func(s *DiscoveryServer)

	// Time to debounce
	// By default, set to 0s to speed up tests
	DebounceTime time.Duration
}

type FakeDiscoveryServer struct {
	*v1alpha3.ConfigGenTest
	t            test.Failer
	Discovery    *DiscoveryServer
	Listener     *bufconn.Listener
	kubeClient   kubelib.Client
	KubeRegistry *kube.FakeController
}

func NewFakeDiscoveryServer(t test.Failer, opts FakeOptions) *FakeDiscoveryServer {
	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
	})

	m := opts.MeshConfig
	if m == nil {
		def := mesh.DefaultMeshConfig()
		m = &def
	}

	// Init with a dummy environment, since we have a circular dependency with the env creation.
	s := NewDiscoveryServer(&model.Environment{PushContext: model.NewPushContext()}, []string{plugin.AuthzCustom, plugin.Authn, plugin.Authz},
		"pilot-123", "istio-system")
	t.Cleanup(func() {
		s.JwtKeyResolver.Close()
		s.pushQueue.ShutDown()
	})

	serviceHandler := func(svc *model.Service, _ model.Event) {
		pushReq := &model.PushRequest{
			Full: true,
			ConfigsUpdated: map[model.ConfigKey]struct{}{{
				Kind:      gvk.ServiceEntry,
				Name:      string(svc.Hostname),
				Namespace: svc.Attributes.Namespace,
			}: {}},
			Reason: []model.TriggerReason{model.ServiceUpdate},
		}
		s.ConfigUpdate(pushReq)
	}

	k8sObjects := getKubernetesObjects(t, opts)
	var defaultKubeClient kubelib.Client
	var defaultKubeController *kube.FakeController
	var registries []serviceregistry.Instance
	if opts.NetworksWatcher != nil {
		opts.NetworksWatcher.AddNetworksHandler(func() {
			s.ConfigUpdate(&model.PushRequest{
				Full:   true,
				Reason: []model.TriggerReason{model.NetworksTrigger},
			})
		})
	}
	for k8sCluster, objs := range k8sObjects {
		client := kubelib.NewFakeClient(objs...)
		k8s, _ := kube.NewFakeControllerWithOptions(kube.FakeControllerOptions{
			ServiceHandler:  serviceHandler,
			Client:          client,
			ClusterID:       k8sCluster,
			DomainSuffix:    "cluster.local",
			XDSUpdater:      s,
			NetworksWatcher: opts.NetworksWatcher,
			Mode:            opts.KubernetesEndpointMode,
			// we wait for the aggregate to sync
			SkipCacheSyncWait: true,
			Stop:              stop,
		})
		// start default client informers after creating ingress/secret controllers
		if defaultKubeClient == nil || k8sCluster == "Kubernetes" {
			defaultKubeClient = client
			defaultKubeController = k8s
		} else {
			client.RunAndWait(stop)
		}
		registries = append(registries, k8s)
	}

	sc := kubesecrets.NewMulticluster(defaultKubeClient, "", "", stop)
	s.Generators[v3.SecretType] = NewSecretGen(sc, &model.DisabledCache{})
	defaultKubeClient.RunAndWait(stop)

	ingr := ingress.NewController(defaultKubeClient, mesh.NewFixedWatcher(m), kube.Options{
		DomainSuffix: "cluster.local",
	})
	defaultKubeClient.RunAndWait(stop)

	cg := v1alpha3.NewConfigGenTest(t, v1alpha3.TestOptions{
		Configs:             opts.Configs,
		ConfigString:        opts.ConfigString,
		ConfigTemplateInput: opts.ConfigTemplateInput,
		MeshConfig:          opts.MeshConfig,
		NetworksWatcher:     opts.NetworksWatcher,
		ServiceRegistries:   registries,
		PushContextLock:     &s.updateMutex,
		ConfigStoreCaches:   []model.ConfigStoreCache{ingr},
		SkipRun:             true,
	})
	cg.ServiceEntryRegistry.AppendServiceHandler(serviceHandler)
	s.updateMutex.Lock()
	s.Env = cg.Env()
	// Disable debounce to reduce test times
	s.debounceOptions.debounceAfter = opts.DebounceTime
	s.MemRegistry = cg.MemRegistry
	s.MemRegistry.EDSUpdater = s
	s.updateMutex.Unlock()

	// Setup config handlers
	// TODO code re-use from server.go
	configHandler := func(_, curr config.Config, event model.Event) {
		pushReq := &model.PushRequest{
			Full: true,
			ConfigsUpdated: map[model.ConfigKey]struct{}{{
				Kind:      curr.GroupVersionKind,
				Name:      curr.Name,
				Namespace: curr.Namespace,
			}: {}},
			Reason: []model.TriggerReason{model.ConfigUpdate},
		}
		s.ConfigUpdate(pushReq)
	}
	schemas := collections.Pilot.All()
	if features.EnableServiceApis {
		schemas = collections.PilotServiceApi.All()
	}
	for _, schema := range schemas {
		// This resource type was handled in external/servicediscovery.go, no need to rehandle here.
		if schema.Resource().GroupVersionKind() == collections.IstioNetworkingV1Alpha3Serviceentries.
			Resource().GroupVersionKind() {
			continue
		}
		if schema.Resource().GroupVersionKind() == collections.IstioNetworkingV1Alpha3Workloadentries.
			Resource().GroupVersionKind() {
			continue
		}

		cg.Store().RegisterEventHandler(schema.Resource().GroupVersionKind(), configHandler)
	}
	for _, registry := range registries {
		k8s, ok := registry.(*kube.FakeController)
		if !ok {
			continue
		}
		cg.ServiceEntryRegistry.AppendWorkloadHandler(k8s.WorkloadInstanceHandler)
		k8s.AppendWorkloadHandler(cg.ServiceEntryRegistry.WorkloadInstanceHandler)
	}
	s.WorkloadEntryController = workloadentry.NewController(cg.Store(), "test", keepalive.Infinity)

	if opts.DiscoveryServerModifier != nil {
		opts.DiscoveryServerModifier(s)
	}

	// Start in memory gRPC listener
	buffer := 1024 * 1024
	listener := bufconn.Listen(buffer)
	grpcServer := grpc.NewServer()
	s.Register(grpcServer)
	go func() {
		if err := grpcServer.Serve(listener); err != nil && !(err == grpc.ErrServerStopped || err.Error() == "closed") {
			t.Fatal(err)
		}
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
	})
	// Start the discovery server
	s.Start(stop)
	cg.ServiceEntryRegistry.XdsUpdater = s
	cache.WaitForCacheSync(stop,
		cg.Registry.HasSynced,
		cg.Store().HasSynced)
	cg.ServiceEntryRegistry.ResyncEDS()

	// Send an update. This ensures that even if there are no configs provided, the push context is
	// initialized.
	s.ConfigUpdate(&model.PushRequest{Full: true})

	// Now that handlers are added, get everything started
	cg.Run()

	// Wait until initial updates are committed
	c := s.InboundUpdates.Load()
	retry.UntilOrFail(t, func() bool {
		return s.CommittedUpdates.Load() >= c
	}, retry.Delay(time.Millisecond))

	// Mark ourselves ready
	s.CachesSynced()

	fake := &FakeDiscoveryServer{
		t:             t,
		Discovery:     s,
		Listener:      listener,
		ConfigGenTest: cg,
		kubeClient:    defaultKubeClient,
		KubeRegistry:  defaultKubeController,
	}

	return fake
}

func (f *FakeDiscoveryServer) KubeClient() kubelib.Client {
	return f.kubeClient
}

func (f *FakeDiscoveryServer) PushContext() *model.PushContext {
	f.Discovery.updateMutex.RLock()
	defer f.Discovery.updateMutex.RUnlock()
	return f.Env().PushContext
}

// ConnectADS starts an ADS connection to the server. It will automatically be cleaned up when the test ends
func (f *FakeDiscoveryServer) ConnectADS() *AdsTest {
	conn, err := grpc.Dial("buffcon", grpc.WithInsecure(), grpc.WithBlock(), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return f.Listener.Dial()
	}))
	if err != nil {
		f.t.Fatalf("failed to connect: %v", err)
	}
	return NewAdsTest(f.t, conn)
}

// ConnectDeltaADS starts a Delta ADS connection to the server. It will automatically be cleaned up when the test ends
func (f *FakeDiscoveryServer) ConnectDeltaADS() *DeltaAdsTest {
	conn, err := grpc.Dial("buffcon", grpc.WithInsecure(), grpc.WithBlock(), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return f.Listener.Dial()
	}))
	if err != nil {
		f.t.Fatalf("failed to connect: %v", err)
	}
	return NewDeltaAdsTest(f.t, conn)
}

// Connect starts an ADS connection to the server using adsc. It will automatically be cleaned up when the test ends
// watch can be configured to determine the resources to watch initially, and wait can be configured to determine what
// resources we should initially wait for.
func (f *FakeDiscoveryServer) Connect(p *model.Proxy, watch []string, wait []string) *adsc.ADSC {
	f.t.Helper()
	p = f.SetupProxy(p)
	initialWatch := []*discovery.DiscoveryRequest{}
	if watch == nil {
		initialWatch = []*discovery.DiscoveryRequest{{TypeUrl: v3.ClusterType}}
	} else {
		for _, typeURL := range watch {
			initialWatch = append(initialWatch, &discovery.DiscoveryRequest{TypeUrl: typeURL})
		}
	}
	if wait == nil {
		initialWatch = []*discovery.DiscoveryRequest{{TypeUrl: v3.ClusterType}}
	}
	adscConn, err := adsc.New("buffcon", &adsc.Config{
		IP:                       p.IPAddresses[0],
		NodeType:                 string(p.Type),
		Meta:                     p.Metadata.ToStruct(),
		Locality:                 p.Locality,
		Namespace:                p.ConfigNamespace,
		InitialDiscoveryRequests: initialWatch,
		GrpcOpts: []grpc.DialOption{
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
				return f.Listener.Dial()
			}),
			grpc.WithInsecure(),
		},
	})
	if err != nil {
		f.t.Fatalf("Error connecting: %v", err)
	}
	if err := adscConn.Run(); err != nil {
		f.t.Fatalf("ADSC: failed running: %v", err)
	}

	if len(wait) > 0 {
		_, err = adscConn.Wait(10*time.Second, wait...)
		if err != nil {
			f.t.Fatalf("Error getting initial for %v config: %v", wait, err)
		}
	}
	f.t.Cleanup(func() {
		adscConn.Close()
	})
	return adscConn
}

func (f *FakeDiscoveryServer) Endpoints(p *model.Proxy) []*endpoint.ClusterLoadAssignment {
	loadAssignments := make([]*endpoint.ClusterLoadAssignment, 0)
	for _, c := range xdstest.ExtractEdsClusterNames(f.Clusters(p)) {
		loadAssignments = append(loadAssignments, f.Discovery.generateEndpoints(NewEndpointBuilder(c, p, f.PushContext())))
	}
	return loadAssignments
}

func getKubernetesObjects(t test.Failer, opts FakeOptions) map[cluster.ID][]runtime.Object {
	objects := map[cluster.ID][]runtime.Object{}

	if len(opts.KubernetesObjects) > 0 {
		objects["Kuberentes"] = append(objects["Kuberenetes"], opts.KubernetesObjects...)
	}
	if len(opts.KubernetesObjectString) > 0 {
		decode := scheme.Codecs.UniversalDeserializer().Decode
		objectStrs := strings.Split(opts.KubernetesObjectString, "---")
		for _, s := range objectStrs {
			if len(strings.TrimSpace(s)) == 0 {
				continue
			}
			o, _, err := decode([]byte(s), nil, nil)
			if err != nil {
				t.Fatalf("failed deserializing kubernetes object: %v", err)
			}
			objects["Kubernetes"] = append(objects["Kubernetes"], o)
		}
	}

	for k8sCluster, clusterObjs := range opts.KubernetesObjectsByCluster {
		objects[k8sCluster] = append(objects[k8sCluster], clusterObjs...)
	}

	if len(objects) == 0 {
		return map[cluster.ID][]runtime.Object{"Kubernetes": {}}
	}

	return objects
}
