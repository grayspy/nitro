package tracers

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/tracers/directory"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/streamingfast/eth-go"
	pbeth "github.com/streamingfast/firehose-ethereum/types/pb/sf/ethereum/type/v2"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var _ core.BlockchainLogger = (*Firehose)(nil)

var firehoseTracerLogLevel = strings.ToLower(os.Getenv("GETH_FIREHOSE_TRACER_LOG_LEVEL"))
var isFirehoseDebugEnabled = firehoseTracerLogLevel == "debug" || firehoseTracerLogLevel == "trace"
var isFirehoseTracerEnabled = firehoseTracerLogLevel == "trace"

var emptyCommonAddress = common.Address{}
var emptyCommonHash = common.Hash{}

func init() {
	staticFirehoseChainValidationOnInit()

	directory.LiveDirectory.Register("firehose", newFirehoseTracer)
}

func newFirehoseTracer() (core.BlockchainLogger, error) {
	firehoseDebug("New firehose tracer")
	return NewFirehoseLogger(), nil
}

type Firehose struct {
	// Global state
	outputBuffer *bytes.Buffer

	// Block state
	block         *pbeth.Block
	blockBaseFee  *big.Int
	blockOrdinal  *Ordinal
	blockFinality *FinalityStatus

	// Transaction state
	transaction         *pbeth.TransactionTrace
	transactionLogIndex uint32
	isPrecompiledAddr   func(addr common.Address) bool

	// Call state
	callStack               *CallStack
	deferredCallState       *DeferredCallState
	latestCallStartSuicided bool
}

func NewFirehoseLogger() *Firehose {
	// FIXME: Where should we put our actual INIT line?
	// FIXME: Pickup version from go-ethereum (PR comment)
	printToFirehose("INIT", "2.3", "geth", "1.12.0")

	return &Firehose{
		// Global state
		outputBuffer: bytes.NewBuffer(make([]byte, 0, 100*1024*1024)),

		// Block state
		blockOrdinal:  &Ordinal{},
		blockFinality: &FinalityStatus{},

		// Transaction state
		transactionLogIndex: 0,

		// Call state
		callStack:               NewCallStack(),
		deferredCallState:       NewDeferredCallState(),
		latestCallStartSuicided: false,
	}
}

// resetBlock resets the block state only, do not reset transaction or call state
func (f *Firehose) resetBlock() {
	f.block = nil
	f.blockBaseFee = nil
	f.blockOrdinal.Reset()
	f.blockFinality.Reset()
}

// resetTransaction resets the transaction state and the call state in one shot
func (f *Firehose) resetTransaction() {
	f.transaction = nil
	f.transactionLogIndex = 0
	f.isPrecompiledAddr = nil

	f.callStack.Reset()
	f.latestCallStartSuicided = false
	f.deferredCallState.Reset()
}

func (f *Firehose) OnBlockStart(b *types.Block, td *big.Int, finalized *types.Header, _ *types.Header, _ *params.ChainConfig) {

	var finalizedNum uint64
	var finalizedHash []byte

	if finalized != nil {
		finalizedNum = finalized.Number.Uint64()
		finalizedHash = finalized.Hash().Bytes()
	}

	f.onBlockStart(b, td, finalizedNum, finalizedHash)
}

func (f *Firehose) onBlockStart(b *types.Block, td *big.Int, finalizedNum uint64, finalizedHash []byte) {
	firehoseDebug("block start number=%d hash=%s", b.NumberU64(), b.Hash())

	f.ensureNotInBlock()

	f.block = &pbeth.Block{
		Hash:   b.Hash().Bytes(),
		Number: b.Number().Uint64(),
		Header: newBlockHeaderFromChainHeader(b.Header(), firehoseBigIntFromNative(new(big.Int).Add(td, b.Difficulty()))),
		Size:   b.Size(),
		// Known Firehose issue: If you fix all known Firehose issue for a new chain, don't forget to bump `Ver` to `4`!
		Ver: 3,
	}

	for _, uncle := range b.Uncles() {
		// TODO: check if td should be part of uncles
		f.block.Uncles = append(f.block.Uncles, newBlockHeaderFromChainHeader(uncle, nil))
	}

	if f.block.Header.BaseFeePerGas != nil {
		f.blockBaseFee = f.block.Header.BaseFeePerGas.Native()
	}

	f.blockFinality.populateFromChain(finalizedNum, finalizedHash)

}
func (f *Firehose) OnBlockUpdate(b *types.Block, td *big.Int) {
	f.ensureInBlock()
	f.block.Hash = b.Hash().Bytes()
	f.block.Number = b.Number().Uint64()
	f.block.Header = newBlockHeaderFromChainHeader(b.Header(), firehoseBigIntFromNative(new(big.Int).Add(td, b.Difficulty())))
	f.block.Size = b.Size()
}

func (f *Firehose) OnBlockEnd(err error) {
	firehoseDebug("block ending err=%s", errorView(err))

	if err == nil {
		f.ensureInBlockAndNotInTrx()
		f.printBlockToFirehose(f.block, f.blockFinality)
	} else {
		// An error occurred, could have happen in transaction/call context, we must not check if in trx/call, only check in block
		f.ensureInBlock()
	}

	f.resetBlock()
	f.resetTransaction()

	firehoseDebug("block end")
}

func (f *Firehose) OnBeaconBlockRootStart(root common.Hash) {
	// FIXME: This needs to be implemented when hard-fork bringing beacon block root is implemented
	// We kind-of decided on having `system_calls` on the Block directly.
}

func (f *Firehose) OnBeaconBlockRootEnd() {
	// FIXME: This needs to be implemented when hard-fork bringing beacon block root is implemented
	// We kind-of decided on having `system_calls` on the Block directly.
}

func (f *Firehose) CaptureTxStart(evm *vm.EVM, tx *types.Transaction, from common.Address) {
	firehoseDebug("trx start hash=%s type=%d gas=%d input=%s", tx.Hash(), tx.Type(), tx.Gas(), inputView(tx.Data()))

	f.ensureInBlockAndNotInTrxAndNotInCall()

	var to common.Address
	if tx.To() == nil {
		to = crypto.CreateAddress(from, evm.StateDB.GetNonce(from))
	} else {
		to = *tx.To()
	}

	f.captureTxStart(tx, tx.Hash(), from, to, evm.IsPrecompileAddr)

	switch tx.Type() {
	case types.ArbitrumDepositTxType, types.ArbitrumSubmitRetryableTxType, types.ArbitrumInternalTxType:
		firehoseDebug("Adding simulated root call to arbitrum tx hash=%s type=%d gas=%d input=%s", tx.Hash(), tx.Type(), tx.Gas(), inputView(tx.Data()))
		f.callStart("root", pbeth.CallType_CALL, from, *tx.To(), tx.Data(), tx.Gas(), tx.Value())
	}
}

// captureTxStart is used internally a two places, in the normal "tracer" and in the "OnGenesisBlock",
// we manually pass some override to the `tx` because genesis block has a different way of creating
// the transaction that wraps the genesis block.
func (f *Firehose) captureTxStart(tx *types.Transaction, hash common.Hash, from, to common.Address, isPrecompiledAddr func(common.Address) bool) {
	f.isPrecompiledAddr = isPrecompiledAddr

	v, r, s := tx.RawSignatureValues()

	f.transaction = &pbeth.TransactionTrace{
		BeginOrdinal:         f.blockOrdinal.Next(),
		Hash:                 hash.Bytes(),
		From:                 from.Bytes(),
		To:                   to.Bytes(),
		Nonce:                tx.Nonce(),
		GasLimit:             tx.Gas(),
		GasPrice:             gasPrice(tx, f.blockBaseFee),
		Value:                firehoseBigIntFromNative(tx.Value()),
		Input:                tx.Data(),
		V:                    emptyBytesToNil(v.Bytes()),
		R:                    normalizeSignaturePoint(r.Bytes()),
		S:                    normalizeSignaturePoint(s.Bytes()),
		Type:                 transactionTypeFromChainTxType(tx.Type()),
		AccessList:           newAccessListFromChain(tx.AccessList()),
		MaxFeePerGas:         maxFeePerGas(tx),
		MaxPriorityFeePerGas: maxPriorityFeePerGas(tx),
	}
}

func (f *Firehose) CaptureTxEnd(receipt *types.Receipt, err error) {
	firehoseDebug("trx ending, err=%s", errorView(err), ctxView(f))
	f.ensureInBlockAndInTrx()

	if receipt != nil {
		switch receipt.Type {
		case types.ArbitrumDepositTxType, types.ArbitrumSubmitRetryableTxType, types.ArbitrumInternalTxType:
			firehoseDebug("Closing simulated root call to arbitrum tx type=%d", receipt.Type)
			f.callEnd("root", nil, receipt.GasUsed, err)
		}

		f.block.TransactionTraces = append(f.block.TransactionTraces, f.completeTransaction(receipt))
	}

	// The reset must be done as the very last thing as the CallStack needs to be
	// properly populated for the `completeTransaction` call above to complete correctly.
	f.resetTransaction()

	firehoseDebug("trx end")
}

