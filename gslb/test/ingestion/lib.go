/*
 * Copyright 2019-2020 VMware, Inc.
 * All Rights Reserved.
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*   http://www.apache.org/licenses/LICENSE-2.0
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*/

package ingestion

import (
	"strings"
	"testing"
	"time"

	gslbalphav1 "github.com/vmware/global-load-balancing-services-for-kubernetes/internal/apis/amko/v1alpha1"

	gslbfake "github.com/vmware/global-load-balancing-services-for-kubernetes/internal/client/clientset/versioned/fake"

	oshiftfake "github.com/openshift/client-go/route/clientset/versioned/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/vmware/global-load-balancing-services-for-kubernetes/gslb/gslbutils"
	gslbingestion "github.com/vmware/global-load-balancing-services-for-kubernetes/gslb/ingestion"

	containerutils "github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

var (
	kubeClient      *k8sfake.Clientset
	keyChan         chan string
	oshiftClient    *oshiftfake.Clientset
	fooOshiftClient *oshiftfake.Clientset
	barOshiftClient *oshiftfake.Clientset
	testStopCh      <-chan struct{}
	gslbClient      *gslbfake.Clientset
	fooKubeClient   *k8sfake.Clientset
	barKubeClient   *k8sfake.Clientset
)

const (
	TestDomain1 = "host1.avi.com"
	TestDomain2 = "host2.avi.com"
	TestDomain3 = "host3.avi.com"
	TestDomain4 = "host4.avi.com"
	TestNS      = "test-def"
	TestSvc     = "foo-svc"
)

func getTestGSLBObject() *gslbalphav1.GSLBConfig {
	memberClusters := []gslbalphav1.MemberCluster{
		gslbalphav1.MemberCluster{
			ClusterContext: "cluster1",
		},
		gslbalphav1.MemberCluster{
			ClusterContext: "cluster2",
		},
	}
	gslbConfigObj := &gslbalphav1.GSLBConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "avi-system",
			Name:            "gslb-config-1",
			ResourceVersion: "10",
		},
		Spec: gslbalphav1.GSLBConfigSpec{
			GSLBLeader:     gslbalphav1.GSLBLeader{"", "", ""},
			MemberClusters: memberClusters,
		},
	}
	return gslbConfigObj
}

func getTestGDPObject(appLabelReq, nsLabelReq bool) *gslbalphav1.GlobalDeploymentPolicy {
	ns := gslbutils.AVISystem
	matchRules := gslbalphav1.MatchRules{
		AppSelector:       gslbalphav1.AppSelector{},
		NamespaceSelector: gslbalphav1.NamespaceSelector{},
	}

	if appLabelReq {
		matchRules.AppSelector.Label = make(map[string]string)
		matchRules.AppSelector.Label["key"] = "value"
	}
	if nsLabelReq {
		matchRules.NamespaceSelector.Label = make(map[string]string)
		matchRules.NamespaceSelector.Label["ns"] = "value"
	}

	matchClusters := []string{"cluster1", "cluster2"}
	gdpSpec := gslbalphav1.GDPSpec{
		MatchRules:    matchRules,
		MatchClusters: matchClusters,
	}
	gdpMeta := metav1.ObjectMeta{
		Name:            "test-gdp-1",
		Namespace:       ns,
		ResourceVersion: "100",
	}
	gdp := gslbalphav1.GlobalDeploymentPolicy{
		ObjectMeta: gdpMeta,
		Spec:       gdpSpec,
	}
	return &gdp
}

func inKeyList(key string, data []string) bool {
	for _, d := range data {
		if key == d {
			return true
		}
	}
	return false
}

func waitAndVerify(t *testing.T, keyList []string, timeoutExpected bool) (bool, string) {
	waitChan := make(chan interface{})
	go func() {
		time.Sleep(10 * time.Second)
		waitChan <- 1
	}()

	select {
	case data := <-keyChan:
		t.Logf("Expected key(s): %s, got data: %s\n", strings.Join(keyList, ","), data)
		if timeoutExpected {
			// If the timeout is expected, then there shouldn't be anything on this channel
			if data != "" {
				errMsg := "Unexpected data: " + data
				return false, errMsg
			}
		}
		if !inKeyList(data, keyList) {
			errMsg := "key match error, expected key(s): " + strings.Join(keyList, ",") + ", got: " + data
			return false, errMsg
		}
	case _ = <-waitChan:
		t.Log("waiting for timeout")
		if timeoutExpected {
			return true, "Success"
		}
		return false, "Timed out waiting for key(s): " + strings.Join(keyList, ",")
	}
	return true, ""
}

