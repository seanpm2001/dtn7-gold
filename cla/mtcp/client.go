package mtcp

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/dtn7/cboring"
	"github.com/dtn7/dtn7-go/bundle"
)

// MTCPClient is an implementation of a Minimal TCP Convergence-Layer client
// which connects to a MTCP server to send bundles.
type MTCPClient struct {
	conn  net.Conn
	peer  bundle.EndpointID
	mutex sync.Mutex

	address   string
	permanent bool
	keepalive time.Duration

	stopSyn chan struct{}
	stopAck chan struct{}
}

// NewMTCPClient creates a new MTCPClient, connected to the given address for
// the registered endpoint ID. The permanent flag indicates if this MTCPClient
// should never be removed from the core.
func NewMTCPClient(address string, peer bundle.EndpointID, permanent bool) *MTCPClient {
	return &MTCPClient{
		peer:      peer,
		address:   address,
		permanent: permanent,
		keepalive: time.Minute,
		stopSyn:   make(chan struct{}),
		stopAck:   make(chan struct{}),
	}
}

// NewMTCPClient creates a new MTCPClient, connected to the given address. The
// permanent flag indicates if this MTCPClient should never be removed from
// the core.
func NewAnonymousMTCPClient(address string, permanent bool) *MTCPClient {
	return NewMTCPClient(address, bundle.DtnNone(), permanent)
}

// SetKeepalive defines the interval between keepalive probes will be sent to
// check the connection. A good value depends on the link. If nothing is
// configured, the default keepalive period is one minute.
func (client *MTCPClient) SetKeepalive(keepalive time.Duration) {
	client.keepalive = keepalive
}

// Start starts this MTCPClient and might return an error and a boolean
// indicating if another Start should be tried later.
func (client *MTCPClient) Start() (error, bool) {
	conn, err := net.DialTimeout("tcp", client.address, time.Second)
	if err == nil {
		client.conn = conn

		go client.keepaliveProbe()
	}

	return err, true
}

func (client *MTCPClient) keepaliveProbe() {
	for {
		select {
		case <-client.stopSyn:
			client.conn.Close()
			close(client.stopAck)

			return

		case <-time.After(client.keepalive):
			client.mutex.Lock()
			err := cboring.WriteByteStringLen(0, client.conn)
			client.mutex.Unlock()

			if err != nil {
				log.WithFields(log.Fields{
					"error":  err,
					"client": client.String(),
				}).Warn("MTCPClient: Keepalive errored")

				client.conn.Close()
				close(client.stopAck)

				return
			}
		}
	}
}

// Send transmits a bundle to this MTCPClient's endpoint.
func (client *MTCPClient) Send(bndl *bundle.Bundle) (err error) {
	defer func() {
		if r := recover(); r != nil && err == nil {
			err = fmt.Errorf("MTCPClient.Send: %v", r)
		}
	}()

	client.mutex.Lock()
	defer client.mutex.Unlock()

	connWriter := bufio.NewWriter(client.conn)

	buff := new(bytes.Buffer)
	if cborErr := cboring.Marshal(bndl, buff); cborErr != nil {
		err = cborErr
		return
	}

	if bsErr := cboring.WriteByteStringLen(uint64(buff.Len()), connWriter); bsErr != nil {
		err = bsErr
		return
	}

	if _, plErr := buff.WriteTo(connWriter); plErr != nil {
		err = plErr
		return
	}

	if flushErr := connWriter.Flush(); flushErr != nil {
		err = flushErr
		return
	}

	return
}

// Close closes the MTCPClient's connection.
func (client *MTCPClient) Close() {
	close(client.stopSyn)
	<-client.stopAck
}

// GetPeerEndpointID returns the endpoint ID assigned to this CLA's peer,
// if it's known. Otherwise the zero endpoint will be returned.
func (client *MTCPClient) GetPeerEndpointID() bundle.EndpointID {
	return client.peer
}

// Address should return a unique address string to both identify this
// ConvergenceSender and ensure it will not opened twice.
func (client *MTCPClient) Address() string {
	return client.address
}

// IsPermanent returns true, if this CLA should not be removed after failures.
func (client *MTCPClient) IsPermanent() bool {
	return client.permanent
}

func (client *MTCPClient) String() string {
	if client.conn != nil {
		return fmt.Sprintf("mtcp://%v", client.conn.RemoteAddr())
	} else {
		return fmt.Sprintf("mtcp://%s", client.address)
	}
}