func (f *Firehose) CaptureArbitrumTransfer(env *vm.EVM, from, to *common.Address, value *big.Int, before bool, purpose string) {
	// transfers for this are caught through OnBalanceChange, etc.
}

func (f *Firehose) CaptureArbitrumStorageGet(key common.Hash, depth int, before bool) {
	// nothing interesting for firehose here
}

func (f *Firehose) CaptureArbitrumStorageSet(key, value common.Hash, depth int, before bool) {
	// nothing interesting for firehose here
}
func (f *Firehose) completeTransaction(receipt *types.Receipt) *pbeth.TransactionTrace {
	firehoseDebug("completing transaction call_count=%d receipt=%s", len(f.transaction.Calls), (*receiptView)(receipt))

	// Sorting needs to happen first, before we populate the state reverted
	slices.SortFunc(f.transaction.Calls, func(i, j *pbeth.Call) int {
		return Compare(i.Index, j.Index)
	})

	rootCall := f.transaction.Calls[0]

	if !f.deferredCallState.IsEmpty() {
		f.deferredCallState.MaybePopulateCallAndReset("root", rootCall)
	}

	// Receipt can be nil if an error occurred during the transaction execution, right now we don't have it
	if receipt != nil {
		f.transaction.Index = uint32(receipt.TransactionIndex)
		f.transaction.GasUsed = receipt.GasUsed
		f.transaction.Receipt = newTxReceiptFromChain(receipt)
		f.transaction.Status = transactionStatusFromChainTxReceipt(receipt.Status)
	}

	// It's possible that the transaction was reverted, but we still have a receipt, in that case, we must
	// check the root call
	if rootCall.StatusReverted {
		f.transaction.Status = pbeth.TransactionTraceStatus_REVERTED
	}

	// Order is important, we must populate the state reverted before we remove the log block index and re-assign ordinals
	f.populateStateReverted()
	f.removeLogBlockIndexOnStateRevertedCalls()
	f.assignOrdinalAndIndexToReceiptLogs()

	// Known Firehose issue: This field has never been populated in the old Firehose instrumentation, so it's the same thing for now
	// f.transaction.ReturnData = rootCall.ReturnData
	f.transaction.EndOrdinal = f.blockOrdinal.Next()

	return f.transaction
}

func (f *Firehose) populateStateReverted() {
	// Calls are ordered by execution index. So the algo is quite simple.
	// We loop through the flat calls, at each call, if the parent is present
	// and reverted, the current call is reverted. Otherwise, if the current call
	// is failed, the state is reverted. In all other cases, we simply continue
	// our iteration loop.
	//
	// This works because we see the parent before its children, and since we
	// trickle down the state reverted value down the children, checking the parent
	// of a call will always tell us if the whole chain of parent/child should
	// be reverted
	//
	calls := f.transaction.Calls
	for _, call := range f.transaction.Calls {
		var parent *pbeth.Call
		if call.ParentIndex > 0 {
			parent = calls[call.ParentIndex-1]
		}

		call.StateReverted = (parent != nil && parent.StateReverted) || call.StatusFailed
	}
}

func (f *Firehose) removeLogBlockIndexOnStateRevertedCalls() {
	for _, call := range f.transaction.Calls {
		if call.StateReverted {
			for _, log := range call.Logs {
				log.BlockIndex = 0
				log.Index = 0
			}
		}
	}
}

func (f *Firehose) assignOrdinalAndIndexToReceiptLogs() {
	firehoseTrace("assigning ordinal and index to logs")
	defer func() {
		firehoseTrace("assigning ordinal and index to logs terminated")
	}()

	trx := f.transaction

	receiptsLogs := trx.Receipt.Logs

	callLogs := []*pbeth.Log{}
	for _, call := range trx.Calls {
		firehoseTrace("checking call reverted=%t logs=%d", call.StateReverted, len(call.Logs))
		if call.StateReverted {
			continue
		}

		callLogs = append(callLogs, call.Logs...)
	}

	slices.SortFunc(callLogs, func(i, j *pbeth.Log) int {
		return Compare(i.Ordinal, j.Ordinal)
	})

	if len(callLogs) != len(receiptsLogs) {
		j, err := json.Marshal(trx)
		if err != nil {
			firehoseDebug("error marshalling trx during panic handling: %s", err)
		}

		firehoseDebug("got this transaction: %s", string(j))
		panic(fmt.Errorf(
			"mismatch between Firehose call logs and Ethereum transaction %s receipt logs at block #%d, transaction receipt has %d logs but there is %d Firehose call logs",
			hex.EncodeToString(trx.Hash),
			f.block.Number,
			len(receiptsLogs),
			len(callLogs),
		))
	}

	var txIndex uint32 = 0
	for _, log := range callLogs {
		log.Index = txIndex
		txIndex++
	}

	for i := 0; i < len(callLogs); i++ {
		callLog := callLogs[i]
		receiptsLog := receiptsLogs[i]

		result := &validationResult{}
		// Ordinal must **not** be checked as we are assigning it here below after the validations
		validateBytesField(result, "Address", callLog.Address, receiptsLog.Address)
		validateUint32Field(result, "BlockIndex", callLog.BlockIndex, receiptsLog.BlockIndex)
		validateBytesField(result, "Data", callLog.Data, receiptsLog.Data)
		validateArrayOfBytesField(result, "Topics", callLog.Topics, receiptsLog.Topics)

		if len(result.failures) > 0 {
			for i, ll := range callLogs {
				result.failures = append(result.failures, fmt.Sprintf("log %d, idx %d", i, ll.Index))
			}
			for i, ll := range receiptsLogs {
				result.failures = append(result.failures, fmt.Sprintf("theirs: log %d, idx %d", i, ll.Index))
			}
			result.panicOnAnyFailures("mismatch between Firehose call log and Ethereum transaction receipt log at index %d", i)
		}

		receiptsLog.Index = callLog.Index
		receiptsLog.Ordinal = callLog.Ordinal
	}
}

// CaptureStart implements the EVMLogger interface to initialize the tracing operation.
func (f *Firehose) CaptureStart(from common.Address, to common.Address, create bool, input []byte, gas uint64, value *big.Int) {
	f.callStart("root", rootCallType(create), from, to, input, gas, value)
}

// CaptureEnd is called after the call finishes to finalize the tracing.
func (f *Firehose) CaptureEnd(output []byte, gasUsed uint64, err error) {
	f.callEnd("root", output, gasUsed, err)
}

// CaptureState implements the EVMLogger interface to trace a single step of VM execution.
func (f *Firehose) CaptureState(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, rData []byte, depth int, err error) {
	firehoseTrace("capture state op=%s gas=%d cost=%d, err=%s", op, gas, cost, errorView(err))

	if activeCall := f.callStack.Peek(); activeCall != nil {
		f.captureInterpreterStep(activeCall, pc, op, gas, cost, scope, rData, depth, err)

		if err == nil && cost > 0 {
			if reason, found := opCodeToGasChangeReasonMap[op]; found {
				activeCall.GasChanges = append(activeCall.GasChanges, f.newGasChange("state", gas, gas-cost, reason))
			}
		}
	}
}

var opCodeToGasChangeReasonMap = map[vm.OpCode]pbeth.GasChange_Reason{
	vm.CREATE:         pbeth.GasChange_REASON_CONTRACT_CREATION,
	vm.CREATE2:        pbeth.GasChange_REASON_CONTRACT_CREATION2,
	vm.CALL:           pbeth.GasChange_REASON_CALL,
	vm.STATICCALL:     pbeth.GasChange_REASON_STATIC_CALL,
	vm.CALLCODE:       pbeth.GasChange_REASON_CALL_CODE,
	vm.DELEGATECALL:   pbeth.GasChange_REASON_DELEGATE_CALL,
	vm.RETURN:         pbeth.GasChange_REASON_RETURN,
	vm.REVERT:         pbeth.GasChange_REASON_REVERT,
	vm.LOG0:           pbeth.GasChange_REASON_EVENT_LOG,
	vm.LOG1:           pbeth.GasChange_REASON_EVENT_LOG,
	vm.LOG2:           pbeth.GasChange_REASON_EVENT_LOG,
	vm.LOG3:           pbeth.GasChange_REASON_EVENT_LOG,
	vm.LOG4:           pbeth.GasChange_REASON_EVENT_LOG,
	vm.SELFDESTRUCT:   pbeth.GasChange_REASON_SELF_DESTRUCT,
	vm.CALLDATACOPY:   pbeth.GasChange_REASON_CALL_DATA_COPY,
	vm.CODECOPY:       pbeth.GasChange_REASON_CODE_COPY,
	vm.EXTCODECOPY:    pbeth.GasChange_REASON_EXT_CODE_COPY,
	vm.RETURNDATACOPY: pbeth.GasChange_REASON_RETURN_DATA_COPY,
}