func addGSLBTestConfigObject(obj interface{}) {
	// Initialize a foo kube client
	fooKubeClient = k8sfake.NewSimpleClientset()
	fooOshiftClient = oshiftfake.NewSimpleClientset()

	fooInformersArg := make(map[string]interface{})
	fooInformersArg[containerutils.INFORMERS_OPENSHIFT_CLIENT] = fooOshiftClient
	fooInformersArg[containerutils.INFORMERS_INSTANTIATE_ONCE] = false

	fooRegisteredInformers := []string{containerutils.RouteInformer, containerutils.IngressInformer, containerutils.ServiceInformer}
	fooInformerInstance := containerutils.NewInformers(containerutils.KubeClientIntf{fooKubeClient}, fooRegisteredInformers, fooInformersArg)
	fooCtrl := gslbingestion.GetGSLBMemberController("cluster1", fooInformerInstance)
	fooCtrl.Start(testStopCh)
	fooCtrl.SetupEventHandlers(gslbingestion.K8SInformers{fooKubeClient})

	// Initialize a bar kube client
	barKubeClient = k8sfake.NewSimpleClientset()
	barOshiftClient = oshiftfake.NewSimpleClientset()
	barInformersArg := make(map[string]interface{})
	barInformersArg[containerutils.INFORMERS_OPENSHIFT_CLIENT] = barOshiftClient
	barInformersArg[containerutils.INFORMERS_INSTANTIATE_ONCE] = false

	barRegisteredInformers := []string{containerutils.RouteInformer, containerutils.IngressInformer, containerutils.ServiceInformer}
	barInformerInstance := containerutils.NewInformers(containerutils.KubeClientIntf{barKubeClient}, barRegisteredInformers, barInformersArg)
	barCtrl := gslbingestion.GetGSLBMemberController("cluster2", barInformerInstance)
	barCtrl.Start(testStopCh)
	barCtrl.SetupEventHandlers(gslbingestion.K8SInformers{barKubeClient})
}

func GetIngressKey(op, cname, ns, name, host string) string {
	return op + "/" + gslbutils.IngressType + "/" + cname + "/" + ns + "/" + name + "/" + host
}

func buildIngressKeyAndVerify(t *testing.T, timeoutExpected bool, op, cname, ns, name, hostname string) {
	actualKey := GetIngressKey(op, cname, ns, name, hostname)
	passed, errStr := waitAndVerify(t, []string{actualKey}, timeoutExpected)
	if !passed {
		t.Fatal(errStr)
	}
}

func buildIngMultiHostKeyAndVerify(t *testing.T, timeoutExpected bool, op, cname, ns, name string,
	hostIPs map[string]string) {

	keys := []string{}
	for host := range hostIPs {
		newKey := GetIngressKey(op, cname, ns, name, host)
		keys = append(keys, newKey)
	}
	for range keys {
		// Have to verify for all the keys, since no order is guranteed
		passed, errStr := waitAndVerify(t, keys, timeoutExpected)
		if !passed {
			t.Fatal(errStr)
		}
	}
}

func GetSvcKey(op, cname, ns, name string) string {
	return op + "/" + gslbutils.SvcType + "/" + cname + "/" + ns + "/" + name
}

func buildSvcKeyAndVerify(t *testing.T, timeoutExpected bool, op, cname, ns, name string) {
	actualKey := GetSvcKey(op, cname, ns, name)
	passed, errStr := waitAndVerify(t, []string{actualKey}, timeoutExpected)
	if !passed {
		t.Fatal(errStr)
	}
}

func GetRouteKey(op, cname, ns, name string) string {
	return op + "/" + gslbutils.RouteType + "/" + cname + "/" + ns + "/" + name
}

func buildRouteKeyAndVerify(t *testing.T, timeoutExpected bool, op, cname, ns, name string) {
	actualKey := GetRouteKey(op, cname, ns, name)
	passed, errStr := waitAndVerify(t, []string{actualKey}, timeoutExpected)
	if !passed {
		t.Fatal(errStr)
	}
}

func addGDPAndGSLBForIngress(t *testing.T) *gslbalphav1.GlobalDeploymentPolicy {
	gslbObj := getTestGSLBObject()
	gc, err := gslbingestion.IsGSLBConfigValid(gslbObj)
	if err != nil {
		t.Fatal("GSLB object invalid")
	}
	addGSLBTestConfigObject(gc)
	gslbutils.AddClusterContext("cluster1")
	gslbutils.AddClusterContext("cluster2")

	ingestionQ := containerutils.SharedWorkQueue().GetQueueByName(containerutils.ObjectIngestionLayer)
	gdp := getTestGDPObject(true, false)
	gslbingestion.AddGDPObj(gdp, ingestionQ.Workqueue, 2)

	return gdp
}

func DeleteTestGDPObj(gdp *gslbalphav1.GlobalDeploymentPolicy) {
	ingestionQ := containerutils.SharedWorkQueue().GetQueueByName(containerutils.ObjectIngestionLayer)
	gslbingestion.DeleteGDPObj(gdp, ingestionQ.Workqueue, 2)
}
