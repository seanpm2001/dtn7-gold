package core

import (
	"fmt"
	"github.com/RyanCarrier/dijkstra"
	"github.com/dtn7/cboring"
	"github.com/dtn7/dtn7-go/bundle"
	"github.com/dtn7/dtn7-go/cla"
	"io"
	"time"

	log "github.com/sirupsen/logrus"
)

const BroadcastAddress = "dtn:broadcast"

// timestampNow outputs the current UNIX-time as an unsigned int64
// (I have no idea, why this is signed by default... does the kernel even allow you to set a negative time?)
func timestampNow() uint64 {
	return uint64(time.Now().Unix())
}

type DTLSRConfig struct {
	// RecomputeTime is the interval (in seconds) until the routing table is recomputed
	RecomputeTime time.Duration
	// BroadcastTime is the interval (in seconds) between broadcasts of peer data
	// Note: Broadcast only happens when there was a change in peer data
	BroadcastTime time.Duration
	// PurgeTime is the interval (in seconds) after which a disconnected peer is removed from the peer list
	PurgeTime time.Duration
}

// DTLSR is an implementation of "Delay Tolerant Link State Routing"
type DTLSR struct {
	c *Core
	// routingTable is a [endpoint]forwardingNode mapping
	routingTable map[bundle.EndpointID]bundle.EndpointID
	// peerChange denotes whether there has been a change in our direct connections
	// since we last calculated our routing table/broadcast our peer data
	peerChange bool
	// peers is our own peerData
	peers peerData
	// receivedChange denotes whether we received new data since we last computed our routing table
	receivedChange bool
	// receivedData is peerData received from other nodes
	receivedData map[bundle.EndpointID]peerData
	// nodeIndex and index Node are a bidirectional mapping EndpointID <-> uint64
	// necessary since the dijkstra implementation only accepts integer node identifiers
	nodeIndex map[bundle.EndpointID]int
	indexNode []bundle.EndpointID
	length    int
	// broadcastAddress is where metadata-bundles are sent to
	broadcastAddress bundle.EndpointID
	// purgeTime is the time until a peer gets removed from the peer list
	purgeTime uint64
}

// peerData contains a peer's connection data
type peerData struct {
	// id is the node's endpoint id
	id bundle.EndpointID
	// timestamp is the time the last change occurred
	// when receiving other node's data, we only update if the timestamp in newer
	timestamp uint64
	// peers is a mapping of previously seen peers and the respective timestamp of the last encounter
	peers map[bundle.EndpointID]uint64
}

func (pd peerData) isNewerThan(other peerData) bool {
	return pd.timestamp > other.timestamp
}

func NewDTLSR(c *Core, config DTLSRConfig) DTLSR {
	log.WithFields(log.Fields{
		"config": config,
	}).Debug("Initialising DTLSR")

	bAddress, err := bundle.NewEndpointID(BroadcastAddress)
	if err != nil {
		log.WithFields(log.Fields{
			"BroadcastAddress": BroadcastAddress,
		}).Fatal("Unable to parse broadcast address")
	}

	dtlsr := DTLSR{
		c:            c,
		routingTable: make(map[bundle.EndpointID]bundle.EndpointID),
		peerChange:   false,
		peers: peerData{
			id:        c.NodeId,
			timestamp: timestampNow(),
			peers:     make(map[bundle.EndpointID]uint64),
		},
		receivedChange:   false,
		receivedData:     make(map[bundle.EndpointID]peerData),
		nodeIndex:        map[bundle.EndpointID]int{c.NodeId: 0},
		indexNode:        []bundle.EndpointID{c.NodeId},
		length:           1,
		broadcastAddress: bAddress,
		purgeTime:        uint64(config.PurgeTime),
	}

	err = c.cron.Register("dtlsr_purge", dtlsr.purgePeers, time.Second*config.PurgeTime)
	if err != nil {
		log.WithFields(log.Fields{
			"reason": err,
		}).Warn("Could not register DTLSR purge job")
	}
	err = c.cron.Register("dtlsr_recompute", dtlsr.recomputeCron, time.Second*config.RecomputeTime)
	if err != nil {
		log.WithFields(log.Fields{
			"reason": err,
		}).Warn("Could not register DTLSR recompute job")
	}
	err = c.cron.Register("dtlsr_broadcast", dtlsr.broadcastCron, time.Second*config.BroadcastTime)
	if err != nil {
		log.WithFields(log.Fields{
			"reason": err,
		}).Warn("Could not register DTLSR broadcast job")
	}

	return dtlsr
}