// CaptureFault implements the EVMLogger interface to trace an execution fault.
func (f *Firehose) CaptureFault(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, depth int, err error) {
	if activeCall := f.callStack.Peek(); activeCall != nil {
		f.captureInterpreterStep(activeCall, pc, op, gas, cost, scope, nil, depth, err)
	}
}

func (f *Firehose) captureInterpreterStep(activeCall *pbeth.Call, pc uint64, op vm.OpCode, gas, cost uint64, _ *vm.ScopeContext, rData []byte, depth int, err error) {
	if !activeCall.ExecutedCode {
		firehoseTrace("setting active call executed code to true")
		activeCall.ExecutedCode = true
	}
}

func (f *Firehose) CaptureEnter(typ vm.OpCode, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	f.ensureInBlockAndInTrx()

	// The invokation for vm.SELFDESTRUCT is called while already in another call, so we must not check that we are not in a call here
	if typ == vm.SELFDESTRUCT {
		f.ensureInCall()
		f.callStack.Peek().Suicide = true

		if value.Sign() != 0 {
			f.OnBalanceChange(from, value, common.Big0, state.BalanceDecreaseSelfdestruct)
		}

		// The next CaptureExit must be ignored, this variable will make the next CaptureExit to be ignored
		f.latestCallStartSuicided = true
		return
	}

	callType := callTypeFromOpCode(typ)
	if callType == pbeth.CallType_UNSPECIFIED {
		panic(fmt.Errorf("unexpected call type, received OpCode %s but only call related opcode (CALL, CREATE, CREATE2, STATIC, DELEGATECALL and CALLCODE) or SELFDESTRUCT is accepted", typ))
	}

	f.callStart("child", callType, from, to, input, gas, value)
}

// CaptureExit is called when EVM exits a scope, even if the scope didn't
// execute any code.
func (f *Firehose) CaptureExit(output []byte, gasUsed uint64, err error) {
	f.callEnd("child", output, gasUsed, err)
}

func (f *Firehose) callStart(source string, callType pbeth.CallType, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	firehoseDebug("call start source=%s index=%d type=%s input=%s", source, f.callStack.NextIndex(), callType, inputView(input))
	f.ensureInBlockAndInTrx()

	// Known Firehose issue: Contract creation call's input is always `nil` in old Firehose patch
	// due to an oversight that having it in `CodeChange` would be sufficient but this is wrong
	// as constructor's input are not part of the code change but part of the call input.
	//
	// New chain integration should remove this `if` statement completely.
	if callType == pbeth.CallType_CREATE {
		input = nil
	}

	v := firehoseBigIntFromNative(value)
	if callType == pbeth.CallType_DELEGATE {
		// If it's a delegate call, the there should be a call in the stack and value should be parent's value
		v = f.callStack.Peek().Value
	}

	call := &pbeth.Call{
		// Known Firehose issue: Ref 042a2ff03fd623f151d7726314b8aad6 (see below)
		//
		// New chain integration should uncomment the code below and remove the `if` statement of the the other ref
		// BeginOrdinal: f.blockOrdinal.Next(),
		CallType: callType,
		Depth:    0,
		Caller:   from.Bytes(),
		Address:  to.Bytes(),
		// We need to clone `input` received by the tracer as it's re-used within Geth!
		Input:    bytes.Clone(input),
		Value:    v,
		GasLimit: gas,
	}

	// Known Firehose issue: The BeginOrdinal of the genesis block root call is never actually
	// incremented and it's always 0.
	//
	// New chain integration should remove this `if` statement and uncomment code of other ref
	// above.
	//
	// Ref 042a2ff03fd623f151d7726314b8aad6
	if f.block.Number != 0 {
		call.BeginOrdinal = f.blockOrdinal.Next()
	}

	if err := f.deferredCallState.MaybePopulateCallAndReset(source, call); err != nil {
		panic(err)
	}

	// Known Firehose issue: The `BeginOrdinal` of the root call is incremented but must
	// be assigned back to 0 because of a bug in the console reader. remove on new chain.
	//
	// New chain integration should remove this `if` statement
	if source == "root" {
		call.BeginOrdinal = 0
	}

	f.callStack.Push(call)
}

func (f *Firehose) callEnd(source string, output []byte, gasUsed uint64, err error) {
	firehoseDebug("call end source=%s index=%d output=%s gasUsed=%d err=%s", source, f.callStack.ActiveIndex(), outputView(output), gasUsed, errorView(err))

	if f.latestCallStartSuicided {
		if source != "child" {
			panic(fmt.Errorf("unexpected source for suicided call end, expected child but got %s, suicide are always produced on a 'child' source", source))
		}

		// Geth native tracer does a `CaptureEnter(SELFDESTRUCT, ...)/CaptureExit(...)`, we must skip the `CaptureExit` call
		// in that case because we did not push it on our CallStack.
		f.latestCallStartSuicided = false
		return
	}

	f.ensureInBlockAndInTrxAndInCall()

	call := f.callStack.Pop()
	call.GasConsumed = gasUsed

	// For create call, we do not save the returned value which is the actual contract's code
	if call.CallType != pbeth.CallType_CREATE {
		call.ReturnData = bytes.Clone(output)
	}

	// Known Firehose issue: How we computed `executed_code` before was not working for contract's that only
	// deal with ETH transfer through Solidity `receive()` built-in since those call have `len(input) == 0`
	//
	// New chain should turn the logic into:
	//
	//     if !call.ExecutedCode && f.isPrecompiledAddr(common.BytesToAddress(call.Address)) {
	//         call.ExecutedCode = true
	//     }
	//
	// At this point, `call.ExecutedCode` is tied to `EVMInterpreter#Run` execution (in `core/vm/interpreter.go`)
	// and is `true` if the run/loop of the interpreter executed.
	//
	// This means that if `false` the interpreter did not run at all and we would had emitted a
	// `account_without_code` event in the old Firehose patch which you have set `call.ExecutecCode`
	// to false
	//
	// For precompiled address however, interpreter does not run so determine  there was a bug in Firehose instrumentation where we would
	if call.ExecutedCode || f.isPrecompiledAddr(common.BytesToAddress(call.Address)) {
		// In this case, we are sure that some code executed. This translates in the old Firehose instrumentation
		// that it would have **never** emitted an `account_without_code`.
		//
		// When no `account_without_code` was executed in the previous Firehose instrumentation,
		// the `call.ExecutedCode` defaulted to the condition below
		call.ExecutedCode = call.CallType != pbeth.CallType_CREATE && len(call.Input) > 0
	} else {
		// In all other cases, we are sure that no code executed. This translates in the old Firehose instrumentation
		// that it would have emitted an `account_without_code` and it would have then forced set the `call.ExecutedCode`
		// to `false`.
		call.ExecutedCode = false
	}

	if err != nil {
		call.FailureReason = err.Error()
		call.StatusFailed = true

		// We also treat ErrInsufficientBalance and ErrDepth as reverted in Firehose model
		// because they do not cost any gas.
		call.StatusReverted = errors.Is(err, vm.ErrExecutionReverted) || errors.Is(err, vm.ErrInsufficientBalance) || errors.Is(err, vm.ErrDepth)
	}

	// Known Firehose issue: The EndOrdinal of the genesis block root call is never actually
	// incremented and it's always 0.
	//
	// New chain should turn the logic into:
	//
	//     call.EndOrdinal = f.blockOrdinal.Next()
	//
	// Removing the condition around the `EndOrdinal` assignment (keeping it!)
	if f.block.Number != 0 {
		call.EndOrdinal = f.blockOrdinal.Next()
	}

	f.transaction.Calls = append(f.transaction.Calls, call)
}

// CaptureKeccakPreimage is called during the KECCAK256 opcode.
func (f *Firehose) CaptureKeccakPreimage(hash common.Hash, data []byte) {
	f.ensureInBlockAndInTrxAndInCall()

	activeCall := f.callStack.Peek()
	if activeCall.KeccakPreimages == nil {
		activeCall.KeccakPreimages = make(map[string]string)
	}

	activeCall.KeccakPreimages[hex.EncodeToString(hash.Bytes())] = hex.EncodeToString(data)
}

