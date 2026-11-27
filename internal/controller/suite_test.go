/*
Copyright 2026 Ksolves India Limited.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	nifiv1alpha1 "github.com/ksolves/nifi-operator/api/v1alpha1"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	Expect(nifiv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// The manager runs as a non-admin user bound only to the operator's generated ClusterRole
	// (config/rbac/role.yaml), not as envtest's system:masters admin. Every spec therefore also
	// proves that role is sufficient - including Kubernetes' RBAC escalation check, which only
	// lets the operator create the NiFi pods' Role because it holds every permission in it.
	operatorCfg := operatorUserConfig()

	mgr, err := ctrl.NewManager(operatorCfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Cache:   cache.Options{ByObject: CacheByObject()},
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred())

	Expect((&NiFiClusterReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("nifi-operator"),
		// The ServiceAccount needn't exist: the tests only assert the RoleBinding's subject.
		CSIDriverServiceAccount: csiDriverTestServiceAccount,
		// envtest runs no real NiFi process to poll or call (NIFI-248 B4's confirmOffloaded needs
		// both): this fake reports every current pod CONNECTED, and OFFLOADED once stepScaleDown
		// has asked to disconnect+offload it, so every existing scale-down spec keeps the timing
		// it already asserts on (pod-count-paced, no real wait). confirmOffloaded's own gating -
		// holding when not yet OFFLOADED, never forcing a stuck one through, no credentials - is
		// covered directly by TestConfirmOffloaded, not duplicated here.
		NewNiFiAPI: func(_ context.Context, cluster *nifiv1alpha1.NiFiCluster) (nifiAPI, error) {
			return &suiteNiFiAPI{client: mgr.GetAPIReader(), namespace: cluster.Namespace}, nil
		},
	}).SetupWithManager(mgr)).To(Succeed())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

// operatorUserConfig creates an envtest user bound to the generated operator ClusterRole and
// returns its rest.Config.
func operatorUserConfig() *rest.Config {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
	Expect(err).NotTo(HaveOccurred())
	role := &rbacv1.ClusterRole{}
	Expect(yaml.Unmarshal(raw, role)).To(Succeed())
	Expect(k8sClient.Create(ctx, role)).To(Succeed())

	user, err := testEnv.AddUser(envtest.User{Name: "nifi-operator-test"}, cfg)
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "nifi-operator-test"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "nifi-operator-test"}},
	})).To(Succeed())
	return user.Config()
}

var _ = AfterSuite(func() {
	cancel()
	Expect(testEnv.Stop()).To(Succeed())
})

// createNodeGroupObject creates a NiFiNodeGroup of cluster clusterName: the only way to have a second node
// group now, as clusters of earlier versions may. envtest runs no webhook, so it can be created,
// with spec.name set as the defaulting webhook did. Deleted at the end of the spec.
func createNodeGroupObject(namespace, clusterName string, ng nifiv1alpha1.NodeGroup) {
	o := &nifiv1alpha1.NiFiNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Name: ng.Name, Namespace: namespace},
		Spec:       nifiv1alpha1.NiFiNodeGroupSpec{ClusterRef: clusterName, NodeGroup: ng},
	}
	Expect(k8sClient.Create(ctx, o)).To(Succeed())
	DeferCleanup(func() { _ = k8sClient.Delete(ctx, o) })
}

// suiteOffloaded tracks, across the whole suite, every pod name a spec's SetNodeStatus call has
// already asked suiteNiFiAPI to offload - a package-level map, not a field on suiteNiFiAPI, since
// NewNiFiAPI's factory makes a fresh *suiteNiFiAPI on every single reconcile, but the "already
// offloaded" memory has to survive across them.
var (
	suiteOffloadedMu sync.Mutex
	suiteOffloaded   = map[string]bool{}
)

// suiteNiFiAPI is the nifiAPI envtest injects everywhere (see SetupWithManager above). envtest
// runs no kubelet, so most specs never create a real Pod for an ordinal at all - only the
// StatefulSets the reconciler itself writes are real objects here - so node existence is derived
// from each StatefulSet's own Spec.Replicas (ordinals 0..Replicas-1), not from Pods. Every such
// ordinal reports CONNECTED until this cluster's own reconciler has asked to offload it, then
// OFFLOADED - immediately, since nothing in envtest ever queues real data. nodeID is simply the
// pod name that ordinal would have.
type suiteNiFiAPI struct {
	client    client.Reader
	namespace string
}

func (f *suiteNiFiAPI) ListNodes(ctx context.Context) ([]nifiNode, error) {
	list := &appsv1.StatefulSetList{}
	if err := f.client.List(ctx, list, client.InNamespace(f.namespace)); err != nil {
		return nil, err
	}
	suiteOffloadedMu.Lock()
	defer suiteOffloadedMu.Unlock()
	var nodes []nifiNode
	for i := range list.Items {
		sts := &list.Items[i]
		replicas := int32(1)
		if sts.Spec.Replicas != nil {
			replicas = *sts.Spec.Replicas
		}
		for ord := int32(0); ord < replicas; ord++ {
			name := fmt.Sprintf("%s-%d", sts.Name, ord)
			status := "CONNECTED"
			if suiteOffloaded[name] {
				status = "OFFLOADED"
			}
			nodes = append(nodes, nifiNode{NodeID: name, Address: name + "." + f.namespace + ".svc.cluster.local", Status: status})
		}
	}
	return nodes, nil
}

func (f *suiteNiFiAPI) SetNodeStatus(_ context.Context, nodeID, status string) error {
	if status == "OFFLOADING" {
		suiteOffloadedMu.Lock()
		suiteOffloaded[nodeID] = true
		suiteOffloadedMu.Unlock()
	}
	return nil
}

func (f *suiteNiFiAPI) DeleteNode(context.Context, string) error { return nil }
