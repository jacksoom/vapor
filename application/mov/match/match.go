package match

import (
	"encoding/hex"
	"math"

	"github.com/vapor/application/mov/common"
	"github.com/vapor/application/mov/contract"
	"github.com/vapor/application/mov/database"
	"github.com/vapor/consensus/segwit"
	"github.com/vapor/errors"
	vprMath "github.com/vapor/math"
	"github.com/vapor/protocol/bc"
	"github.com/vapor/protocol/bc/types"
	"github.com/vapor/protocol/vm"
	"github.com/vapor/protocol/vm/vmutil"
)

const maxFeeRate = 0.05

type Engine struct {
	orderTable  *OrderTable
	nodeProgram []byte
}

func NewEngine(movStore database.MovStore, nodeProgram []byte) *Engine {
	return &Engine{orderTable: NewOrderTable(movStore), nodeProgram: nodeProgram}
}

func (e *Engine) HasMatchedTx(tradePairs ...*common.TradePair) bool {
	if err := validateTradePairs(tradePairs); err != nil {
		return false
	}

	orders := e.peekOrders(tradePairs)
	if len(orders) == 0 {
		return false
	}

	for i, order := range orders {
		if canNotBeMatched(order, orders[getOppositeIndex(len(orders), i)]) {
			return false
		}
	}
	return true
}

// NextMatchedTx return the next matchable transaction by the specified trade pairs
// the size of trade pairs at least 2, and the sequence of trade pairs can form a loop
// for example, [assetA -> assetB, assetB -> assetC, assetC -> assetA]
func (e *Engine) NextMatchedTx(tradePairs ...*common.TradePair) (*types.Tx, error) {
	if err := validateTradePairs(tradePairs); err != nil {
		return nil, err
	}

	orders := e.peekOrders(tradePairs)
	if len(orders) == 0 {
		return nil, errors.New("no order for the specified trade pair in the order table")
	}

	tx, err := e.buildMatchTx(orders)
	if err != nil {
		return nil, err
	}

	for _, tradePair := range tradePairs {
		e.orderTable.PopOrder(tradePair)
	}

	if err := addPartialTradeOrder(tx, e.orderTable); err != nil {
		return nil, err
	}
	return tx, nil
}

func (e *Engine) peekOrders(tradePairs []*common.TradePair) []*common.Order {
	var orders []*common.Order
	for _, tradePair := range tradePairs {
		order := e.orderTable.PeekOrder(tradePair)
		if order == nil {
			return nil
		}

		orders = append(orders, order)
	}
	return orders
}

func validateTradePairs(tradePairs []*common.TradePair) error {
	if len(tradePairs) < 2 {
		return errors.New("size of trade pairs at least 2")
	}

	for i, tradePair := range tradePairs {
		oppositeTradePair := tradePairs[getOppositeIndex(len(tradePairs), i)]
		if *tradePair.ToAssetID != *oppositeTradePair.FromAssetID {
			return errors.New("specified trade pairs is invalid")
		}
	}
	return nil
}

func canNotBeMatched(order, oppositeOrder *common.Order) bool {
	rate := 1 / order.Rate
	return rate < oppositeOrder.Rate
}

func (e *Engine) buildMatchTx(orders []*common.Order) (*types.Tx, error) {
	txData := &types.TxData{Version: 1}
	for i, order := range orders {
		input := types.NewSpendInput(nil, *order.Utxo.SourceID, *order.FromAssetID, order.Utxo.Amount, order.Utxo.SourcePos, order.Utxo.ControlProgram)
		txData.Inputs = append(txData.Inputs, input)

		oppositeOrder := orders[getOppositeIndex(len(orders), i)]
		if err := addMatchTxOutput(txData, input, order, oppositeOrder.Utxo.Amount); err != nil {
			return nil, err
		}
	}

	if err := e.addMatchTxFeeOutput(txData); err != nil {
		return nil, err
	}

	byteData, err := txData.MarshalText()
	if err != nil {
		return nil, err
	}

	txData.SerializedSize = uint64(len(byteData))
	return types.NewTx(*txData), nil
}

func addMatchTxOutput(txData *types.TxData, txInput *types.TxInput, order *common.Order, oppositeAmount uint64) error {
	contractArgs, err := segwit.DecodeP2WMCProgram(order.Utxo.ControlProgram)
	if err != nil {
		return err
	}

	requestAmount := calcRequestAmount(order.Utxo.Amount, contractArgs)
	receiveAmount := vprMath.MinUint64(requestAmount, oppositeAmount)
	shouldPayAmount := CalcShouldPayAmount(receiveAmount, contractArgs)
	isPartialTrade := order.Utxo.Amount > shouldPayAmount

	setMatchTxArguments(txInput, isPartialTrade, len(txData.Outputs), receiveAmount)
	txData.Outputs = append(txData.Outputs, types.NewIntraChainOutput(*order.ToAssetID, receiveAmount, contractArgs.SellerProgram))
	if isPartialTrade {
		txData.Outputs = append(txData.Outputs, types.NewIntraChainOutput(*order.FromAssetID, order.Utxo.Amount-shouldPayAmount, order.Utxo.ControlProgram))
	}
	return nil
}