func (f *Firehose) OnGenesisBlock(b *types.Block, alloc core.GenesisAlloc) {
	f.onBlockStart(b, big.NewInt(0), 0, nil)
	f.captureTxStart(types.NewTx(&types.LegacyTx{}), emptyCommonHash, emptyCommonAddress, emptyCommonAddress, func(common.Address) bool { return false })
	f.CaptureStart(emptyCommonAddress, emptyCommonAddress, false, nil, 0, nil)

	for _, addr := range sortedKeys(alloc) {
		account := alloc[addr]

		f.OnNewAccount(addr)

		if account.Balance != nil && account.Balance.Sign() != 0 {
			activeCall := f.callStack.Peek()
			activeCall.BalanceChanges = append(activeCall.BalanceChanges, f.newBalanceChange("genesis", addr, common.Big0, account.Balance, pbeth.BalanceChange_REASON_GENESIS_BALANCE))
		}

		if len(account.Code) > 0 {
			f.OnCodeChange(addr, emptyCommonHash, nil, common.BytesToHash(crypto.Keccak256(account.Code)), account.Code)
		}

		if account.Nonce > 0 {
			f.OnNonceChange(addr, 0, account.Nonce)
		}

		for _, key := range sortedKeys(account.Storage) {
			f.OnStorageChange(addr, key, emptyCommonHash, account.Storage[key])
		}
	}

	f.CaptureEnd(nil, 0, nil)
	f.CaptureTxEnd(&types.Receipt{
		PostState: b.Root().Bytes(),
		Status:    types.ReceiptStatusSuccessful,
	}, nil)
	f.OnBlockEnd(nil)
}

type bytesGetter interface {
	comparable
	Bytes() []byte
}

func sortedKeys[K bytesGetter, V any](m map[K]V) []K {
	keys := maps.Keys(m)
	slices.SortFunc(keys, func(i, j K) int {
		return bytes.Compare(i.Bytes(), j.Bytes())
	})

	return keys
}

func (f *Firehose) OnBalanceChange(a common.Address, prev, new *big.Int, reason state.BalanceChangeReason) {
	if reason == state.BalanceChangeUnspecified {
		// We ignore those, if they are mislabelled, too bad so particular attention needs to be ported to this
		return
	}

	f.ensureInBlockOrTrx()

	change := f.newBalanceChange("tracer", a, prev, new, balanceChangeReasonFromChain(reason))

	if f.transaction != nil {
		activeCall := f.callStack.Peek()

		// There is an initial transfer happening will the call is not yet started, we track it manually
		if activeCall == nil {
			f.deferredCallState.balanceChanges = append(f.deferredCallState.balanceChanges, change)
			return
		}

		activeCall.BalanceChanges = append(activeCall.BalanceChanges, change)
	} else {
		f.block.BalanceChanges = append(f.block.BalanceChanges, change)
	}
}

func (f *Firehose) newBalanceChange(tag string, address common.Address, oldValue, newValue *big.Int, reason pbeth.BalanceChange_Reason) *pbeth.BalanceChange {
	firehoseTrace("balance changed tag=%s before=%d after=%d reason=%s", tag, oldValue, newValue, reason)

	if reason == pbeth.BalanceChange_REASON_UNKNOWN {
		panic(fmt.Errorf("received unknown balance change reason %s", reason))
	}

	return &pbeth.BalanceChange{
		Ordinal:  f.blockOrdinal.Next(),
		Address:  address.Bytes(),
		OldValue: firehoseBigIntFromNative(oldValue),
		NewValue: firehoseBigIntFromNative(newValue),
		Reason:   reason,
	}
}

func (f *Firehose) OnNonceChange(a common.Address, prev, new uint64) {
	// important: NonceChange is sometimes called with prev==new outside of any transaction
	if new == prev {
		firehoseDebug("skipping NonceChange new==prev (%d)", prev)
		return
	}

	f.ensureInBlockAndInTrx()

	activeCall := f.callStack.Peek()
	change := &pbeth.NonceChange{
		Address:  a.Bytes(),
		OldValue: prev,
		NewValue: new,
		Ordinal:  f.blockOrdinal.Next(),
	}

	// There is an initial nonce change happening when the call is not yet started, we track it manually
	if activeCall == nil {
		f.deferredCallState.nonceChanges = append(f.deferredCallState.nonceChanges, change)
		return
	}

	activeCall.NonceChanges = append(activeCall.NonceChanges, change)
}

func (f *Firehose) OnCodeChange(a common.Address, prevCodeHash common.Hash, prev []byte, codeHash common.Hash, code []byte) {
	f.ensureInBlockOrTrx()

	change := &pbeth.CodeChange{
		Address: a.Bytes(),
		OldHash: prevCodeHash.Bytes(),
		OldCode: prev,
		NewHash: codeHash.Bytes(),
		NewCode: code,
		Ordinal: f.blockOrdinal.Next(),
	}

	if f.transaction != nil {
		activeCall := f.callStack.Peek()
		if activeCall == nil {
			f.panicNotInState("caller expected to be in call state but we were not, this is a bug")
		}

		activeCall.CodeChanges = append(activeCall.CodeChanges, change)
	} else {
		f.block.CodeChanges = append(f.block.CodeChanges, change)
	}
}

func (f *Firehose) OnStorageChange(a common.Address, k, prev, new common.Hash) {
	firehoseTrace("on storage change addr=%s", a)
	f.ensureInBlockAndInTrx()

	activeCall := f.callStack.Peek()
	change := &pbeth.StorageChange{
		Address:  a.Bytes(),
		Key:      k.Bytes(),
		OldValue: prev.Bytes(),
		NewValue: new.Bytes(),
		Ordinal:  f.blockOrdinal.Next(),
	}
	// There is an initial gas consumption happening will the call is not yet started, we track it manually
	if activeCall == nil {
		f.deferredCallState.storageChanges = append(f.deferredCallState.storageChanges, change)
		return
	}

	activeCall.StorageChanges = append(activeCall.StorageChanges, change)
}

func (f *Firehose) OnLog(l *types.Log) {
	firehoseTrace("on log addr=%s topics=%d, txindex=%d", l.Address, len(l.Topics), f.transactionLogIndex)

	f.ensureInBlockAndInTrx()
	topics := make([][]byte, len(l.Topics))
	for i, topic := range l.Topics {
		topics[i] = topic.Bytes()
	}

	log := &pbeth.Log{
		Address:    l.Address.Bytes(),
		Topics:     topics,
		Data:       l.Data,
		Index:      f.transactionLogIndex,
		BlockIndex: uint32(l.Index),
		Ordinal:    f.blockOrdinal.Next(),
	}

	f.transactionLogIndex++

	activeCall := f.callStack.Peek()
	if activeCall == nil {
		f.deferredCallState.logs = append(f.deferredCallState.logs, log)
		return
	}

	activeCall.Logs = append(activeCall.Logs, log)
}

func (f *Firehose) OnNewAccount(a common.Address) {
	f.ensureInBlockOrTrx()
	if f.transaction == nil {
		// We receive OnNewAccount on finalization of the block which means there is no
		// transaction active. In that case, we do not track the account creation because
		// the "old" Firehose didn't but mainly because we don't have `AccountCreation` at
		// the block level so what can we do...

		// This fix was applied on Erigon branch after chain's comparison. I need to check
		// with what the old patch was doing to write a meaningful comment here and ensure
		// they got the logic right
		f.blockOrdinal.Next()
		return
	}

	if f.isPrecompiledAddr(a) {
		return
	}

	accountCreation := &pbeth.AccountCreation{
		Account: a.Bytes(),
		Ordinal: f.blockOrdinal.Next(),
	}

	activeCall := f.callStack.Peek()
	if activeCall == nil {
		f.deferredCallState.accountCreations = append(f.deferredCallState.accountCreations, accountCreation)
		return
	}

	activeCall.AccountCreations = append(activeCall.AccountCreations, accountCreation)
}

func (f *Firehose) OnGasChange(old, new uint64, reason vm.GasChangeReason) {
	f.ensureInBlockAndInTrx()

	if old == new {
		return
	}

	if reason == vm.GasChangeCallOpCode {
		// We ignore those because we track OpCode gas consumption manually by tracking the gas value at `CaptureState` call
		return
	}

	// Known Firehose issue: New geth native tracer added more gas change, some that we were indeed missing and
	// should have included in our previous patch.
	//
	// For new chain, this code should be remove so that they are included and useful to user.
	//
	// Ref eb1916a67d9bea03df16a7a3e2cfac72
	if reason == vm.GasChangeTxInitialBalance ||
		reason == vm.GasChangeTxRefunds ||
		reason == vm.GasChangeTxLeftOverReturned ||
		reason == vm.GasChangeCallInitialBalance ||
		reason == vm.GasChangeCallLeftOverReturned {
		return
	}

	activeCall := f.callStack.Peek()
	change := f.newGasChange("tracer", old, new, gasChangeReasonFromChain(reason))

	// There is an initial gas consumption happening will the call is not yet started, we track it manually
	if activeCall == nil {
		f.deferredCallState.gasChanges = append(f.deferredCallState.gasChanges, change)
		return
	}

	activeCall.GasChanges = append(activeCall.GasChanges, change)
}

func (f *Firehose) newGasChange(tag string, oldValue, newValue uint64, reason pbeth.GasChange_Reason) *pbeth.GasChange {
	firehoseTrace("gas consumed tag=%s before=%d after=%d reason=%s", tag, oldValue, newValue, reason)

	// Should already be checked by the caller, but we keep it here for safety if the code ever change
	if reason == pbeth.GasChange_REASON_UNKNOWN {
		panic(fmt.Errorf("received unknown gas change reason %s", reason))
	}

	return &pbeth.GasChange{
		OldValue: oldValue,
		NewValue: newValue,
		Ordinal:  f.blockOrdinal.Next(),
		Reason:   reason,
	}
}

