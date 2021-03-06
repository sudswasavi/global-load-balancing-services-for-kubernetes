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

package gslbutils

import (
	"errors"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gslbalphav1 "github.com/vmware/global-load-balancing-services-for-kubernetes/internal/apis/amko/v1alpha1"

	gslbcs "github.com/vmware/global-load-balancing-services-for-kubernetes/internal/client/clientset/versioned"

	routev1 "github.com/openshift/api/route/v1"
	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
	"k8s.io/api/networking/v1beta1"
	"k8s.io/client-go/kubernetes"
)

const (
	// MaxClusters is the supported number of clusters
	MaxClusters int = 10
	// GSLBKubePath is a temporary path to put the kubeconfig
	GSLBKubePath = "/tmp/gslb-kubeconfig"
	//AVISystem is the namespace where everything AVI related is created
	AVISystem = "avi-system"
	// Ingestion layer operations
	ObjectAdd    = "ADD"
	ObjectDelete = "DELETE"
	ObjectUpdate = "UPDATE"
	// Ingestion layer objects
	RouteType        = gslbalphav1.RouteObj
	IngressType      = gslbalphav1.IngressObj
	SvcType          = gslbalphav1.LBSvcObj
	PassthroughRoute = "passthrough"
	// Refresh cycle for AVI cache in seconds
	DefaultRefreshInterval = 600
	// Store types
	AcceptedStore = "Accepted"
	RejectedStore = "Rejected"

	// Multi-cluster key lengths
	IngMultiClusterKeyLen = 6
	MultiClusterKeyLen    = 5

	// Default values for Retry Operations
	SlowSyncTime      = 120
	SlowRetryQueue    = "SlowRetry"
	FastRetryQueue    = "FastRetry"
	DefaultRetryCount = 5

	AmkoUser = "amko-gslb"

	NumRestWorkers = 8

	// Service Protocols
	ProtocolTCP = "TCP"
	ProtocolUDP = "UDP"

	// Health monitors
	SystemHealthMonitorTypeTCP   = "HEALTH_MONITOR_TCP"
	SystemHealthMonitorTypeUDP   = "HEALTH_MONITOR_UDP"
	SystemGslbHealthMonitorTCP   = "System-GSLB-TCP"
	SystemGslbHealthMonitorHTTP  = "HEALTH_MONITOR_HTTP"
	SystemGslbHealthMonitorHTTPS = "HEALTH_MONITOR_HTTPS"

	// default passthrough health monitor (TCP), to be used for all passthrough routes
	SystemGslbHealthMonitorPassthrough = "amko--passthrough-hm-tcp"

	// Ports for health monitoring
	DefaultTCPHealthMonitorPort   = "80"
	DefaultHTTPHealthMonitorPort  = 80
	DefaultHTTPSHealthMonitorPort = 443

	// Timeout for rest operations
	RestTimeoutSecs = 600
)

// InformersPerCluster is the number of informers per cluster
var InformersPerCluster *utils.AviCache

func SetInformersPerCluster(clusterName string, info *utils.Informers) {
	InformersPerCluster.AviCacheAdd(clusterName, info)
}

func GetInformersPerCluster(clusterName string) *utils.Informers {
	info, ok := InformersPerCluster.AviCacheGet(clusterName)
	if !ok {
		utils.AviLog.Warnf("Failed to get informer for cluster %v", clusterName)
		return nil
	}
	return info.(*utils.Informers)
}

func MultiClusterKey(operation, objType, clusterName, ns, objName string) string {
	compositeObjName := clusterName + "/" + ns + "/" + objName
	return MultiClusterKeyWithObjName(operation, objType, compositeObjName)
}

func MultiClusterKeyWithObjName(operation, objType, compositeName string) string {
	return operation + "/" + objType + "/" + compositeName
}

func ExtractMultiClusterKey(key string) (string, string, string, string, string) {
	segments := strings.Split(key, "/")
	var operation, objType, cluster, ns, name, hostname string
	if segments[1] == IngressType {
		if len(segments) == IngMultiClusterKeyLen {
			operation, objType, cluster, ns, name, hostname = segments[0], segments[1], segments[2], segments[3], segments[4], segments[5]
			name += "/" + hostname
		}
	} else if len(segments) == MultiClusterKeyLen {
		operation, objType, cluster, ns, name = segments[0], segments[1], segments[2], segments[3], segments[4]
	}
	return operation, objType, cluster, ns, name
}