func (e *Engine) addMatchTxFeeOutput(txData *types.TxData) error {
	feeAssetAmountMap, err := CalcFeeFromMatchedTx(txData)
	if err != nil {
		return err
	}

	for feeAssetID, amount := range feeAssetAmountMap {
		var reminder int64 = 0
		feeAmount := amount.payableFeeAmount
		if amount.payableFeeAmount > amount.maxFeeAmount {
			feeAmount = amount.maxFeeAmount
			reminder = amount.payableFeeAmount - amount.maxFeeAmount
		}
		txData.Outputs = append(txData.Outputs, types.NewIntraChainOutput(feeAssetID, uint64(feeAmount), e.nodeProgram))

		// There is the remaining amount after paying the handling fee, assign it evenly to participants in the transaction
		averageAmount := reminder / int64(len(txData.Inputs))
		if averageAmount == 0 {
			averageAmount = 1
		}
		for i := 0; i < len(txData.Inputs) && reminder > 0; i++ {
			contractArgs, err := segwit.DecodeP2WMCProgram(txData.Inputs[i].ControlProgram())
			if err != nil {
				return err
			}

			if i == len(txData.Inputs)-1 {
				txData.Outputs = append(txData.Outputs, types.NewIntraChainOutput(feeAssetID, uint64(reminder), contractArgs.SellerProgram))
			} else {
				txData.Outputs = append(txData.Outputs, types.NewIntraChainOutput(feeAssetID, uint64(averageAmount), contractArgs.SellerProgram))
			}
			reminder -= averageAmount
		}
	}
	return nil
}

func setMatchTxArguments(txInput *types.TxInput, isPartialTrade bool, position int, receiveAmounts uint64) {
	var arguments [][]byte
	if isPartialTrade {
		arguments = [][]byte{vm.Int64Bytes(int64(receiveAmounts)), vm.Int64Bytes(int64(position)), vm.Int64Bytes(contract.PartialTradeClauseSelector)}
	} else {
		arguments = [][]byte{vm.Int64Bytes(int64(position)), vm.Int64Bytes(contract.FullTradeClauseSelector)}
	}
	txInput.SetArguments(arguments)
}

func addPartialTradeOrder(tx *types.Tx, orderTable *OrderTable) error {
	for i, output := range tx.Outputs {
		if !segwit.IsP2WMCScript(output.ControlProgram()) {
			continue
		}

		order, err := common.NewOrderFromOutput(tx, i)
		if err != nil {
			return err
		}

		if err := orderTable.AddOrder(order); err != nil {
			return err
		}
	}
	return nil
}

func getOppositeIndex(size int, selfIdx int) int {
	oppositeIdx := selfIdx + 1
	if selfIdx >= size-1 {
		oppositeIdx = 0
	}
	return oppositeIdx
}

type feeAmount struct {
	maxFeeAmount     int64
	payableFeeAmount int64
}

func CalcFeeFromMatchedTx(txData *types.TxData) (map[bc.AssetID]*feeAmount, error) {
	assetAmountMap := make(map[bc.AssetID]*feeAmount)
	for _, input := range txData.Inputs {
		assetAmountMap[input.AssetID()] = &feeAmount{}
	}

	receiveOutputMap := make(map[string]*types.TxOutput)
	for _, output := range txData.Outputs {
		// minus the amount of the re-order
		if segwit.IsP2WMCScript(output.ControlProgram()) {
			assetAmountMap[*output.AssetAmount().AssetId].payableFeeAmount -= int64(output.AssetAmount().Amount)
		} else {
			receiveOutputMap[hex.EncodeToString(output.ControlProgram())] = output
		}
	}

	for _, input := range txData.Inputs {
		contractArgs, err := segwit.DecodeP2WMCProgram(input.ControlProgram())
		if err != nil {
			return nil, err
		}

		assetAmountMap[input.AssetID()].payableFeeAmount += int64(input.AssetAmount().Amount)
		receiveOutput, ok := receiveOutputMap[hex.EncodeToString(contractArgs.SellerProgram)]
		if !ok {
			return nil, errors.New("the input of matched tx has no receive output")
		}

		assetAmountMap[*receiveOutput.AssetAmount().AssetId].payableFeeAmount -= int64(receiveOutput.AssetAmount().Amount)
		assetAmountMap[input.AssetID()].maxFeeAmount = CalcMaxFeeAmount(CalcShouldPayAmount(receiveOutput.AssetAmount().Amount, contractArgs))
	}

	for assetID, amount := range assetAmountMap {
		if amount.payableFeeAmount == 0 {
			delete(assetAmountMap, assetID)
		}
	}
	return assetAmountMap, nil
}

func calcRequestAmount(fromAmount uint64, contractArg *vmutil.MagneticContractArgs) uint64 {
	return uint64(int64(fromAmount) * contractArg.RatioNumerator / contractArg.RatioDenominator)
}

func CalcShouldPayAmount(receiveAmount uint64, contractArg *vmutil.MagneticContractArgs) uint64 {
	return uint64(math.Ceil(float64(receiveAmount) * float64(contractArg.RatioDenominator) / float64(contractArg.RatioNumerator)))
}

func CalcMaxFeeAmount(shouldPayAmount uint64) int64 {
	return int64(math.Ceil(float64(shouldPayAmount) * maxFeeRate))
}