func (f *Firehose) ensureInBlock() {
	if f.block == nil {
		f.panicNotInState("caller expected to be in block state but we were not, this is a bug")
	}
}

func (f *Firehose) ensureNotInBlock() {
	if f.block != nil {
		f.panicNotInState("caller expected to not be in block state but we were, this is a bug")
	}
}

func (f *Firehose) ensureInBlockAndInTrx() {
	f.ensureInBlock()

	if f.transaction == nil {
		f.panicNotInState("caller expected to be in transaction state but we were not, this is a bug")
	}
}

func (f *Firehose) ensureInBlockAndNotInTrx() {
	f.ensureInBlock()

	if f.transaction != nil {
		f.panicNotInState("caller expected to not be in transaction state but we were, this is a bug")
	}
}

func (f *Firehose) ensureInBlockAndNotInTrxAndNotInCall() {
	f.ensureInBlock()

	if f.transaction != nil {
		f.panicNotInState("caller expected to not be in transaction state but we were, this is a bug")
	}

	if f.callStack.HasActiveCall() {
		f.panicNotInState("caller expected to not be in call state but we were, this is a bug")
	}
}

func (f *Firehose) ensureInBlockOrTrx() {
	if f.transaction == nil && f.block == nil {
		f.panicNotInState("caller expected to be in either block or  transaction state but we were not, this is a bug")
	}
}

func (f *Firehose) ensureInBlockAndInTrxAndInCall() {
	if f.transaction == nil || f.block == nil {
		f.panicNotInState("caller expected to be in block and in transaction but we were not, this is a bug")
	}

	if !f.callStack.HasActiveCall() {
		f.panicNotInState("caller expected to be in call state but we were not, this is a bug")
	}
}

func (f *Firehose) ensureInCall() {
	if f.block == nil {
		f.panicNotInState("caller expected to be in call state but we were not, this is a bug")
	}
}

func (f *Firehose) panicNotInState(msg string) string {
	firehoseDebugPrintStack()
	panic(fmt.Errorf("%s (inBlock=%t, inTransaction=%t, inCall=%t)", msg, f.block != nil, f.transaction != nil, f.callStack.HasActiveCall()))
}

// printToFirehose is an easy way to print to Firehose format, it essentially
// adds the "FIRE" prefix to the input and joins the input with spaces as well
// as adding a newline at the end.
//
// It flushes this through [flushToFirehose] to the `os.Stdout` writer.
func (f *Firehose) printBlockToFirehose(block *pbeth.Block, finalityStatus *FinalityStatus) {
	marshalled, err := proto.Marshal(block)
	if err != nil {
		panic(fmt.Errorf("failed to marshal block: %w", err))
	}

	f.outputBuffer.Reset()

	libNum, libID := finalityStatus.ToFirehoseLogParams()

	// **Important* The final space in the Sprintf template is mandatory!
	f.outputBuffer.WriteString(fmt.Sprintf("FIRE BLOCK %d %s %s %s ", block.Number, hex.EncodeToString(block.Hash), libNum, libID))

	encoder := base64.NewEncoder(base64.StdEncoding, f.outputBuffer)
	if _, err = encoder.Write(marshalled); err != nil {
		panic(fmt.Errorf("write to encoder should have been infaillible: %w", err))
	}

	if err := encoder.Close(); err != nil {
		panic(fmt.Errorf("closing encoder should have been infaillible: %w", err))
	}

	f.outputBuffer.WriteString("\n")

	flushToFirehose(f.outputBuffer.Bytes(), os.Stdout)
}

// printToFirehose is an easy way to print to Firehose format, it essentially
// adds the "FIRE" prefix to the input and joins the input with spaces as well
// as adding a newline at the end.
//
// It flushes this through [flushToFirehose] to the `os.Stdout` writer.
func printToFirehose(input ...string) {
	flushToFirehose([]byte("FIRE "+strings.Join(input, " ")+"\n"), os.Stdout)
}

// flushToFirehose sends data to Firehose via `io.Writter` checking for errors
// and retrying if necessary.
//
// If error is still present after 10 retries, prints an error message to `writer`
// as well as writing file `/tmp/firehose_writer_failed_print.log` with the same
// error message.
func flushToFirehose(in []byte, writer io.Writer) {
	var written int
	var err error
	loops := 10
	for i := 0; i < loops; i++ {
		written, err = writer.Write(in)

		if len(in) == written {
			return
		}

		in = in[written:]
		if i == loops-1 {
			break
		}
	}

	errstr := fmt.Sprintf("\nFIREHOSE FAILED WRITING %dx: %s\n", loops, err)
	os.WriteFile("/tmp/firehose_writer_failed_print.log", []byte(errstr), 0644)
	fmt.Fprint(writer, errstr)
}

// FIXME: Create a unit test that is going to fail as soon as any header is added in
func newBlockHeaderFromChainHeader(h *types.Header, td *pbeth.BigInt) *pbeth.BlockHeader {
	var withdrawalsHashBytes []byte
	if hash := h.WithdrawalsHash; hash != nil {
		withdrawalsHashBytes = hash.Bytes()
	}

	pbHead := &pbeth.BlockHeader{
		Hash:             h.Hash().Bytes(),
		Number:           h.Number.Uint64(),
		ParentHash:       h.ParentHash.Bytes(),
		UncleHash:        h.UncleHash.Bytes(),
		Coinbase:         h.Coinbase.Bytes(),
		StateRoot:        h.Root.Bytes(),
		TransactionsRoot: h.TxHash.Bytes(),
		ReceiptRoot:      h.ReceiptHash.Bytes(),
		LogsBloom:        h.Bloom.Bytes(),
		Difficulty:       firehoseBigIntFromNative(h.Difficulty),
		TotalDifficulty:  td,
		GasLimit:         h.GasLimit,
		GasUsed:          h.GasUsed,
		Timestamp:        timestamppb.New(time.Unix(int64(h.Time), 0)),
		ExtraData:        h.Extra,
		MixHash:          h.MixDigest.Bytes(),
		Nonce:            h.Nonce.Uint64(),
		BaseFeePerGas:    firehoseBigIntFromNative(h.BaseFee),
		WithdrawalsRoot:  withdrawalsHashBytes,
	}

	if pbHead.Difficulty == nil {
		pbHead.Difficulty = &pbeth.BigInt{Bytes: []byte{0}}
	}

	return pbHead
}

// FIXME: Bring back Firehose test that ensures no new tx type are missed
func transactionTypeFromChainTxType(txType uint8) pbeth.TransactionTrace_Type {
	switch txType {
	case types.AccessListTxType:
		return pbeth.TransactionTrace_TRX_TYPE_ACCESS_LIST
	case types.DynamicFeeTxType:
		return pbeth.TransactionTrace_TRX_TYPE_DYNAMIC_FEE
	case types.LegacyTxType:
		return pbeth.TransactionTrace_TRX_TYPE_LEGACY
		// Add when enabled in a fork
	case types.BlobTxType:
		panic("blobs tx type not supported yet")
	case types.ArbitrumDepositTxType:
		return pbeth.TransactionTrace_TRX_TYPE_ARBITRUM_DEPOSIT
	case types.ArbitrumUnsignedTxType:
		return pbeth.TransactionTrace_TRX_TYPE_ARBITRUM_UNSIGNED
	case types.ArbitrumContractTxType:
		return pbeth.TransactionTrace_TRX_TYPE_ARBITRUM_CONTRACT
	case types.ArbitrumRetryTxType:
		return pbeth.TransactionTrace_TRX_TYPE_ARBITRUM_RETRY
	case types.ArbitrumSubmitRetryableTxType:
		return pbeth.TransactionTrace_TRX_TYPE_ARBITRUM_SUBMIT_RETRYABLE
	case types.ArbitrumInternalTxType:
		return pbeth.TransactionTrace_TRX_TYPE_ARBITRUM_INTERNAL
	case types.ArbitrumLegacyTxType:
		return pbeth.TransactionTrace_TRX_TYPE_ARBITRUM_LEGACY

	default:
		panic(fmt.Errorf("unknown transaction type %d", txType))
	}
}

func transactionStatusFromChainTxReceipt(txStatus uint64) pbeth.TransactionTraceStatus {
	switch txStatus {
	case types.ReceiptStatusSuccessful:
		return pbeth.TransactionTraceStatus_SUCCEEDED
	case types.ReceiptStatusFailed:
		return pbeth.TransactionTraceStatus_FAILED
	default:
		panic(fmt.Errorf("unknown transaction status %d", txStatus))
	}
}

func rootCallType(create bool) pbeth.CallType {
	if create {
		return pbeth.CallType_CREATE
	}

	return pbeth.CallType_CALL
}

