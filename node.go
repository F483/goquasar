package quasar

import (
	"github.com/f483/dejavu"
	"io"
	"math/rand"
	"sync"
	"time"
)

// Node holds the quasar pubsup state
type Node struct {
	net               networkOverlay
	subscribers       map[hash160digest][]io.Writer
	topics            map[hash160digest][]byte
	mutex             *sync.RWMutex
	peers             map[pubkey]*peerData
	log               *Logger
	history           dejavu.DejaVu // memory of past events
	cfg               *Config
	stopDispatcher    chan bool
	stopPropagation   chan bool
	stopExpiredPeerGC chan bool
}

// New create instance with the sane defaults.
func New() *Node {
	// TODO pass node id/pubkey and peers list
	return newNode(nil, nil, &StandardConfig)
}

// NewCustom create instance with custom logging/setup, for testing only.
func NewCustom(l *Logger, c *Config) *Node {
	return newNode(nil, l, c)
}

func newNode(n networkOverlay, l *Logger, c *Config) *Node {
	d := dejavu.NewProbabilistic(c.HistoryLimit, c.HistoryAccuracy)
	return &Node{
		net:               n,
		subscribers:       make(map[hash160digest][]io.Writer),
		topics:            make(map[hash160digest][]byte),
		mutex:             new(sync.RWMutex),
		peers:             make(map[pubkey]*peerData),
		log:               l,
		history:           d,
		cfg:               c,
		stopDispatcher:    nil, // set on Start() call
		stopPropagation:   nil, // set on Start() call
		stopExpiredPeerGC: nil, // set on Start() call
	}
}

func (n *Node) processUpdate(u *peerUpdate) {
	go n.log.updateReceived(n, u)
	if n.net.isConnected(u.peer) == false {
		go n.log.updateFail(n, u)
		return // ignore to prevent memory attack
	}

	n.mutex.Lock()
	data, ok := n.peers[*u.peer]

	if !ok { // init if doesnt exist
		depth := n.cfg.FiltersDepth
		data = &peerData{
			filters:    newFilters(n.cfg),
			timestamps: make([]uint64, depth, depth),
		}
		n.peers[*u.peer] = data
	}

	// update peer data
	data.filters[u.index] = u.filter
	data.timestamps[u.index] = makePeerTimestamp()
	n.mutex.Unlock()
	go n.log.updateSuccess(n, u)
}

// Publish a message on the network for given topic.
func (n *Node) Publish(topic []byte, message []byte) {
	// TODO validate input
	event := newEvent(topic, message, n.cfg.DefaultEventTTL)
	go n.log.eventPublished(n, event)
	go n.route(event)
}

func (n *Node) isDuplicate(e *event) bool {
	return n.history.Witness(append(e.topicDigest[:20], e.message...))
}

func (n *Node) deliver(receivers []io.Writer, e *event) {
	for _, receiver := range receivers {
		receiver.Write(e.message)
	}
}

func (n *Node) subscriptions() []hash160digest {
	digests := make([]hash160digest, 0)
	for digest := range n.subscribers {
		digests = append(digests, digest)
	}
	return digests
}

// Algorithm 1 from the quasar paper.
func (n *Node) sendUpdates() {
	n.mutex.RLock()
	filters := newFilters(n.cfg)
	pubkey := n.net.id()
	digests := n.subscriptions()
	digests = append(digests, hash160(pubkey[:]))
	filters[0] = newFilterFromDigests(n.cfg, digests...)
	for _, data := range n.peers {
		// XXX better if only expiredPeerGC takes care of it?
		// if peerDataExpired(data, n.cfg) {
		// 		continue
		// 	}
		for i := 1; uint32(i) < n.cfg.FiltersDepth; i++ {
			size := int(n.cfg.FiltersM / 8)
			for j := 0; j < size; j++ { // inline merge for performance
				filters[i][j] = filters[i][j] | data.filters[i-1][j]
			}
		}
	}
	for _, id := range n.net.connectedPeers() {
		for i := 0; uint32(i) < (n.cfg.FiltersDepth - 1); i++ {
			// top filter never sent as not used by peers
			go n.net.sendUpdate(id, uint32(i), filters[i])
			go n.log.updateSent(n, uint32(i), filters[i], id)
		}
	}
	n.mutex.RUnlock()
}