func SplitMultiClusterObjectName(name string) (string, string, string, error) {
	if name == "" {
		return "", "", "", errors.New("multi-cluster route/svc name is empty")
	}
	reqList := strings.Split(name, "/")

	if len(reqList) != 3 {
		return "", "", "", errors.New("multi-cluster route/svc name format is unexpected")
	}
	return reqList[0], reqList[1], reqList[2], nil
}

func SplitMultiClusterIngHostName(name string) (string, string, string, string, error) {
	if name == "" {
		return "", "", "", "", errors.New("multi-cluster ingress host name is empty")
	}
	reqList := strings.Split(name, "/")

	if len(reqList) != 4 {
		return "", "", "", "", errors.New("multi-cluster ingress name format is unexpected")
	}
	return reqList[0], reqList[1], reqList[2], reqList[3], nil
}

func SplitMultiClusterNS(name string) (string, string, error) {
	if name == "" {
		return "", "", errors.New("multi-cluster namespace is empty")
	}
	reqList := strings.Split(name, "/")
	if len(reqList) != 2 {
		return "", "", errors.New("multi-cluster namespace format is unexpected")
	}
	return reqList[0], reqList[1], nil
}

func RouteGetIPAddr(route *routev1.Route) (string, bool) {
	hostname := route.Spec.Host
	// Return true if the IP address is present in an route's status field, else return false
	routeStatus := route.Status
	for _, ingr := range routeStatus.Ingress {
		// check if the status message was populated by ako
		if !strings.HasPrefix(ingr.RouterName, "ako-") {
			continue
		}
		conditions := ingr.Conditions
		// check the hostname with the route's status hostname field
		if ingr.Host != hostname {
			continue
		}
		for _, condition := range conditions {
			// TODO: Check if the message field contains an IP address
			if condition.Message == "" {
				continue
			}
			// Check if this is a IP address
			addr := net.ParseIP(condition.Message)
			if addr != nil {
				return condition.Message, true
			}
		}
	}
	return "", false
}

type IngressHostIP struct {
	Hostname string
	IPAddr   string
}

func getHostListFromIngress(ingress *v1beta1.Ingress) []string {
	hostList := []string{}
	for _, rule := range ingress.Spec.Rules {
		if rule.Host != "" {
			hostList = append(hostList, rule.Host)
		}
	}
	return hostList
}

func IngressGetIPAddrs(ingress *v1beta1.Ingress) []IngressHostIP {
	ingHostIP := []IngressHostIP{}
	hostList := getHostListFromIngress(ingress)
	ingStatus := ingress.Status
	ingList := ingStatus.LoadBalancer.Ingress
	if len(ingList) == 0 {
		Logf("Ingress %s doesn't have the status field populated", ingress.GetObjectMeta().GetName())
		Debugf("Ingress: %v", ingress)
		return ingHostIP
	}
	for _, ingr := range ingList {
		// Check if this is a IP address
		addr := net.ParseIP(ingr.IP)
		if addr == nil {
			Warnf("Address %s is not an IP address: %s", addr)
			continue
		}
		// Found an IP address, return
		if ingr.Hostname == "" {
			Warnf("Hostname is empty in ingress %s", ingress.Name)
			continue
		}
		if utils.HasElem(hostList, ingr.Hostname) {
			ingHostIP = append(ingHostIP, IngressHostIP{
				Hostname: ingr.Hostname,
				IPAddr:   ingr.IP,
			})
		}
	}
	return ingHostIP
}

// Logf is aliased to utils' Info.Printf
var Logf = utils.AviLog.Infof

// Errf is aliased to utils' Error.Printf
var Errf = utils.AviLog.Errorf

// Warnf is aliased to utils' Warning.Printf
var Warnf = utils.AviLog.Warnf

// Debugf is aliased to utils' Debug.Printf
var Debugf = utils.AviLog.Debugf