func callTypeFromOpCode(typ vm.OpCode) pbeth.CallType {
	switch typ {
	case vm.CALL:
		return pbeth.CallType_CALL
	case vm.STATICCALL:
		return pbeth.CallType_STATIC
	case vm.DELEGATECALL:
		return pbeth.CallType_DELEGATE
	case vm.CREATE, vm.CREATE2:
		return pbeth.CallType_CREATE
	case vm.CALLCODE:
		return pbeth.CallType_CALLCODE
	}

	return pbeth.CallType_UNSPECIFIED
}

func newTxReceiptFromChain(receipt *types.Receipt) (out *pbeth.TransactionReceipt) {
	out = &pbeth.TransactionReceipt{
		StateRoot:         receipt.PostState,
		CumulativeGasUsed: receipt.CumulativeGasUsed,
		LogsBloom:         receipt.Bloom[:],
	}

	if len(receipt.Logs) > 0 {
		out.Logs = make([]*pbeth.Log, len(receipt.Logs))
		for i, log := range receipt.Logs {
			out.Logs[i] = &pbeth.Log{
				Address: log.Address.Bytes(),
				Topics: func() [][]byte {
					if len(log.Topics) == 0 {
						return nil
					}

					out := make([][]byte, len(log.Topics))
					for i, topic := range log.Topics {
						out[i] = topic.Bytes()
					}
					return out
				}(),
				Data:       log.Data,
				Index:      uint32(i),
				BlockIndex: uint32(log.Index),

				// Ordinal on transaction receipt logs is populated at the very end, so pairing
				// between call logs and receipt logs is made
			}
		}
	}

	return out
}

func newAccessListFromChain(accessList types.AccessList) (out []*pbeth.AccessTuple) {
	if len(accessList) == 0 {
		return nil
	}

	out = make([]*pbeth.AccessTuple, len(accessList))
	for i, tuple := range accessList {
		out[i] = &pbeth.AccessTuple{
			Address: tuple.Address.Bytes(),
			StorageKeys: func() [][]byte {
				out := make([][]byte, len(tuple.StorageKeys))
				for i, key := range tuple.StorageKeys {
					out[i] = key.Bytes()
				}
				return out
			}(),
		}
	}

	return
}

var balanceChangeReasonToPb = map[state.BalanceChangeReason]pbeth.BalanceChange_Reason{
	state.BalanceIncreaseRewardMineUncle:      pbeth.BalanceChange_REASON_REWARD_MINE_UNCLE,
	state.BalanceIncreaseRewardMineBlock:      pbeth.BalanceChange_REASON_REWARD_MINE_BLOCK,
	state.BalanceIncreaseDaoContract:          pbeth.BalanceChange_REASON_DAO_REFUND_CONTRACT,
	state.BalanceDecreaseDaoAccount:           pbeth.BalanceChange_REASON_DAO_ADJUST_BALANCE,
	state.BalanceChangeTransfer:               pbeth.BalanceChange_REASON_TRANSFER,
	state.BalanceIncreaseGenesisBalance:       pbeth.BalanceChange_REASON_GENESIS_BALANCE,
	state.BalanceDecreaseGasBuy:               pbeth.BalanceChange_REASON_GAS_BUY,
	state.BalanceIncreaseRewardTransactionFee: pbeth.BalanceChange_REASON_REWARD_TRANSACTION_FEE,
	state.BalanceIncreaseGasReturn:            pbeth.BalanceChange_REASON_GAS_REFUND,
	state.BalanceChangeTouchAccount:           pbeth.BalanceChange_REASON_TOUCH_ACCOUNT,
	state.BalanceIncreaseSelfdestruct:         pbeth.BalanceChange_REASON_SUICIDE_REFUND,
	state.BalanceDecreaseSelfdestruct:         pbeth.BalanceChange_REASON_SUICIDE_WITHDRAW,
	state.BalanceDecreaseSelfdestructBurn:     pbeth.BalanceChange_REASON_BURN,
	state.BalanceChangeWithdrawal:             pbeth.BalanceChange_REASON_WITHDRAWAL,

	state.BalanceChangeUnspecified: pbeth.BalanceChange_REASON_UNKNOWN,
}

func balanceChangeReasonFromChain(reason state.BalanceChangeReason) pbeth.BalanceChange_Reason {
	if r, ok := balanceChangeReasonToPb[reason]; ok {
		return r
	}

	panic(fmt.Errorf("unknown tracer balance change reason value '%d', check state.BalanceChangeReason so see to which constant it refers to", reason))
}

var gasChangeReasonToPb = map[vm.GasChangeReason]pbeth.GasChange_Reason{
	// Known Firehose issue: Those are new gas change trace that we were missing initially in our old
	// Firehose patch. See Known Firehose issue referenced eb1916a67d9bea03df16a7a3e2cfac72 for details
	// search for the id within this project to find back all links).
	//
	// New chain should uncomment the code below and remove the same assigments to UNKNOWN
	//
	// vm.GasChangeTxInitialBalance:     pbeth.GasChange_REASON_TX_INITIAL_BALANCE,
	// vm.GasChangeTxRefunds:            pbeth.GasChange_REASON_TX_REFUNDS,
	// vm.GasChangeTxLeftOverReturned:   pbeth.GasChange_REASON_TX_LEFT_OVER_RETURNED,
	// vm.GasChangeCallInitialBalance:   pbeth.GasChange_REASON_CALL_INITIAL_BALANCE,
	// vm.GasChangeCallLeftOverReturned: pbeth.GasChange_REASON_CALL_LEFT_OVER_RETURNED,
	vm.GasChangeTxInitialBalance:     pbeth.GasChange_REASON_UNKNOWN,
	vm.GasChangeTxRefunds:            pbeth.GasChange_REASON_UNKNOWN,
	vm.GasChangeTxLeftOverReturned:   pbeth.GasChange_REASON_UNKNOWN,
	vm.GasChangeCallInitialBalance:   pbeth.GasChange_REASON_UNKNOWN,
	vm.GasChangeCallLeftOverReturned: pbeth.GasChange_REASON_UNKNOWN,

	vm.GasChangeTxIntrinsicGas:          pbeth.GasChange_REASON_INTRINSIC_GAS,
	vm.GasChangeCallContractCreation:    pbeth.GasChange_REASON_CONTRACT_CREATION,
	vm.GasChangeCallContractCreation2:   pbeth.GasChange_REASON_CONTRACT_CREATION2,
	vm.GasChangeCallCodeStorage:         pbeth.GasChange_REASON_CODE_STORAGE,
	vm.GasChangeCallPrecompiledContract: pbeth.GasChange_REASON_PRECOMPILED_CONTRACT,
	vm.GasChangeCallStorageColdAccess:   pbeth.GasChange_REASON_STATE_COLD_ACCESS,
	vm.GasChangeCallLeftOverRefunded:    pbeth.GasChange_REASON_REFUND_AFTER_EXECUTION,
	vm.GasChangeCallFailedExecution:     pbeth.GasChange_REASON_FAILED_EXECUTION,

	// Ignored, we track them manually, newGasChange ensure that we panic if we see Unknown
	vm.GasChangeCallOpCode: pbeth.GasChange_REASON_UNKNOWN,
}

func gasChangeReasonFromChain(reason vm.GasChangeReason) pbeth.GasChange_Reason {
	if r, ok := gasChangeReasonToPb[reason]; ok {
		if r == pbeth.GasChange_REASON_UNKNOWN {
			panic(fmt.Errorf("tracer gas change reason value '%d' mapped to %s which is not accepted", reason, r))
		}

		return r
	}

	panic(fmt.Errorf("unknown tracer gas change reason value '%d', check vm.GasChangeReason so see to which constant it refers to", reason))
}

func maxFeePerGas(tx *types.Transaction) *pbeth.BigInt {
	switch tx.Type() {
	case types.LegacyTxType, types.AccessListTxType, types.ArbitrumDepositTxType, types.ArbitrumUnsignedTxType, types.ArbitrumContractTxType, types.ArbitrumRetryTxType, types.ArbitrumSubmitRetryableTxType, types.ArbitrumInternalTxType, types.ArbitrumLegacyTxType:
		return nil

	case types.DynamicFeeTxType, types.BlobTxType:
		return firehoseBigIntFromNative(tx.GasFeeCap())

	}

	panic(errUnhandledTransactionType("maxFeePerGas", tx.Type()))
}

func maxPriorityFeePerGas(tx *types.Transaction) *pbeth.BigInt {
	switch tx.Type() {
	case types.LegacyTxType, types.AccessListTxType, types.ArbitrumDepositTxType, types.ArbitrumUnsignedTxType, types.ArbitrumContractTxType, types.ArbitrumRetryTxType, types.ArbitrumSubmitRetryableTxType, types.ArbitrumInternalTxType, types.ArbitrumLegacyTxType:
		return nil

	case types.DynamicFeeTxType, types.BlobTxType:
		return firehoseBigIntFromNative(tx.GasTipCap())
	}

	panic(errUnhandledTransactionType("maxPriorityFeePerGas", tx.Type()))
}