func (dtlsr DTLSR) NotifyIncoming(bp BundlePack) {
	if metaDataBlock, err := bp.MustBundle().ExtensionBlock(ExtBlockTypeDTLSRBlock); err == nil {
		log.WithFields(log.Fields{
			"peer": bp.MustBundle().PrimaryBlock.SourceNode,
		}).Debug("Received metadata")

		dtlsrBlock := metaDataBlock.Value.(*DTLSRBlock)
		data := dtlsrBlock.getPeerData()

		log.WithFields(log.Fields{
			"peer": bp.MustBundle().PrimaryBlock.SourceNode,
			"data": data,
		}).Debug("Decoded peer data")

		storedData, present := dtlsr.receivedData[data.id]

		if !present {
			log.Debug("Data for new peer")
			// if we didn't have any data for that peer, we simply add it
			dtlsr.receivedData[data.id] = data
			dtlsr.receivedChange = true

			// track node
			dtlsr.newNode(data.id)

			// track peers of this node
			for node := range data.peers {
				dtlsr.newNode(node)
			}
		} else {
			// check if the received data is newer and replace it if it is
			if data.isNewerThan(storedData) {
				log.Debug("Updating peer data")
				dtlsr.receivedData[data.id] = data
				dtlsr.receivedChange = true

				// track peers of this node
				for node := range data.peers {
					dtlsr.newNode(node)
				}
			}
		}
	}
}

func (_ DTLSR) ReportFailure(_ BundlePack, _ cla.ConvergenceSender) {
	// if the transmission failed, that is sad, but there is really nothing to do...
	return
}

func (dtlsr DTLSR) SenderForBundle(bp BundlePack) (sender []cla.ConvergenceSender, delete bool) {
	delete = false

	if bp.MustBundle().PrimaryBlock.Destination == dtlsr.broadcastAddress {
		// broadcast bundles are always forwarded to everyone
		log.WithFields(log.Fields{
			"bundle":    bp.MustBundle().ID(),
			"recipient": bp.MustBundle().PrimaryBlock.Destination,
		}).Debug("Relaying broadcast bundle")
		sender = dtlsr.c.claManager.Sender()
		return
	}

	recipient := bp.MustBundle().PrimaryBlock.Destination
	forwarder, present := dtlsr.routingTable[recipient]
	if !present {
		// we don't know where to forward this bundle
		log.WithFields(log.Fields{
			"bundle":    bp.ID(),
			"recipient": recipient,
		}).Debug("DTLSR could not find a node to forward to")
		return
	}

	for _, cs := range dtlsr.c.claManager.Sender() {
		if cs.GetPeerEndpointID() == forwarder {
			sender = append(sender, cs)
			log.WithFields(log.Fields{
				"bundle":              bp.ID(),
				"recipient":           recipient,
				"convergence-senders": sender,
			}).Debug("DTLSR selected Convergence Sender for an outgoing bundle")
			// we only ever forward to a single node
			return
		}
	}

	log.WithFields(log.Fields{
		"bundle":    bp.ID(),
		"recipient": recipient,
	}).Debug("DTLSR could not find forwarder amongst connected nodes")
	return
}

func (dtlsr DTLSR) ReportPeerAppeared(peer cla.Convergence) {
	log.WithFields(log.Fields{
		"peer": peer,
	}).Debug("Peer appeared")

	peerReceiver, ok := peer.(cla.ConvergenceReceiver)
	if !ok {
		log.Warn("Peer was not a ConvergenceReceiver")
		return
	}

	peerID := peerReceiver.GetEndpointID()

	// track node
	dtlsr.newNode(peerID)

	// add node to peer list
	dtlsr.peers.peers[peerID] = 0
	dtlsr.peers.timestamp = timestampNow()
	dtlsr.peerChange = true
}

func (dtlsr DTLSR) ReportPeerDisappeared(peer cla.Convergence) {
	log.WithFields(log.Fields{
		"peer": peer,
	}).Debug("Peer disappeared")

	peerReceiver, ok := peer.(cla.ConvergenceReceiver)
	if !ok {
		log.Warn("Peer was not a ConvergenceReceiver")
		return
	}

	peerID := peerReceiver.GetEndpointID()

	// set expiration timestamp for peer
	timestamp := timestampNow()
	dtlsr.peers.peers[peerID] = timestamp
	dtlsr.peers.timestamp = timestamp
	dtlsr.peerChange = true
}