// Cluster Routes store for all the route objects.
var (
	AcceptedRouteStore   *ClusterStore
	RejectedRouteStore   *ClusterStore
	AcceptedLBSvcStore   *ClusterStore
	RejectedLBSvcStore   *ClusterStore
	AcceptedIngressStore *ClusterStore
	RejectedIngressStore *ClusterStore
	AcceptedNSStore      *ObjectStore
	RejectedNSStore      *ObjectStore
)

func GetGSLBServiceChecksum(ipList, domainList, memberObjs []string, hmNames []string) uint32 {
	sort.Strings(ipList)
	sort.Strings(domainList)
	sort.Strings(memberObjs)
	sort.Strings(hmNames)

	// checksum has to take into consideration the non-path HMs and the path based HMs

	return utils.Hash(utils.Stringify(ipList)) +
		utils.Hash(utils.Stringify(domainList)) +
		utils.Hash(utils.Stringify(memberObjs)) +
		utils.Hash(utils.Stringify(hmNames))
}

func GetGSLBHmChecksum(name, hmType string, port int32) uint32 {
	portStr := strconv.FormatInt(int64(port), 10)
	return utils.Hash(name) + utils.Hash(hmType) + utils.Hash(portStr)
}

func GetAviAdminTenantRef() string {
	return "https://" + os.Getenv("GSLB_CTRL_IPADDRESS") + "/api/tenant/" + utils.ADMIN_NS
}

// GSLBConfigObj is global and is initialized only once
type GSLBConfigObj struct {
	configObj  *gslbalphav1.GSLBConfig
	configLock sync.RWMutex
}

var gcObj GSLBConfigObj

// gslbConfigObjectAdded is a channel which halts the initialization operation until a GSLB config object
// is added. Even the GDP informers are started after this operation goes through.
// This channel's usage can be found in gslb.go.
var gslbConfigObjectAdded chan bool
var chanOnce sync.Once

func GetGSLBConfigObjectChan() *chan bool {
	chanOnce.Do(func() {
		gslbConfigObjectAdded = make(chan bool, 1)
	})
	return &gslbConfigObjectAdded
}

func GetGSLBConfigNameAndNS() (string, string) {
	gcObj.configLock.Lock()
	defer gcObj.configLock.Unlock()

	if gcObj.configObj == nil {
		return "", ""
	}
	return gcObj.configObj.Name, gcObj.configObj.Namespace
}

func updateGSLBConfigStatusMsg(msg string) {
	gcObj.configLock.Lock()
	defer gcObj.configLock.Unlock()

	gcObj.configObj.Status.State = msg
}

func SetGSLBConfigObj(gc *gslbalphav1.GSLBConfig) {
	gcObj.configLock.Lock()
	defer gcObj.configLock.Unlock()

	gcObj.configObj = gc
}

func UpdateGSLBConfigStatus(msg string) error {
	if !PublishGSLBStatus {
		return nil
	}

	updateGSLBConfigStatusMsg(msg)
	updatedGC, updateErr := GlobalGslbClient.AmkoV1alpha1().GSLBConfigs(gcObj.configObj.ObjectMeta.Namespace).Update(gcObj.configObj)
	if updateErr != nil {
		Errf("error in updating the GSLBConfig object: %s", updateErr.Error())
		return errors.New("error in GSLBConfig object update, " + updateErr.Error())
	}
	SetGSLBConfigObj(updatedGC)
	return nil
}

// gslbConfigSet and its setter and getter functions, to be used by the AddGSLBConfig method. This value
// is set to true once a GSLB Configuration has been successfully done.
var gslbConfigSet bool = false

func IsGSLBConfigSet() bool {
	return gslbConfigSet
}

func SetGSLBConfig(value bool) {
	gslbConfigSet = value
}

var GlobalKubeClient *kubernetes.Clientset
var GlobalGslbClient *gslbcs.Clientset
var PublishGDPStatus bool
var PublishGSLBStatus bool

type AviControllerConfig struct {
	Username string
	Password string
	IPAddr   string
	Version  string
}

var gslbLeaderConfig AviControllerConfig
var leaderConfig sync.Once

func NewAviControllerConfig(username, password, ipAddr, version string) *AviControllerConfig {
	leaderConfig.Do(func() {
		gslbLeaderConfig = AviControllerConfig{
			Username: username,
			Password: password,
			IPAddr:   ipAddr,
			Version:  version,
		}
	})
	return &gslbLeaderConfig
}

