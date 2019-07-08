package state

import (
	"encoding/hex"
	"sort"

	"github.com/vapor/common/arithmetic"
	"github.com/vapor/config"
	"github.com/vapor/consensus"
	"github.com/vapor/crypto/ed25519/chainkd"
	"github.com/vapor/errors"
	"github.com/vapor/math/checked"
	"github.com/vapor/protocol/bc"
	"github.com/vapor/protocol/bc/types"
)

// ConsensusNode represents a consensus node
type ConsensusNode struct {
	XPub    chainkd.XPub
	VoteNum uint64
	Order   uint64
}

type byVote []*ConsensusNode

func (c byVote) Len() int { return len(c) }
func (c byVote) Less(i, j int) bool {
	return c[i].VoteNum > c[j].VoteNum || (c[i].VoteNum == c[j].VoteNum && c[i].XPub.String() > c[j].XPub.String())
}
func (c byVote) Swap(i, j int) { c[i], c[j] = c[j], c[i] }

// CalcVoteSeq calculate the vote sequence
// seq 0 is the genesis block
// seq 1 is the the block height 1, to block height RoundVoteBlockNums
// seq 2 is the block height RoundVoteBlockNums + 1 to block height 2 * RoundVoteBlockNums
// consensus node of the current round is the final result of previous round
func CalcVoteSeq(blockHeight uint64) uint64 {
	if blockHeight == 0 {
		return 0
	}
	return (blockHeight-1)/consensus.RoundVoteBlockNums + 1
}

// ConsensusResult represents a snapshot of each round of DPOS voting
// Seq indicates the sequence of current votes, which start from zero
// NumOfVote indicates the number of votes each consensus node receives, the key of map represent public key
// CoinbaseReward indicates the coinbase receiver and reward
type ConsensusResult struct {
	Seq            uint64
	NumOfVote      map[string]uint64
	CoinbaseReward map[string]uint64
	BlockHash      bc.Hash
	BlockHeight    uint64
}

// CoinbaseReward contains receiver and reward
type CoinbaseReward struct {
	Amount         uint64
	ControlProgram []byte
}

// SortByAmount implements sort.Interface for CoinbaseReward slices
type SortByAmount []CoinbaseReward

func (a SortByAmount) Len() int           { return len(a) }
func (a SortByAmount) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a SortByAmount) Less(i, j int) bool { return a[i].Amount < a[j].Amount }

// CalCoinbaseReward calculate the coinbase reward for block
func CalCoinbaseReward(block *types.Block) (*CoinbaseReward, error) {
	var coinbaseReceiver []byte
	if len(block.Transactions) > 0 && len(block.Transactions[0].Outputs) > 0 {
		coinbaseReceiver = block.Transactions[0].Outputs[0].ControlProgram()
	}

	if coinbaseReceiver == nil {
		return nil, errors.New("not found coinbase receiver")
	}

	coinbaseAmount := consensus.BlockSubsidy(block.BlockHeader.Height)
	for _, tx := range block.Transactions {
		txFee, err := arithmetic.CalculateTxFee(tx)
		if err != nil {
			return nil, errors.Wrap(checked.ErrOverflow, "calculate transaction fee")
		}
		coinbaseAmount += txFee
	}

	return &CoinbaseReward{
		Amount:         coinbaseAmount,
		ControlProgram: coinbaseReceiver,
	}, nil
}

// ApplyBlock calculate the consensus result for new block
func (c *ConsensusResult) ApplyBlock(block *types.Block) error {
	if c.BlockHash != block.PreviousBlockHash {
		return errors.New("block parent hash is not equals last block hash of vote result")
	}

	if err := c.AttachCoinbaseReward(block); err != nil {
		return err
	}

	for _, tx := range block.Transactions {
		for _, input := range tx.Inputs {
			vetoInput, ok := input.TypedInput.(*types.VetoInput)
			if !ok {
				continue
			}

			pubkey := hex.EncodeToString(vetoInput.Vote)
			c.NumOfVote[pubkey], ok = checked.SubUint64(c.NumOfVote[pubkey], vetoInput.Amount)
			if !ok {
				return checked.ErrOverflow
			}

			if c.NumOfVote[pubkey] == 0 {
				delete(c.NumOfVote, pubkey)
			}
		}

		for _, output := range tx.Outputs {
			voteOutput, ok := output.TypedOutput.(*types.VoteOutput)
			if !ok {
				continue
			}

			pubkey := hex.EncodeToString(voteOutput.Vote)
			if c.NumOfVote[pubkey], ok = checked.AddUint64(c.NumOfVote[pubkey], voteOutput.Amount); !ok {
				return checked.ErrOverflow
			}
		}
	}

	c.BlockHash = block.Hash()
	c.BlockHeight = block.Height
	c.Seq = CalcVoteSeq(block.Height)
	return nil
}

