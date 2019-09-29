package flowgenerator

import (
	"github.com/google/gopacket/layers"
	. "gitlab.x.lan/yunshan/droplet-libs/datatype"
)

func (m *FlowMap) initUdpFlow(flowExtra *FlowExtra, meta *MetaPacket) {
	m.initFlow(flowExtra, meta)
	taggedFlow := flowExtra.taggedFlow
	flowMetricsPeerSrc := &taggedFlow.FlowMetricsPeers[FLOW_METRICS_PEER_SRC]
	flowMetricsPeerSrc.TotalPacketCount = 1
	flowMetricsPeerSrc.PacketCount = 1
	flowMetricsPeerSrc.TickPacketCount = 1
	flowMetricsPeerSrc.TotalByteCount = uint64(meta.PacketLen)
	flowMetricsPeerSrc.ByteCount = uint64(meta.PacketLen)
	flowMetricsPeerSrc.TickByteCount = uint64(meta.PacketLen)
	updatePlatformData(taggedFlow, meta.EndpointData, false)
	m.fillGeoInfo(taggedFlow)
	flowExtra.flowState = FLOW_STATE_ESTABLISHED
	flowExtra.timeout = openingTimeout
	m.updateUDPDirection(meta, flowExtra, true) // 新建流时矫正流方向
	meta.IsActiveService = taggedFlow.IsActiveService
}

func (m *FlowMap) updateUdpFlow(flowExtra *FlowExtra, meta *MetaPacket) {
	m.updateFlow(flowExtra, meta)
	if flowExtra.taggedFlow.FlowMetricsPeers[FLOW_METRICS_PEER_SRC].PacketCount > 0 &&
		flowExtra.taggedFlow.FlowMetricsPeers[FLOW_METRICS_PEER_DST].PacketCount > 0 {
		flowExtra.timeout = establishedRstTimeout
	}
	meta.IsActiveService = flowExtra.taggedFlow.IsActiveService
}

func (m *FlowMap) updateUDPDirection(meta *MetaPacket, flowExtra *FlowExtra, isFirstPacket bool) {
	srcScore, dstScore := uint8(0), uint8(0)
	if meta.EthType == layers.EthernetTypeIPv4 {
		srcKey := ServiceKey(int16(meta.EndpointData.SrcInfo.L3EpcId), meta.IpSrc, meta.PortSrc)
		dstKey := ServiceKey(int16(meta.EndpointData.DstInfo.L3EpcId), meta.IpDst, meta.PortDst)

		srcScore, dstScore = m.udpServiceTable.GetUDPScore(isFirstPacket, srcKey, dstKey)
		if meta.Direction == SERVER_TO_CLIENT {
			srcScore, dstScore = dstScore, srcScore
		}
	} else {
		ServiceKey6(m.srcServiceKey, int16(meta.EndpointData.SrcInfo.L3EpcId), meta.Ip6Src, meta.PortSrc)
		ServiceKey6(m.dstServiceKey, int16(meta.EndpointData.DstInfo.L3EpcId), meta.Ip6Dst, meta.PortDst)

		srcScore, dstScore = m.udpServiceTable6.GetUDPScore(isFirstPacket, m.srcServiceKey, m.dstServiceKey)
		if meta.Direction == SERVER_TO_CLIENT {
			srcScore, dstScore = dstScore, srcScore
		}
	}

	if !IsClientToServer(srcScore, dstScore) {
		flowExtra.reverseFlow()
		flowExtra.reversed = !flowExtra.reversed
		meta.Direction = (CLIENT_TO_SERVER + SERVER_TO_CLIENT) - meta.Direction // reverse
	}
	flowExtra.taggedFlow.IsActiveService = IsActiveService(srcScore, dstScore)
}