func GetAviConfig() AviControllerConfig {
	return gslbLeaderConfig
}

var initializedClusterContexts []string

func AddClusterContext(cc string) {
	if IsClusterContextPresent(cc) {
		return
	}
	initializedClusterContexts = append(initializedClusterContexts, cc)
}

func IsClusterContextPresent(cc string) bool {
	for _, context := range initializedClusterContexts {
		if context == cc {
			return true
		}
	}
	return false
}

var controllerIsLeader bool

func SetControllerAsLeader() {
	controllerIsLeader = true
}

func SetControllerAsFollower() {
	controllerIsLeader = false
}

func IsControllerLeader() bool {
	return controllerIsLeader
}

func GetKeyIdx(strList []string, key string) (int, bool) {
	for i, str := range strList {
		if str == key {
			return i, true
		}
	}
	return -1, false
}

var waitGroupMap map[string]*sync.WaitGroup
var wgSyncOnce sync.Once

const (
	WGIngestion = "ingestion"
	WGFastRetry = "fastretry"
	WGSlowRetry = "slowretry"
	WGGraph     = "graph"
)

func SetWaitGroupMap() {
	wgSyncOnce.Do(func() {
		waitGroupMap = make(map[string]*sync.WaitGroup)
		waitGroupMap[WGIngestion] = &sync.WaitGroup{}
		waitGroupMap[WGFastRetry] = &sync.WaitGroup{}
		waitGroupMap[WGGraph] = &sync.WaitGroup{}
		waitGroupMap[WGSlowRetry] = &sync.WaitGroup{}
	})
}

func GetWaitGroupFromMap(name string) *sync.WaitGroup {
	wg, ok := waitGroupMap[name]
	if !ok {
		return nil
	}
	return wg
}

func WaitForWorkersToExit() {
	timeoutChan := make(chan struct{})
	// timeout after 10 seconds
	timeout := 10 * time.Second
	go func() {
		defer close(timeoutChan)
		for _, wg := range waitGroupMap {
			wg.Wait()
		}
	}()
	select {
	case <-timeoutChan:
		return
	case <-time.After(timeout):
		return
	}
}

func IsLogLevelValid(level string) bool {
	_, ok := utils.LogLevelMap[level]
	return ok
}

type ResyncStatus struct {
	required bool
	lock     sync.RWMutex
}

var resync ResyncStatus
var resyncOnce sync.Once

func InitResync() {
	resyncOnce.Do(func() {
		resync.required = false
	})
}

func SetResyncRequired(value bool) {
	resync.lock.Lock()
	defer resync.lock.Unlock()
	resync.required = value
}

func IsResyncRequired() bool {
	resync.lock.RLock()
	defer resync.lock.RUnlock()
	return resync.required
}

func GetHmTypeForProtocol(protocol string) (string, error) {
	switch protocol {
	case ProtocolTCP:
		return SystemHealthMonitorTypeTCP, nil
	case ProtocolUDP:
		return SystemHealthMonitorTypeUDP, nil
	default:
		return "", errors.New("unrecognized protocol")
	}
}

func GetHmTypeForTLS(tls bool) string {
	if tls {
		return SystemGslbHealthMonitorHTTPS
	}
	return SystemGslbHealthMonitorHTTP
}

func BuildHmPathName(gsName, path string, isSec bool) string {
	prefix := "amko--http--"
	if isSec {
		prefix = "amko--https--"
	}
	return prefix + gsName + "--" + path
}

func GetPathFromHmName(hmName string) string {
	hmNameSplit := strings.Split(hmName, "--")
	if len(hmNameSplit) != 4 {
		Errf("hmName: %s, msg: hm is malformed, expected a path based hm", hmName)
		return ""
	}

	return hmNameSplit[3]
}

func BuildNonPathHmName(gsName string) string {
	return "amko--" + gsName
}

func GetGSFromHmName(hmName string) (string, error) {
	// for path based hms
	hmNameSplit := strings.Split(hmName, "--")
	if len(hmNameSplit) == 4 {
		return hmNameSplit[2], nil
	} else if len(hmNameSplit) == 2 {
		return hmNameSplit[1], nil
	}
	return "", errors.New("error in parsing gs name, unexpected format")
}
