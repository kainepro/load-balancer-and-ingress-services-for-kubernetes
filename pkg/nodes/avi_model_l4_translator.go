/*
* [2013] - [2019] Avi Networks Incorporated
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

package nodes

import (
	"fmt"

	avimodels "github.com/avinetworks/sdk/go/models"
	avicache "gitlab.eng.vmware.com/orion/akc/pkg/cache"
	"gitlab.eng.vmware.com/orion/akc/pkg/lib"
	"gitlab.eng.vmware.com/orion/container-lib/utils"
	corev1 "k8s.io/api/core/v1"
)

func contains(s []int32, e int32) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func (o *AviObjectGraph) ConstructAviL4VsNode(svcObj *corev1.Service, key string) *AviVsNode {
	var avi_vs_meta *AviVsNode

	// FQDN should come from the cloud. Modify
	avi_vs_meta = &AviVsNode{Name: svcObj.ObjectMeta.Name + "--" + svcObj.ObjectMeta.Namespace, Tenant: utils.ADMIN_NS,
		EastWest: false, ServiceMetadata: avicache.LBServiceMetadataObj{ServiceName: svcObj.ObjectMeta.Name, Namespace: svcObj.ObjectMeta.Namespace}}

	vrfcontext := lib.GetVrf()
	avi_vs_meta.VrfContext = vrfcontext

	isTCP := false
	var portProtocols []AviPortHostProtocol
	for _, port := range svcObj.Spec.Ports {
		pp := AviPortHostProtocol{Port: int32(port.Port), Protocol: fmt.Sprint(port.Protocol), Name: port.Name}
		portProtocols = append(portProtocols, pp)
		if port.Protocol == "" || port.Protocol == utils.TCP {
			isTCP = true
		}
	}
	avi_vs_meta.PortProto = portProtocols
	// Default case.
	avi_vs_meta.ApplicationProfile = utils.DEFAULT_L4_APP_PROFILE
	if !isTCP {
		avi_vs_meta.NetworkProfile = utils.SYSTEM_UDP_FAST_PATH
	} else {
		avi_vs_meta.NetworkProfile = utils.DEFAULT_TCP_NW_PROFILE
	}
	var fqdns []string
	vsVipNode := &AviVSVIPNode{Name: svcObj.ObjectMeta.Name + "-vsvip", Tenant: utils.ADMIN_NS,
		FQDNs: fqdns, EastWest: false, VrfContext: vrfcontext}
	avi_vs_meta.VSVIPRefs = append(avi_vs_meta.VSVIPRefs, vsVipNode)
	utils.AviLog.Info.Printf("key: %s, msg: created vs object: %s", key, utils.Stringify(avi_vs_meta))
	return avi_vs_meta
}

func (o *AviObjectGraph) ConstructAviTCPPGPoolNodes(svcObj *corev1.Service, vsNode *AviVsNode, key string) {
	for _, portProto := range vsNode.PortProto {
		filterPort := portProto.Port
		pgName := vsNode.Name + "-l4-" + fmt.Sprint(filterPort)

		pgNode := &AviPoolGroupNode{Name: pgName, Tenant: utils.ADMIN_NS, Port: fmt.Sprint(filterPort)}
		// For TCP - the PG to Pool relationship is 1x1
                poolNode := &AviPoolNode{Name: "l4-" + pgName, Tenant: utils.ADMIN_NS, Protocol: portProto.Protocol, PortName: portProto.Name}
		poolNode.VrfContext = lib.GetVrf()

		if servers := PopulateServers(poolNode, svcObj.ObjectMeta.Namespace, svcObj.ObjectMeta.Name, key); servers != nil {
			poolNode.Servers = servers
		}
		pool_ref := fmt.Sprintf("/api/pool?name=%s", poolNode.Name)
		pgNode.Members = append(pgNode.Members, &avimodels.PoolGroupMember{PoolRef: &pool_ref})

		vsNode.PoolRefs = append(vsNode.PoolRefs, poolNode)
		utils.AviLog.Info.Printf("key: %s, msg: evaluated L4 pool group values :%v", key, utils.Stringify(pgNode))
		utils.AviLog.Info.Printf("key: %s, msg: evaluated L4 pool values :%v", key, utils.Stringify(poolNode))
		vsNode.TCPPoolGroupRefs = append(vsNode.TCPPoolGroupRefs, pgNode)
		pgNode.CalculateCheckSum()
		poolNode.CalculateCheckSum()

		o.AddModelNode(poolNode)
		vsNode.PoolGroupRefs = append(vsNode.PoolGroupRefs, pgNode)
		o.GraphChecksum = o.GraphChecksum + pgNode.GetCheckSum()
		o.GraphChecksum = o.GraphChecksum + poolNode.GetCheckSum()
	}
}

func PopulateServers(poolNode *AviPoolNode, ns string, serviceName string, key string) []AviPoolMetaServer {
	// Find the servers that match the port.
	epObj, err := utils.GetInformers().EpInformer.Lister().Endpoints(ns).Get(serviceName)
	if err != nil {
		utils.AviLog.Info.Printf("key: %s, msg: error while retrieving endpoints", key)
		return nil
	}
	var pool_meta []AviPoolMetaServer
	for _, ss := range epObj.Subsets {
		port_match := false
		for _, epp := range ss.Ports {
			if poolNode.PortName == epp.Name {
				port_match = true
				poolNode.Port = epp.Port
				break
			}
		}
		if len(ss.Ports) == 1 && len(epObj.Subsets) == 1 {
			// If it's just a single port then we make that as the server port.
			port_match = true
			poolNode.Port = ss.Ports[0].Port
		}
		if port_match {
			var atype string
			utils.AviLog.Info.Printf("key: %s, msg: found port match for port %v", key, poolNode.Port)
			for _, addr := range ss.Addresses {

				ip := addr.IP
				if utils.IsV4(addr.IP) {
					atype = "V4"
				} else {
					atype = "V6"
				}
				a := avimodels.IPAddr{Type: &atype, Addr: &ip}
				server := AviPoolMetaServer{Ip: a}
				if addr.NodeName != nil {
					server.ServerNode = *addr.NodeName
				}
				pool_meta = append(pool_meta, server)
			}
		}
	}
	utils.AviLog.Info.Printf("key: %s, msg: servers for port: %v, are: %v", key, poolNode.Port, utils.Stringify(pool_meta))
	return pool_meta
}

func (o *AviObjectGraph) BuildL4LBGraph(namespace string, svcName string, key string) {
	var VsNode *AviVsNode
	svcObj, err := utils.GetInformers().ServiceInformer.Lister().Services(namespace).Get(svcName)
	if err != nil {
		utils.AviLog.Warning.Printf("key: %s, msg: error in obtaining the object for service: %s", key, svcName)
		return
	}
	VsNode = o.ConstructAviL4VsNode(svcObj, key)
	o.ConstructAviTCPPGPoolNodes(svcObj, VsNode, key)
	o.AddModelNode(VsNode)
	VsNode.CalculateCheckSum()
	o.GraphChecksum = o.GraphChecksum + VsNode.GetCheckSum()
	utils.AviLog.Info.Printf("key: %s, msg: checksum  for AVI VS object %v", key, VsNode.GetCheckSum())
	utils.AviLog.Info.Printf("key: %s, msg: computed Graph checksum for VS is: %v", key, o.GraphChecksum)
}