// DispatchingAllowed allows the processing of all packages.
func (_ DTLSR) DispatchingAllowed(_ BundlePack) bool {
	return true
}

// newNode adds a node to the index-mapping (if it was not previously tracked)
func (dtlsr DTLSR) newNode(id bundle.EndpointID) {
	log.WithFields(log.Fields{
		"NodeID": id,
	}).Debug("Tracking Node")
	_, present := dtlsr.nodeIndex[id]

	if present {
		log.WithFields(log.Fields{
			"NodeID": id,
		}).Debug("Node already tracked")
		// node is already tracked
		return
	}

	dtlsr.nodeIndex[id] = dtlsr.length
	dtlsr.indexNode = append(dtlsr.indexNode, id)
	dtlsr.length = dtlsr.length + 1
	log.WithFields(log.Fields{
		"NodeID": id,
	}).Debug("Added node to tracking store")
}

// computeRoutingTable finds shortest paths using dijkstra's algorithm
func (dtlsr DTLSR) computeRoutingTable() {
	log.Debug("Recomputing routing table")

	currentTime := timestampNow()
	graph := dijkstra.NewGraph()

	// add vertices
	for i := 0; i < dtlsr.length; i++ {
		graph.AddVertex(i)
	}

	// add edges originating from this node
	for peer, timestamp := range dtlsr.peers.peers {
		var edgeCost int64
		if timestamp == 0 {
			edgeCost = 0
		} else {
			edgeCost = int64(currentTime - timestamp)
		}

		if err := graph.AddArc(0, dtlsr.nodeIndex[peer], edgeCost); err != nil {
			log.WithFields(log.Fields{
				"reason": err,
			}).Warn("Error computing routing table")
			return
		}

		log.WithFields(log.Fields{
			"peerA": dtlsr.c.NodeId,
			"peerB": peer,
			"cost":  edgeCost,
		}).Debug("Added vertex")
	}

	// add edges originating from other nodes
	for _, data := range dtlsr.receivedData {
		for peer, timestamp := range data.peers {
			var edgeCost int64
			if timestamp == 0 {
				edgeCost = 0
			} else {
				edgeCost = int64(currentTime - timestamp)
			}

			if err := graph.AddArc(dtlsr.nodeIndex[data.id], dtlsr.nodeIndex[peer], edgeCost); err != nil {
				log.WithFields(log.Fields{
					"reason": err,
				}).Warn("Error computing routing table")
				return
			}

			log.WithFields(log.Fields{
				"peerA": data.id,
				"peerB": peer,
				"cost":  edgeCost,
			}).Debug("Added vertex")
		}
	}

	routingTable := make(map[bundle.EndpointID]bundle.EndpointID)
	for i := 1; i < dtlsr.length; i++ {
		shortest, err := graph.Shortest(0, i)
		if err == nil {
			routingTable[dtlsr.indexNode[0]] = dtlsr.indexNode[shortest.Path[0]]
		}
	}

	log.WithFields(log.Fields{
		"routingTable": routingTable,
	}).Debug("Finished routing table computation")

	dtlsr.routingTable = routingTable
}

// recomputeCron gets called periodically by the core's cron module.
// Only actually triggers a recompute if the underlying data has changed.
func (dtlsr DTLSR) recomputeCron() {
	log.WithFields(log.Fields{
		"peerChange":     dtlsr.peerChange,
		"receivedChange": dtlsr.receivedChange,
	}).Debug("Executing recomputeCron")
	if dtlsr.peerChange || dtlsr.receivedChange {
		dtlsr.computeRoutingTable()
		dtlsr.receivedChange = false
	}
}

// broadcast broadcasts this node's peer data to the network
func (dtlsr DTLSR) broadcast() {
	// send broadcast bundle with our new peer data
	bundleBuilder := bundle.Builder()
	bundleBuilder.Destination(dtlsr.broadcastAddress)
	bundleBuilder.Source(dtlsr.c.NodeId)
	bundleBuilder.CreationTimestampNow()
	// no Payload
	bundleBuilder.PayloadBlock(0)
	metadatBundle, err := bundleBuilder.Build()
	if err != nil {
		log.WithFields(log.Fields{
			"reason": err,
		}).Warn("Unable to build metadata bundle")
		return
	}
	metadataBlock := NewDTLSRBlock(dtlsr.peers)
	metadatBundle.AddExtensionBlock(bundle.NewCanonicalBlock(0, 0, metadataBlock))

	dtlsr.c.SendBundle(&metadatBundle)
}

