package consensusmgr

import (
	"reflect"

	"github.com/sirupsen/logrus"

	"github.com/vapor/event"
	"github.com/vapor/netsync/peers"
	"github.com/vapor/p2p"
	"github.com/vapor/p2p/security"
	"github.com/vapor/protocol/bc"
	"github.com/vapor/protocol/bc/types"
)

// Switch is the interface for p2p switch.
type Switch interface {
	AddReactor(name string, reactor p2p.Reactor) p2p.Reactor
}

// Chain is the interface for Bytom core.
type Chain interface {
	BestBlockHeight() uint64
	GetHeaderByHash(*bc.Hash) (*types.BlockHeader, error)
	ProcessBlock(*types.Block) (bool, error)
	ProcessBlockSignature(signature []byte, pubkey [64]byte, blockHash *bc.Hash) error
}

type blockMsg struct {
	block  *types.Block
	peerID string
}

// Manager is the consensus message network synchronization manager.
type Manager struct {
	sw              Switch
	chain           Chain
	peers           *peers.PeerSet
	blockFetcher    *blockFetcher
	eventDispatcher *event.Dispatcher

	quit chan struct{}
}

// NewManager create new manager.
func NewManager(sw Switch, chain Chain, dispatcher *event.Dispatcher, peers *peers.PeerSet) *Manager {
	manager := &Manager{
		sw:              sw,
		chain:           chain,
		peers:           peers,
		blockFetcher:    newBlockFetcher(chain, peers),
		eventDispatcher: dispatcher,
		quit:            make(chan struct{}),
	}
	protocolReactor := NewConsensusReactor(manager)
	manager.sw.AddReactor("CONSENSUS", protocolReactor)
	return manager
}

func (m *Manager) addPeer(peer peers.BasePeer) {
	m.peers.AddPeer(peer)
}

func (m *Manager) processMsg(peerID string, msgType byte, msg ConsensusMessage) {
	peer := m.peers.GetPeer(peerID)
	if peer == nil {
		return
	}

	logrus.WithFields(logrus.Fields{"module": logModule, "peer": peerID, "type": reflect.TypeOf(msg), "message": msg.String()}).Info("receive message from peer")

	switch msg := msg.(type) {
	case *BlockProposeMsg:
		m.handleBlockProposeMsg(peerID, msg)

	case *BlockSignatureMsg:
		m.handleBlockSignatureMsg(peerID, msg)

	default:
		logrus.WithFields(logrus.Fields{"module": logModule, "peer": peerID, "message_type": reflect.TypeOf(msg)}).Error("unhandled message type")
	}
}

func (m *Manager) handleBlockProposeMsg(peerID string, msg *BlockProposeMsg) {
	block, err := msg.GetProposeBlock()
	if err != nil {
		logrus.WithFields(logrus.Fields{"module": logModule, "err": err}).Warning("failed on get propose block")
		return
	}

	hash := block.Hash()
	m.peers.MarkBlock(peerID, &hash)
	m.blockFetcher.processNewBlock(&blockMsg{peerID: peerID, block: block})
	m.peers.SetStatus(peerID, block.Height, &hash)
}

func (m *Manager) handleBlockSignatureMsg(peerID string, msg *BlockSignatureMsg) {
	blockHash := bc.NewHash(msg.BlockHash)
	if err := m.chain.ProcessBlockSignature(msg.Signature, msg.PubKey, &blockHash); err != nil {
		m.peers.ProcessIllegal(peerID, security.LevelMsgIllegal, err.Error())
		return
	}
	m.peers.MarkBlockSignature(peerID, msg.Signature)
}

func (m *Manager) blockProposeMsgBroadcastLoop() {
	blockProposeMsgSub, err := m.eventDispatcher.Subscribe(event.NewProposedBlockEvent{})
	if err != nil {
		logrus.WithFields(logrus.Fields{"module": logModule, "err": err}).Error("failed on subscribe NewBlockProposeEvent")
		return
	}
	defer blockProposeMsgSub.Unsubscribe()

	for {
		select {
		case obj, ok := <-blockProposeMsgSub.Chan():
			if !ok {
				logrus.WithFields(logrus.Fields{"module": logModule}).Warning("blockProposeMsgSub channel closed")
				return
			}

			ev, ok := obj.Data.(event.NewProposedBlockEvent)
			if !ok {
				logrus.WithFields(logrus.Fields{"module": logModule}).Error("event type error")
				continue
			}
			proposeMsg, err := NewBlockProposeMsg(&ev.Block)
			if err != nil {
				logrus.WithFields(logrus.Fields{"module": logModule, "err": err}).Error("failed on create BlockProposeMsg")
				return
			}

			if err := m.peers.BroadcastMsg(NewBroadcastMsg(proposeMsg, consensusChannel)); err != nil {
				logrus.WithFields(logrus.Fields{"module": logModule, "err": err}).Error("failed on broadcast BlockProposeBroadcastMsg")
				continue
			}

		case <-m.quit:
			return
		}
	}
}

func (m *Manager) blockSignatureMsgBroadcastLoop() {
	blockSignatureMsgSub, err := m.eventDispatcher.Subscribe(event.BlockSignatureEvent{})
	if err != nil {
		logrus.WithFields(logrus.Fields{"module": logModule, "err": err}).Error("failed on subscribe BlockSignatureEvent")
		return
	}
	defer blockSignatureMsgSub.Unsubscribe()
	for {
		select {
		case obj, ok := <-blockSignatureMsgSub.Chan():
			if !ok {
				logrus.WithFields(logrus.Fields{"module": logModule}).Warning("blockProposeMsgSub channel closed")
				return
			}

			ev, ok := obj.Data.(event.BlockSignatureEvent)
			if !ok {
				logrus.WithFields(logrus.Fields{"module": logModule}).Error("event type error")
				continue
			}

			blockHeader, err := m.chain.GetHeaderByHash(&ev.BlockHash)
			if err != nil {
				logrus.WithFields(logrus.Fields{"module": logModule, "err": err}).Error("failed on get header by hash from chain.")
				return
			}

			blockSignatureMsg := NewBroadcastMsg(NewBlockSignatureMsg(ev.BlockHash, blockHeader.Height, ev.Signature, ev.XPub), consensusChannel)
			if err := m.peers.BroadcastMsg(blockSignatureMsg); err != nil {
				logrus.WithFields(logrus.Fields{"module": logModule, "err": err}).Error("failed on broadcast BlockSignBroadcastMsg.")
				return
			}

		case <-m.quit:
			return
		}
	}
}

func (m *Manager) removePeer(peerID string) {
	m.peers.RemovePeer(peerID)
}

//Start consensus manager service.
func (m *Manager) Start() error {
	go m.blockProposeMsgBroadcastLoop()
	go m.blockSignatureMsgBroadcastLoop()
	return nil
}

//Stop consensus manager service.
func (m *Manager) Stop() {
	close(m.quit)
}