func gasPrice(tx *types.Transaction, baseFee *big.Int) *pbeth.BigInt {
	switch tx.Type() {
	case types.LegacyTxType, types.AccessListTxType, types.ArbitrumDepositTxType, types.ArbitrumUnsignedTxType, types.ArbitrumContractTxType, types.ArbitrumRetryTxType, types.ArbitrumSubmitRetryableTxType, types.ArbitrumInternalTxType, types.ArbitrumLegacyTxType:
		return firehoseBigIntFromNative(tx.GasPrice())

	case types.DynamicFeeTxType, types.BlobTxType:
		if baseFee == nil {
			return firehoseBigIntFromNative(tx.GasPrice())
		}

		return firehoseBigIntFromNative(math.BigMin(new(big.Int).Add(tx.GasTipCap(), baseFee), tx.GasFeeCap()))
	}

	panic(errUnhandledTransactionType("gasPrice", tx.Type()))
}

func FirehoseDebug(msg string, args ...interface{}) {
	firehoseDebug(msg, args...)
}

func firehoseDebug(msg string, args ...interface{}) {
	if isFirehoseDebugEnabled {
		fmt.Fprintf(os.Stderr, "[Firehose] "+msg+"\n", args...)
	}
}

func firehoseTrace(msg string, args ...interface{}) {
	if isFirehoseTracerEnabled {
		fmt.Fprintf(os.Stderr, "[Firehose] "+msg+"\n", args...)
	}
}

// Ignore unused, we keep it around for debugging purposes
var _ = firehoseDebugPrintStack

func firehoseDebugPrintStack() {
	if isFirehoseDebugEnabled {
		fmt.Fprintf(os.Stderr, "[Firehose] Stacktrace\n")

		// PrintStack prints to Stderr
		debug.PrintStack()
	}
}

func errUnhandledTransactionType(tag string, value uint8) error {
	return fmt.Errorf("unhandled transaction type's %d for firehose.%s(), carefully review the patch, if this new transaction type add new fields, think about adding them to Firehose Block format, when you see this message, it means something changed in the chain model and great care and thinking most be put here to properly understand the changes and the consequences they bring for the instrumentation", value, tag)
}

type Ordinal struct {
	value uint64
}

// Reset resets the ordinal to zero.
func (o *Ordinal) Reset() {
	o.value = 0
}

// Next gives you the next sequential ordinal value that you should
// use to assign to your exeuction trace (block, transaction, call, etc).
func (o *Ordinal) Next() (out uint64) {
	o.value++

	return o.value
}

type CallStack struct {
	index uint32
	stack []*pbeth.Call
	depth int
}

func NewCallStack() *CallStack {
	return &CallStack{}
}

func (s *CallStack) Reset() {
	s.index = 0
	s.stack = s.stack[:0]
	s.depth = 0
}

func (s *CallStack) HasActiveCall() bool {
	return len(s.stack) > 0
}

// Push a call onto the stack. The `Index` and `ParentIndex` of this call are
// assigned by this method which knowns how to find the parent call and deal with
// it.
func (s *CallStack) Push(call *pbeth.Call) {
	s.index++
	call.Index = s.index

	call.Depth = uint32(s.depth)
	s.depth++

	// If a current call is active, it's the parent of this call
	if parent := s.Peek(); parent != nil {
		call.ParentIndex = parent.Index
	}

	s.stack = append(s.stack, call)
}

func (s *CallStack) ActiveIndex() uint32 {
	if len(s.stack) == 0 {
		return 0
	}

	return s.stack[len(s.stack)-1].Index
}

func (s *CallStack) NextIndex() uint32 {
	return s.index + 1
}

func (s *CallStack) Pop() (out *pbeth.Call) {
	if len(s.stack) == 0 {
		panic(fmt.Errorf("pop from empty call stack"))
	}

	out = s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]
	s.depth--

	return
}

// Peek returns the top of the stack without removing it, it's the
// activate call.
func (s *CallStack) Peek() *pbeth.Call {
	if len(s.stack) == 0 {
		return nil
	}

	return s.stack[len(s.stack)-1]
}

// DeferredCallState is a helper struct that can be used to accumulate call's state
// that is recorded before the Call has been started. This happens on the "starting"
// portion of the call/created.
type DeferredCallState struct {
	balanceChanges   []*pbeth.BalanceChange
	gasChanges       []*pbeth.GasChange
	nonceChanges     []*pbeth.NonceChange
	storageChanges   []*pbeth.StorageChange
	logs             []*pbeth.Log
	accountCreations []*pbeth.AccountCreation
}

func NewDeferredCallState() *DeferredCallState {
	return &DeferredCallState{}
}

func (d *DeferredCallState) MaybePopulateCallAndReset(source string, call *pbeth.Call) error {
	if d.IsEmpty() {
		return nil
	}

	if source != "root" {
		return fmt.Errorf("unexpected source for deferred call state, expected root but got %s, deferred call's state are always produced on the 'root' call", source)
	}

	// We must happen because it's populated at beginning of the call as well as at the very end
	call.AccountCreations = append(call.AccountCreations, d.accountCreations...)
	call.BalanceChanges = append(call.BalanceChanges, d.balanceChanges...)
	call.GasChanges = append(call.GasChanges, d.gasChanges...)
	call.StorageChanges = append(call.StorageChanges, d.storageChanges...)
	call.Logs = append(call.Logs, d.logs...)
	call.AccountCreations = append(call.AccountCreations, d.accountCreations...)
	call.NonceChanges = append(call.NonceChanges, d.nonceChanges...)

	d.Reset()

	return nil
}

func (d *DeferredCallState) IsEmpty() bool {
	return len(d.balanceChanges) == 0 && len(d.gasChanges) == 0 && len(d.nonceChanges) == 0 && len(d.storageChanges) == 0 && len(
		d.logs) == 0
}

func (d *DeferredCallState) Reset() {
	d.accountCreations = nil
	d.balanceChanges = nil
	d.gasChanges = nil
	d.storageChanges = nil
	d.logs = nil
	d.accountCreations = nil
	d.nonceChanges = nil
}

func ctxView(f *Firehose) _ctxView {
	return _ctxView{f}
}

type _ctxView struct {
	f *Firehose
}

func (v _ctxView) String() string {
	if v.f == nil {
		return "no firehose"
	}
	blk := "<no block>"
	if v.f.block != nil {
		blk = v.f.block.AsRef().String()
	}

	trx := "<no trx>"
	if v.f.transaction != nil {
		trx = eth.Hash(v.f.transaction.Hash).Pretty()
	}

	return fmt.Sprintf("ctx=[%s, %s]", blk, trx)
}

func errorView(err error) _errorView {
	return _errorView{err}
}

type _errorView struct {
	err error
}

func (e _errorView) String() string {
	if e.err == nil {
		return "<no error>"
	}

	return e.err.Error()
}

type inputView []byte

func (b inputView) String() string {
	if len(b) == 0 {
		return "<empty>"
	}

	if len(b) < 4 {
		return common.Bytes2Hex(b)
	}

	method := b[:4]
	rest := b[4:]

	if len(rest)%32 == 0 {
		return fmt.Sprintf("%s (%d params)", common.Bytes2Hex(method), len(rest)/32)
	}

	// Contract input starts with pre-defined chracters AFAIK, we could show them more nicely

	return fmt.Sprintf("%d bytes", len(rest))
}

type outputView []byte

func (b outputView) String() string {
	if len(b) == 0 {
		return "<empty>"
	}

	return fmt.Sprintf("%d bytes", len(b))
}

type receiptView types.Receipt

func (r *receiptView) String() string {
	if r == nil {
		return "<failed>"
	}

	status := "unknown"
	switch r.Status {
	case types.ReceiptStatusSuccessful:
		status = "success"
	case types.ReceiptStatusFailed:
		status = "failed"
	}

	return fmt.Sprintf("[status=%s, gasUsed=%d, logs=%d]", status, r.GasUsed, len(r.Logs))
}

func emptyBytesToNil(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}

	return in
}

func normalizeSignaturePoint(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}

	if len(value) < 32 {
		offset := 32 - len(value)

		out := make([]byte, 32)
		copy(out[offset:32], value)

		return out
	}

	return value[0:32]
}

func firehoseBigIntFromNative(in *big.Int) *pbeth.BigInt {
	if in == nil || in.Sign() == 0 {
		return nil
	}

	return &pbeth.BigInt{Bytes: in.Bytes()}
}

type FinalityStatus struct {
	LastIrreversibleBlockNumber uint64
	LastIrreversibleBlockHash   []byte
}

func (s *FinalityStatus) populateFromChain(num uint64, hash []byte) {
	if hash == nil {
		s.Reset()
		return
	}

	s.LastIrreversibleBlockNumber = num //finalHeader.Number.Uint64()
	s.LastIrreversibleBlockHash = hash  //finalHeader.Hash().Bytes()
}