// broadcastCron gets called periodically by the core's cron module.
// Only actually triggers a broadcast if peer data has changed
func (dtlsr DTLSR) broadcastCron() {
	log.WithFields(log.Fields{
		"peerChange": dtlsr.peerChange,
	}).Debug("Executing broadcastCron")
	if dtlsr.peerChange {
		dtlsr.broadcast()
		dtlsr.peerChange = false

		// a change in our own peer data should also trigger a routing recompute
		// but if this method gets called before recomputeCron(),
		// we don't want this information to be lost
		dtlsr.receivedChange = true
	}
}

// purgePeers removes peers who have not been seen for a long time
func (dtlsr DTLSR) purgePeers() {
	log.Debug("Executing purgePeers")
	currentTime := timestampNow()

	for peerID, timestamp := range dtlsr.peers.peers {
		if timestamp != 0 && currentTime > timestamp+dtlsr.purgeTime {
			log.WithFields(log.Fields{
				"peer":            peerID,
				"disconnect_time": timestamp,
			}).Debug("Removing stale peer")
			delete(dtlsr.peers.peers, peerID)
			dtlsr.peerChange = true
		}
	}
}

// TODO: Turn this into an administrative record

const ExtBlockTypeDTLSRBlock uint64 = 193

// DTLSRBlock contains routing metadata
type DTLSRBlock peerData

func NewDTLSRBlock(data peerData) *DTLSRBlock {
	newBlock := DTLSRBlock(data)
	return &newBlock
}

func (dtlsrb *DTLSRBlock) getPeerData() peerData {
	return peerData(*dtlsrb)
}

func (dtlsrb *DTLSRBlock) BlockTypeCode() uint64 {
	return ExtBlockTypeDTLSRBlock
}

func (dtlsrb *DTLSRBlock) CheckValid() error {
	return nil
}

func (dtlsrb *DTLSRBlock) MarshalCbor(w io.Writer) error {
	// start with the (apparently) required outer array
	if err := cboring.WriteArrayLength(3, w); err != nil {
		return err
	}

	// write our own endpoint id
	if err := cboring.Marshal(&dtlsrb.id, w); err != nil {
		return err
	}

	// write the timestamp
	if err := cboring.WriteUInt(dtlsrb.timestamp, w); err != nil {
		return err
	}

	// write the peer data array header
	if err := cboring.WriteArrayLength(uint64(len(dtlsrb.peers)), w); err != nil {
		return err
	}

	// write the actual data
	for peerID, timestamp := range dtlsrb.peers {
		if err := cboring.Marshal(&peerID, w); err != nil {
			return err
		}
		if err := cboring.WriteUInt(timestamp, w); err != nil {
			return err
		}
	}

	return nil
}

func (dtlsrb *DTLSRBlock) UnmarshalCbor(r io.Reader) error {
	// read the (apparently) required outer array
	if l, err := cboring.ReadArrayLength(r); err != nil {
		return err
	} else if l != 3 {
		return fmt.Errorf("expected 3 fields, got %d", l)
	}

	// read endpoint id
	id := bundle.EndpointID{}
	if err := cboring.Unmarshal(&id, r); err != nil {
		return err
	} else {
		dtlsrb.id = id
	}

	// read the timestamp
	if timestamp, err := cboring.ReadUInt(r); err != nil {
		return err
	} else {
		dtlsrb.timestamp = timestamp
	}

	var lenData uint64

	// read length of data array
	lenData, err := cboring.ReadArrayLength(r)
	if err != nil {
		return err
	}

	// read the actual data
	peers := make(map[bundle.EndpointID]uint64)
	var i uint64
	for i = 0; i < lenData; i++ {
		peerID := bundle.EndpointID{}
		if err := cboring.Unmarshal(&peerID, r); err != nil {
			return err
		}

		timestamp, err := cboring.ReadUInt(r)
		if err != nil {
			return err
		}

		peers[peerID] = timestamp
	}

	dtlsrb.peers = peers

	return nil
}