// Algorithm 2 from the quasar paper.
func (n *Node) route(e *event) {
	n.mutex.RLock()
	id := n.net.id()
	if n.isDuplicate(e) {
		go n.log.eventDropDuplicate(n, e)
		n.mutex.RUnlock()
		return
	}
	if receivers, ok := n.subscribers[*e.topicDigest]; ok {
		n.log.eventDeliver(n, e)
		n.deliver(receivers, e)
		e.publishers = append(e.publishers, id)
		for _, peerId := range n.net.connectedPeers() {
			go n.net.sendEvent(peerId, e)
			go n.log.eventRouteDirect(n, e, peerId)
		}
		n.mutex.RUnlock()
		return
	}
	e.ttl -= 1
	if e.ttl == 0 {
		go n.log.eventDropTTL(n, e)
		n.mutex.RUnlock()
		return
	}
	for i := 0; uint32(i) < n.cfg.FiltersDepth; i++ {
		for peerId, data := range n.peers {
			f := data.filters[i]
			if filterContainsDigest(f, n.cfg, *e.topicDigest) {
				negRt := false
				for _, publisher := range e.publishers {
					if filterContains(f, n.cfg, publisher[:]) {
						negRt = true
					}
				}
				if !negRt {
					go n.net.sendEvent(&peerId, e)
					go n.log.eventRouteWell(n, e, &peerId)
					n.mutex.RUnlock()
					return
				}
			}
		}
	}
	peerId := n.randomPeer()
	if peerId != nil {
		go n.net.sendEvent(peerId, e)
		go n.log.eventRouteRandom(n, e, peerId)
	}
	n.mutex.RUnlock()
}

func (n *Node) randomPeer() *pubkey {
	peers := n.net.connectedPeers()
	if len(peers) == 0 {
		return nil
	}
	return peers[rand.Intn(len(peers))]
}

func (n *Node) dispatchInput() {
	for {
		select {
		case peerUpdate := <-n.net.receivedUpdateChannel():
			if validUpdate(peerUpdate, n.cfg) {
				go n.processUpdate(peerUpdate)
			}
		case event := <-n.net.receivedEventChannel():
			if validEvent(event) {
				go n.log.eventReceived(n, event)
				go n.route(event)
			}
		case <-n.stopDispatcher:
			return
		}
	}
}

func (n *Node) removeExpiredPeers() {
	n.mutex.Lock()
	toRemove := []*pubkey{}
	for peerId, data := range n.peers {
		if peerDataExpired(data, n.cfg) {
			toRemove = append(toRemove, &peerId)
		}
	}
	for _, peerId := range toRemove {
		delete(n.peers, *peerId)
	}
	n.mutex.Unlock()
}

func (n *Node) expiredPeerGC() {
	delay := time.Duration(n.cfg.FilterFreshness/2) * time.Millisecond
	for {
		select {
		case <-time.After(delay):
			go n.removeExpiredPeers()
		case <-n.stopExpiredPeerGC:
			return
		}
	}
}

func (n *Node) propagateFilters() {
	delay := time.Duration(n.cfg.PropagationDelay) * time.Millisecond
	for {
		select {
		case <-time.After(delay):
			go n.sendUpdates()
		case <-n.stopPropagation:
			return
		}
	}
}

// Start quasar system
func (n *Node) Start() {
	n.net.start()
	n.stopDispatcher = make(chan bool)
	n.stopPropagation = make(chan bool)
	n.stopExpiredPeerGC = make(chan bool)
	go n.dispatchInput()
	go n.propagateFilters()
	go n.expiredPeerGC()
}

// Stop quasar system
func (n *Node) Stop() {
	n.net.stop()
	n.stopDispatcher <- true
	n.stopPropagation <- true
	n.stopExpiredPeerGC <- true
}

// Subscribe provided message receiver channel to given topic.
func (n *Node) Subscribe(topic []byte, receiver io.Writer) {
	// TODO validate input
	digest := hash160(topic)
	n.mutex.Lock()
	receivers, ok := n.subscribers[digest]
	if ok != true { // new subscription
		n.subscribers[digest] = []io.Writer{receiver}
		n.topics[digest] = topic
	} else { // append to existing subscribers
		n.subscribers[digest] = append(receivers, receiver)
	}
	n.mutex.Unlock()
}

// Unsubscribe message receiver channel from topic. If nil receiver
// channel is provided all message receiver channels for given topic
// will be removed.
func (n *Node) Unsubscribe(topic []byte, receiver io.Writer) {
	// TODO validate input

	digest := hash160(topic)
	n.mutex.Lock()
	receivers, ok := n.subscribers[digest]

	// remove specific message receiver
	if ok && receiver != nil {
		for i, v := range receivers {
			if v == receiver {
				receivers = append(receivers[:i], receivers[i+1:]...)
				n.subscribers[digest] = receivers
				break
			}
		}
	}

	// remove sub key if no specific message
	// receiver provided or no message receiver remaining
	if ok && (receiver == nil || len(n.subscribers[digest]) == 0) {
		delete(n.subscribers, digest)
		delete(n.topics, digest)
	}
	n.mutex.Unlock()
}

// Subscribers retruns message receivers for given topic.
func (n *Node) Subscribers(topic []byte) []io.Writer {
	// TODO validate input
	digest := hash160(topic)
	results := []io.Writer{}
	n.mutex.RLock()
	if receivers, ok := n.subscribers[digest]; ok {
		results = append(results, receivers...)
	}
	n.mutex.RUnlock()
	return results
}

// Subscriptions retruns a slice of currently subscribed topics.
func (n *Node) Subscriptions() [][]byte {
	n.mutex.RLock()
	topics := make([][]byte, len(n.topics))
	i := 0
	for _, topic := range n.topics {
		topics[i] = topic
		i++
	}
	n.mutex.RUnlock()
	return topics
}
