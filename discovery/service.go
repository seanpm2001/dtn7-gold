package discovery

import (
	"fmt"
	"log"
	"time"

	"github.com/schollz/peerdiscovery"
)

// DiscoveryService is a type to publish the node's CLAs to its network while
// discovering new peers. Internally UDP mulitcast packets are used.
type DiscoveryService struct {
	stopChan4 chan struct{}
	stopChan6 chan struct{}
}

func (ds *DiscoveryService) notify(discovered peerdiscovery.Discovered) {
	dms, err := NewDiscoveryMessagesFromCbor(discovered.Payload)
	if err != nil {
		log.Printf("Peer discovery failed to parse incomming package from %v: %v",
			discovered.Address, err)

		return
	}

	log.Printf("Peer discovery discovered %v at %v", dms, discovered.Address)
}

// Close shuts the DiscoveryService down.
func (ds *DiscoveryService) Close() {
	if ds.stopChan4 != nil {
		ds.stopChan4 <- struct{}{}
	}

	if ds.stopChan6 != nil {
		ds.stopChan6 <- struct{}{}
	}
}

// NewDiscoveryService starts a new DiscoveryService and promotes the given
// DiscoveryMessages through IPv4 and/or IPv6, as specified in the parameters.
// Furthermore, received DiscoveryMessages will be processed.
func NewDiscoveryService(dms []DiscoveryMessage, ipv4, ipv6 bool) (*DiscoveryService, error) {
	var ds = &DiscoveryService{}

	if ipv4 {
		ds.stopChan4 = make(chan struct{})
	}

	if ipv6 {
		ds.stopChan6 = make(chan struct{})
	}

	msg, err := DiscoveryMessagesToCbor(dms)
	if err != nil {
		return nil, err
	}

	sets := []struct {
		active           bool
		multicastAddress string
		stopChan         chan struct{}
		ipVersion        peerdiscovery.IPVersion
	}{
		{ipv4, DiscoveryAddress4, ds.stopChan4, peerdiscovery.IPv4},
		{ipv6, DiscoveryAddress6, ds.stopChan6, peerdiscovery.IPv6},
	}

	for _, set := range sets {
		if !set.active {
			continue
		}

		set := peerdiscovery.Settings{
			Limit:            -1,
			Port:             fmt.Sprintf("%d", DiscoveryPort),
			MulticastAddress: set.multicastAddress,
			Payload:          msg,
			Delay:            10 * time.Second,
			TimeLimit:        -1,
			StopChan:         set.stopChan,
			AllowSelf:        true,
			IPVersion:        set.ipVersion,
			Notify:           ds.notify,
		}

		go peerdiscovery.Discover(set)
	}

	return ds, nil
}