// ConsensusNodes returns all consensus nodes
func (c *ConsensusResult) ConsensusNodes() (map[string]*ConsensusNode, error) {
	var nodes []*ConsensusNode
	for pubkey, voteNum := range c.NumOfVote {
		if voteNum >= consensus.MinConsensusNodeVoteNum {
			var xpub chainkd.XPub
			if err := xpub.UnmarshalText([]byte(pubkey)); err != nil {
				return nil, err
			}

			nodes = append(nodes, &ConsensusNode{XPub: xpub, VoteNum: voteNum})
		}
	}
	// In principle, there is no need to sort all voting nodes.
	// if there is a performance problem, consider the optimization later.
	sort.Sort(byVote(nodes))
	result := make(map[string]*ConsensusNode)
	for i := 0; i < len(nodes) && i < consensus.NumOfConsensusNode; i++ {
		nodes[i].Order = uint64(i)
		result[nodes[i].XPub.String()] = nodes[i]
	}

	if len(result) != 0 {
		return result, nil
	}
	return federationNodes(), nil
}

func federationNodes() map[string]*ConsensusNode {
	consensusResult := map[string]*ConsensusNode{}
	for i, xpub := range config.CommonConfig.Federation.Xpubs {
		consensusResult[xpub.String()] = &ConsensusNode{XPub: xpub, VoteNum: 0, Order: uint64(i)}
	}
	return consensusResult
}

// DetachBlock calculate the consensus result for detach block
func (c *ConsensusResult) DetachBlock(block *types.Block) error {
	if c.BlockHash != block.Hash() {
		return errors.New("block hash is not equals last block hash of vote result")
	}

	if err := c.DetachCoinbaseReward(block); err != nil {
		return err
	}

	for i := len(block.Transactions) - 1; i >= 0; i-- {
		tx := block.Transactions[i]
		for _, input := range tx.Inputs {
			vetoInput, ok := input.TypedInput.(*types.VetoInput)
			if !ok {
				continue
			}

			pubkey := hex.EncodeToString(vetoInput.Vote)
			if c.NumOfVote[pubkey], ok = checked.AddUint64(c.NumOfVote[pubkey], vetoInput.Amount); !ok {
				return checked.ErrOverflow
			}
		}

		for _, output := range tx.Outputs {
			voteOutput, ok := output.TypedOutput.(*types.VoteOutput)
			if !ok {
				continue
			}

			pubkey := hex.EncodeToString(voteOutput.Vote)
			c.NumOfVote[pubkey], ok = checked.SubUint64(c.NumOfVote[pubkey], voteOutput.Amount)
			if !ok {
				return checked.ErrOverflow
			}

			if c.NumOfVote[pubkey] == 0 {
				delete(c.NumOfVote, pubkey)
			}
		}
	}

	c.BlockHash = block.PreviousBlockHash
	c.BlockHeight = block.Height - 1
	c.Seq = CalcVoteSeq(block.Height - 1)
	return nil
}

func (c *ConsensusResult) Fork() *ConsensusResult {
	f := &ConsensusResult{
		Seq:            c.Seq,
		NumOfVote:      map[string]uint64{},
		CoinbaseReward: map[string]uint64{},
		BlockHash:      c.BlockHash,
		BlockHeight:    c.BlockHeight,
	}

	for key, value := range c.NumOfVote {
		f.NumOfVote[key] = value
	}

	for key, value := range c.CoinbaseReward {
		f.CoinbaseReward[key] = value
	}
	return f
}

func (c *ConsensusResult) IsFinalize() bool {
	return c.BlockHeight%consensus.RoundVoteBlockNums == 0
}

// AttachCoinbaseReward attach coinbase reward
func (c *ConsensusResult) AttachCoinbaseReward(block *types.Block) error {
	reward, err := CalCoinbaseReward(block)
	if err != nil {
		return err
	}

	if block.Height%consensus.RoundVoteBlockNums == 1 {
		c.CoinbaseReward = map[string]uint64{}
	}

	var ok bool
	program := hex.EncodeToString(reward.ControlProgram)
	c.CoinbaseReward[program], ok = checked.AddUint64(c.CoinbaseReward[program], reward.Amount)
	if !ok {
		return checked.ErrOverflow
	}
	return nil
}

// DetachCoinbaseReward detach coinbase reward
func (c *ConsensusResult) DetachCoinbaseReward(block *types.Block) error {
	if block.Height%consensus.RoundVoteBlockNums == 0 {
		for i, output := range block.Transactions[0].Outputs {
			if i == 0 {
				continue
			}
			program := output.ControlProgram()
			c.CoinbaseReward[hex.EncodeToString(program)] = output.AssetAmount().Amount
		}
	}

	reward, err := CalCoinbaseReward(block)
	if err != nil {
		return err
	}

	var ok bool
	program := hex.EncodeToString(reward.ControlProgram)
	if c.CoinbaseReward[program], ok = checked.SubUint64(c.CoinbaseReward[program], reward.Amount); !ok {
		return checked.ErrOverflow
	}

	if c.CoinbaseReward[program] == 0 {
		delete(c.CoinbaseReward, program)
	}
	return nil
}

// GetCoinbaseRewards convert into CoinbaseReward array and sort it by amount
func (c *ConsensusResult) GetCoinbaseRewards(blockHeight uint64) ([]CoinbaseReward, error) {
	rewards := []CoinbaseReward{}
	if blockHeight%consensus.RoundVoteBlockNums != 0 {
		return rewards, nil
	}

	for p, amount := range c.CoinbaseReward {
		coinbaseAmount := amount
		program, err := hex.DecodeString(p)
		if err != nil {
			return nil, err
		}

		rewards = append(rewards, CoinbaseReward{
			Amount:         coinbaseAmount,
			ControlProgram: program,
		})
	}
	sort.Sort(SortByAmount(rewards))
	return rewards, nil
}