// ToFirehoseLogParams converts the data into the format expected by Firehose reader,
// replacing the value with "." if the data is empty.
func (s *FinalityStatus) ToFirehoseLogParams() (libNum, libID string) {
	if s.IsEmpty() {
		return ".", "."
	}

	return strconv.FormatUint(s.LastIrreversibleBlockNumber, 10), hex.EncodeToString(s.LastIrreversibleBlockHash)
}

func (s *FinalityStatus) Reset() {
	s.LastIrreversibleBlockNumber = 0
	s.LastIrreversibleBlockHash = nil
}

func (s *FinalityStatus) IsEmpty() bool {
	return s.LastIrreversibleBlockNumber == 0 && len(s.LastIrreversibleBlockHash) == 0
}

var errFirehoseUnknownType = errors.New("firehose unknown tx type")
var sanitizeRegexp = regexp.MustCompile(`[\t( ){2,}]+`)

func staticFirehoseChainValidationOnInit() {
	firehoseKnownTxTypes := map[byte]bool{
		types.LegacyTxType:     true,
		types.AccessListTxType: true,
		types.DynamicFeeTxType: true,
		types.BlobTxType:       true,
		// these generate an error when trying to EncodeRLP
		//types.ArbitrumDepositTxType:         true,
		//types.ArbitrumUnsignedTxType:        true,
		//types.ArbitrumContractTxType:        true,
		//types.ArbitrumRetryTxType:           true,
		//types.ArbitrumSubmitRetryableTxType: true,
		//types.ArbitrumInternalTxType:        true,
		//types.ArbitrumLegacyTxType:          true,
	}

	for txType := byte(0); txType < 255; txType++ {
		err := validateFirehoseKnownTransactionType(txType, firehoseKnownTxTypes[txType])
		if err != nil {
			panic(fmt.Errorf(sanitizeRegexp.ReplaceAllString(`
				If you see this panic message, it comes from a sanity check of Firehose instrumentation
				around Ethereum transaction types.

				Over time, Ethereum added new transaction types but there is no easy way for Firehose to
				report a compile time check that a new transaction's type must be handled. As such, we
				have a runtime check at initialization of the process that encode/decode each possible
				transaction's receipt and check proper handling.

				This panic means that a transaction that Firehose don't know about has most probably
				been added and you must take **great care** to instrument it. One of the most important place
				to look is in 'firehose.StartTransaction' where it should be properly handled. Think
				carefully, read the EIP and ensure that any new "semantic" the transactions type's is
				bringing is handled and instrumented (it might affect Block and other execution units also).

				For example, when London fork appeared, semantic of 'GasPrice' changed and it required
				a different computation for 'GasPrice' when 'DynamicFeeTx' transaction were added. If you determined
				it was indeed a new transaction's type, fix 'firehoseKnownTxTypes' variable above to include it
				as a known Firehose type (after proper instrumentation of course).

				It's also possible the test itself is now flaky, we do 'receipt := types.Receipt{Type: <type>}'
				then 'buffer := receipt.EncodeRLP(...)' and then 'receipt.DecodeRLP(buffer)'. This should catch
				new transaction types but could be now generate false positive.

				Received error: %w
			`, " "), err))
		}
	}
}

func validateFirehoseKnownTransactionType(txType byte, isKnownFirehoseTxType bool) error {
	writerBuffer := bytes.NewBuffer(nil)

	receipt := types.Receipt{Type: txType}
	err := receipt.EncodeRLP(writerBuffer)
	if err != nil {
		if err == types.ErrTxTypeNotSupported {
			if isKnownFirehoseTxType {
				return fmt.Errorf("firehose known type but encoding RLP of receipt led to 'types.ErrTxTypeNotSupported'")
			}

			// It's not a known type and encoding reported the same, so validation is OK
			return nil
		}

		// All other cases results in an error as we should have been able to encode it to RLP
		return fmt.Errorf("encoding RLP: %w", err)
	}

	readerBuffer := bytes.NewBuffer(writerBuffer.Bytes())
	err = receipt.DecodeRLP(rlp.NewStream(readerBuffer, 0))
	if err != nil {
		if err == types.ErrTxTypeNotSupported {
			if isKnownFirehoseTxType {
				return fmt.Errorf("firehose known type but decoding of RLP of receipt led to 'types.ErrTxTypeNotSupported'")
			}

			// It's not a known type and decoding reported the same, so validation is OK
			return nil
		}

		// All other cases results in an error as we should have been able to decode it from RLP
		return fmt.Errorf("decoding RLP: %w", err)
	}

	// If we reach here, encoding/decoding accepted the transaction's type, so let's ensure we expected the same
	if !isKnownFirehoseTxType {
		return fmt.Errorf("unknown tx type value %d: %w", txType, errFirehoseUnknownType)
	}

	return nil
}

type validationResult struct {
	failures []string
}

func (r *validationResult) panicOnAnyFailures(msg string, args ...any) {
	if len(r.failures) > 0 {
		panic(fmt.Errorf(fmt.Sprintf(msg, args...)+": validation failed:\n %s", strings.Join(r.failures, "\n")))
	}
}

// We keep them around, planning in the future to use them (they existed in the previous Firehose patch)
var _, _, _, _, _ = validateAddressField, validateBigIntField, validateHashField, validateUint64Field, validateUint32Field

func validateAddressField(into *validationResult, field string, a, b common.Address) {
	validateField(into, field, a, b, a == b, common.Address.String)
}

func validateBigIntField(into *validationResult, field string, a, b *big.Int) {
	equal := false
	if a == nil && b == nil {
		equal = true
	} else if a == nil || b == nil {
		equal = false
	} else {
		equal = a.Cmp(b) == 0
	}

	validateField(into, field, a, b, equal, func(x *big.Int) string {
		if x == nil {
			return "<nil>"
		} else {
			return x.String()
		}
	})
}

func validateBytesField(into *validationResult, field string, a, b []byte) {
	validateField(into, field, a, b, bytes.Equal(a, b), common.Bytes2Hex)
}

func validateArrayOfBytesField(into *validationResult, field string, a, b [][]byte) {
	if len(a) != len(b) {
		into.failures = append(into.failures, fmt.Sprintf("%s [(actual element) %d != %d (expected element)]", field, len(a), len(b)))
		return
	}

	for i := range a {
		validateBytesField(into, fmt.Sprintf("%s[%d]", field, i), a[i], b[i])
	}
}

func validateHashField(into *validationResult, field string, a, b common.Hash) {
	validateField(into, field, a, b, a == b, common.Hash.String)
}

func validateUint32Field(into *validationResult, field string, a, b uint32) {
	validateField(into, field, a, b, a == b, func(x uint32) string { return strconv.FormatUint(uint64(x), 10) })
}

func validateUint64Field(into *validationResult, field string, a, b uint64) {
	validateField(into, field, a, b, a == b, func(x uint64) string { return strconv.FormatUint(x, 10) })
}

// validateField, pays the price for failure message construction only when field are not equal
func validateField[T any](into *validationResult, field string, a, b T, equal bool, toString func(x T) string) {
	if !equal {
		into.failures = append(into.failures, fmt.Sprintf("%s [(actual) %s %s %s (expected)]", field, toString(a), "!=", toString(b)))
	}
}

// This is copied from https://cs.opensource.google/go/go/+/refs/tags/go1.21.0:src/cmp/cmp.go
// allows building without go 1.21
// Ordered is a constraint that permits any ordered type: any type
// that supports the operators < <= >= >.
// If future releases of Go add new ordered types,
// this constraint will be modified to include them.
//
// Note that floating-point types may contain NaN ("not-a-number") values.
// An operator such as == or < will always report false when
// comparing a NaN value with any other value, NaN or not.
// See the [Compare] function for a consistent way to compare NaN values.
type Ordered interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr |
		~float32 | ~float64 |
		~string
}

// Less reports whether x is less than y.
// For floating-point types, a NaN is considered less than any non-NaN,
// and -0.0 is not less than (is equal to) 0.0.
func Less[T Ordered](x, y T) bool {
	return (isNaN(x) && !isNaN(y)) || x < y
}

// Compare returns
//
//	-1 if x is less than y,
//	 0 if x equals y,
//	+1 if x is greater than y.
//
// For floating-point types, a NaN is considered less than any non-NaN,
// a NaN is considered equal to a NaN, and -0.0 is equal to 0.0.
func Compare[T Ordered](x, y T) int {
	xNaN := isNaN(x)
	yNaN := isNaN(y)
	if xNaN && yNaN {
		return 0
	}
	if xNaN || x < y {
		return -1
	}
	if yNaN || x > y {
		return +1
	}
	return 0
}

// isNaN reports whether x is a NaN without requiring the math package.
// This will always return false if T is not floating-point.
func isNaN[T Ordered](x T) bool {
	return x != x
}
