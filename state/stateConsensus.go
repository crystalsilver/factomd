// Copyright 2017 Factom Foundation
// Use of this source code is governed by the MIT
// license that can be found in the LICENSE file.

package state

import (
	"errors"
	"fmt"
	"hash"
	"time"

	"github.com/FactomProject/factomd/common/constants"
	"github.com/FactomProject/factomd/common/entryBlock"
	"github.com/FactomProject/factomd/common/entryCreditBlock"
	"github.com/FactomProject/factomd/common/interfaces"
	"github.com/FactomProject/factomd/common/messages"
	"github.com/FactomProject/factomd/common/primitives"
	"github.com/FactomProject/factomd/database/databaseOverlay"
	"github.com/FactomProject/factomd/util"

	log "github.com/sirupsen/logrus"
)

// consenLogger is the general logger for all consensus related logs. You can add additional fields,
// or create more context loggers off of this
var consenLogger = packageLogger.WithFields(log.Fields{"subpack": "consensus"})

var _ = fmt.Print
var _ = (*hash.Hash32)(nil)

//***************************************************************
// Process Loop for Consensus
//
// Returns true if some message was processed.
//***************************************************************

func (s *State) executeMsg(vm *VM, msg interfaces.IMsg) (ret bool) {
	preExecuteMsgTime := time.Now()
	_, ok := s.Replay.Valid(constants.INTERNAL_REPLAY, msg.GetRepeatHash().Fixed(), msg.GetTimestamp(), s.GetTimestamp())
	if !ok {
		consenLogger.WithFields(msg.LogFields()).Debug("ExecuteMsg (Replay Invalid)")
		return
	}
	s.SetString()
	msg.ComputeVMIndex(s)

	if s.IgnoreMissing {
		now := s.GetTimestamp().GetTimeSeconds()
		if now-msg.GetTimestamp().GetTimeSeconds() > 60*15 {
			return
		}
	}

	switch msg.Validate(s) {
	case 1:
		if s.RunLeader &&
			s.Leader &&
			!s.Saving &&
			vm != nil && int(vm.Height) == len(vm.List) &&
			(!s.Syncing || !vm.Synced) &&
			(msg.IsLocal() || msg.GetVMIndex() == s.LeaderVMIndex) &&
			s.LeaderPL.DBHeight+1 >= s.GetHighestKnownBlock() {
			if len(vm.List) == 0 {
				s.SendDBSig(s.LLeaderHeight, s.LeaderVMIndex)
				TotalXReviewQueueInputs.Inc()
				s.XReview = append(s.XReview, msg)
			} else {
				msg.LeaderExecute(s)
			}
		} else {
			msg.FollowerExecute(s)
		}
		ret = true
	case 0:
		TotalHoldingQueueInputs.Inc()
		TotalHoldingQueueRecycles.Inc()
		s.Holding[msg.GetMsgHash().Fixed()] = msg
	default:
		TotalHoldingQueueInputs.Inc()
		TotalHoldingQueueRecycles.Inc()
		s.Holding[msg.GetMsgHash().Fixed()] = msg
		if !msg.SentInvalid() {
			msg.MarkSentInvalid(true)
			s.networkInvalidMsgQueue <- msg
		}
	}

	executeMsgTime := time.Since(preExecuteMsgTime)
	TotalExecuteMsgTime.Add(float64(executeMsgTime.Nanoseconds()))

	return

}

func (s *State) Process() (progress bool) {

	if s.ResetRequest {
		s.ResetRequest = false
		s.DoReset()
		return false
	}

	// If we are not running the leader, then look to see if we have waited long enough to
	// start running the leader.  If we are, start the clock on Ignoring Missing Messages.  This
	// is so we don't conflict with past version of the network if we have to reboot the network.
	if !s.RunLeader {
		now := s.GetTimestamp().GetTimeMilli() // Timestamps are in milliseconds, so wait 20
		if now-s.StartDelay > s.StartDelayLimit {
			if s.DBFinished == true {
				s.RunLeader = true
				if !s.IgnoreDone {
					s.StartDelay = now // Reset StartDelay for Ignore Missing
					s.IgnoreDone = true
				}
			}
		}
		s.LeaderPL = s.ProcessLists.Get(s.LLeaderHeight)
		if s.CurrentMinute > 9 {
			s.Leader, s.LeaderVMIndex = s.LeaderPL.GetVirtualServers(9, s.IdentityChainID)
		} else {
			s.Leader, s.LeaderVMIndex = s.LeaderPL.GetVirtualServers(s.CurrentMinute, s.IdentityChainID)
		}
	} else if s.IgnoreMissing {
		s.LeaderPL = s.ProcessLists.Get(s.LLeaderHeight)
		if s.CurrentMinute > 9 {
			s.Leader, s.LeaderVMIndex = s.LeaderPL.GetVirtualServers(9, s.IdentityChainID)
		} else {
			s.Leader, s.LeaderVMIndex = s.LeaderPL.GetVirtualServers(s.CurrentMinute, s.IdentityChainID)
		}
		now := s.GetTimestamp().GetTimeMilli() // Timestamps are in milliseconds, so wait 20
		if now-s.StartDelay > s.StartDelayLimit {
			s.IgnoreMissing = false
		}
	}

	process := make(chan interfaces.IMsg, 10000)
	room := func() bool { return len(process) < 9995 }

	var vm *VM
	if s.Leader {
		vm = s.LeaderPL.VMs[s.LeaderVMIndex]
		if vm.Height == 0 {
			s.SendDBSig(s.LeaderPL.DBHeight, s.LeaderVMIndex)
		}
	}

	/** Process all the DBStates  that might be pending **/

	for room() {
		ix := int(s.GetHighestSavedBlk()) - s.DBStatesReceivedBase + 1
		if ix < 0 || ix >= len(s.DBStatesReceived) {
			break
		}
		msg := s.DBStatesReceived[ix]
		if msg == nil {
			break
		}
		process <- msg
		s.DBStatesReceived[ix] = nil
	}

	s.ReviewHolding()

	preAckLoopTime := time.Now()
	// Process acknowledgements if we have some.
ackLoop:
	for room() {
		select {
		case ack := <-s.ackQueue:
			a := ack.(*messages.Ack)
			if a.DBHeight >= s.LLeaderHeight && ack.Validate(s) == 1 {
				if s.IgnoreMissing {
					now := s.GetTimestamp().GetTimeSeconds()
					if now-a.GetTimestamp().GetTimeSeconds() < 60*15 {
						s.executeMsg(vm, ack)
					}
				} else {
					s.executeMsg(vm, ack)
				}
			}
			progress = true
		default:
			break ackLoop
		}
	}

	ackLoopTime := time.Since(preAckLoopTime)
	TotalAckLoopTime.Add(float64(ackLoopTime.Nanoseconds()))

	preEmptyLoopTime := time.Now()

	// Process inbound messages
emptyLoop:
	for room() {
		select {
		case msg := <-s.msgQueue:

			if s.executeMsg(vm, msg) && !msg.IsPeer2Peer() {
				msg.SendOut(s, msg)
			}
		default:
			break emptyLoop
		}
	}
	emptyLoopTime := time.Since(preEmptyLoopTime)
	TotalEmptyLoopTime.Add(float64(emptyLoopTime.Nanoseconds()))

	preProcessXReviewTime := time.Now()
	// Reprocess any stalled messages, but not so much compared inbound messages
	// Process last first
skipreview:
	for {
		for _, msg := range s.XReview {
			if !room() {
				break skipreview
			}
			if msg == nil {
				continue
			}
			process <- msg
			progress = s.executeMsg(vm, msg) || progress
		}
		s.XReview = s.XReview[:0]
		break
	}
	processXReviewTime := time.Since(preProcessXReviewTime)
	TotalProcessXReviewTime.Add(float64(processXReviewTime.Nanoseconds()))

	preProcessProcChanTime := time.Now()
	for len(process) > 0 {
		msg := <-process
		s.executeMsg(vm, msg)
		if !msg.IsPeer2Peer() {
			msg.SendOut(s, msg)
		}
		s.UpdateState()
	}

	processProcChanTime := time.Since(preProcessProcChanTime)
	TotalProcessProcChanTime.Add(float64(processProcChanTime.Nanoseconds()))

	return
}

//***************************************************************
// Checkpoint DBKeyMR
//***************************************************************
func CheckDBKeyMR(s *State, ht uint32, hash string) error {
	if s.Network != "MAIN" && s.Network != "main" {
		return nil
	}
	if val, ok := constants.CheckPoints[ht]; ok {
		if val != hash {
			return fmt.Errorf("%20s CheckPoints at %d DB height failed\n", s.FactomNodeName, ht)
		}
	}
	return nil
}

//***************************************************************
// Consensus Methods
//***************************************************************

// Places the entries in the holding map back into the XReview list for
// review if this is a leader, and those messages are that leader's
// responsibility
func (s *State) ReviewHolding() {
	preReviewHoldingTime := time.Now()
	if len(s.XReview) > 0 {
		return
	}

	if s.inMsgQueue.Length() > constants.INMSGQUEUE_LOW {
		return
	}

	now := s.GetTimestamp()
	if s.ResendHolding == nil {
		s.ResendHolding = now
	}
	if now.GetTimeMilli()-s.ResendHolding.GetTimeMilli() < 300 {
		return
	}

	s.DB.Trim()

	s.ResendHolding = now
	// Anything we are holding, we need to reprocess.
	s.XReview = make([]interfaces.IMsg, 0)

	highest := s.GetHighestKnownBlock()
	saved := s.GetHighestSavedBlk()

	for k, v := range s.Holding {

		if int(highest)-int(saved) > 1000 {
			TotalHoldingQueueOutputs.Inc()
			delete(s.Holding, k)
		}

		mm, ok := v.(*messages.MissingMsgResponse)
		if ok {
			ff, ok := mm.MsgResponse.(*messages.FullServerFault)
			if ok && ff.DBHeight < saved {
				TotalHoldingQueueOutputs.Inc()
				delete(s.Holding, k)
			}
			continue
		}

		sf, ok := v.(*messages.ServerFault)
		if ok && sf.DBHeight < saved {
			TotalHoldingQueueOutputs.Inc()
			delete(s.Holding, k)
			continue
		}

		ff, ok := v.(*messages.FullServerFault)
		if ok && ff.DBHeight < saved {
			TotalHoldingQueueOutputs.Inc()
			delete(s.Holding, k)
			continue
		}

		eom, ok := v.(*messages.EOM)
		if ok && ((eom.DBHeight <= saved && saved > 0) || (eom.DBHeight < highest-3 && highest > 2)) {
			TotalHoldingQueueOutputs.Inc()
			delete(s.Holding, k)
			continue
		}

		dbsmsg, ok := v.(*messages.DBStateMsg)
		if ok && (dbsmsg.DirectoryBlock.GetHeader().GetDBHeight() < saved-1 && saved > 0) {
			TotalHoldingQueueOutputs.Inc()
			delete(s.Holding, k)
			continue
		}

		dbsigmsg, ok := v.(*messages.DirectoryBlockSignature)
		if ok && ((dbsigmsg.DBHeight <= saved && saved > 0) || (dbsigmsg.DBHeight < highest-3 && highest > 2)) {
			TotalHoldingQueueOutputs.Inc()
			delete(s.Holding, k)
			continue
		}

		_, ok = s.Replay.Valid(constants.INTERNAL_REPLAY, v.GetRepeatHash().Fixed(), v.GetTimestamp(), s.GetTimestamp())
		if !ok {
			TotalHoldingQueueOutputs.Inc()
			delete(s.Holding, k)
			continue
		}

		if v.Expire(s) {
			s.ExpireCnt++
			TotalHoldingQueueOutputs.Inc()
			delete(s.Holding, k)
			continue
		}

		if v.Resend(s) {
			if v.Validate(s) == 1 {
				s.ResendCnt++
				v.SendOut(s, v)
				continue
			}
		}

		if v.Validate(s) < 0 {
			TotalHoldingQueueOutputs.Inc()
			delete(s.Holding, k)
			continue
		}
		TotalXReviewQueueInputs.Inc()
		s.XReview = append(s.XReview, v)
		TotalHoldingQueueOutputs.Inc()
		delete(s.Holding, k)
	}
	reviewHoldingTime := time.Since(preReviewHoldingTime)
	TotalReviewHoldingTime.Add(float64(reviewHoldingTime.Nanoseconds()))
}

// Adds blocks that are either pulled locally from a database, or acquired from peers.
func (s *State) AddDBState(isNew bool,
	directoryBlock interfaces.IDirectoryBlock,
	adminBlock interfaces.IAdminBlock,
	factoidBlock interfaces.IFBlock,
	entryCreditBlock interfaces.IEntryCreditBlock,
	eBlocks []interfaces.IEntryBlock,
	entries []interfaces.IEBEntry) *DBState {

	dbState := s.DBStates.NewDBState(isNew, directoryBlock, adminBlock, factoidBlock, entryCreditBlock, eBlocks, entries)

	if dbState == nil {
		//s.AddStatus(fmt.Sprintf("AddDBState(): Fail dbstate is nil at dbht: %d", directoryBlock.GetHeader().GetDBHeight()))
		return nil
	}

	ht := dbState.DirectoryBlock.GetHeader().GetDBHeight()
	DBKeyMR := dbState.DirectoryBlock.GetKeyMR().String()

	err := CheckDBKeyMR(s, ht, DBKeyMR)
	if err != nil {
		panic(fmt.Errorf("Found block at height %d that didn't match a checkpoint. Got %s, expected %s", ht, DBKeyMR, constants.CheckPoints[ht])) //TODO make failing when given bad blocks fail more elegantly
	}

	if ht > s.LLeaderHeight {
		s.Syncing = false
		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s Add DBState: s.EOM(%v)", s.FactomNodeName, s.EOM))
		s.EOM = false
		s.DBSig = false
		s.LLeaderHeight = ht
		s.ProcessLists.Get(ht + 1)
		s.CurrentMinute = 0
		s.EOMProcessed = 0
		s.DBSigProcessed = 0
		s.StartDelay = s.GetTimestamp().GetTimeMilli()
		s.RunLeader = false
		s.LeaderPL = s.ProcessLists.Get(s.LLeaderHeight)

		{
			// Okay, we have just loaded a new DBState.  The temp balances are no longer valid, if they exist.  Nuke them.
			s.LeaderPL.FactoidBalancesTMutex.Lock()
			defer s.LeaderPL.FactoidBalancesTMutex.Unlock()

			s.LeaderPL.ECBalancesTMutex.Lock()
			defer s.LeaderPL.ECBalancesTMutex.Unlock()

			s.LeaderPL.FactoidBalancesT = map[[32]byte]int64{}
			s.LeaderPL.ECBalancesT = map[[32]byte]int64{}
		}

		s.Leader, s.LeaderVMIndex = s.LeaderPL.GetVirtualServers(s.CurrentMinute, s.IdentityChainID)
		for s.ProcessLists.UpdateState(s.LLeaderHeight) {
		}
	}
	if ht == 0 && s.LLeaderHeight < 1 {
		s.LLeaderHeight = 1
	}

	return dbState
}

// Messages that will go into the Process List must match an Acknowledgement.
// The code for this is the same for all such messages, so we put it here.
//
// Returns true if it finds a match, puts the message in holding, or invalidates the message
func (s *State) FollowerExecuteMsg(m interfaces.IMsg) {
	FollowerExecutions.Inc()
	TotalHoldingQueueInputs.Inc()
	s.Holding[m.GetMsgHash().Fixed()] = m
	ack, _ := s.Acks[m.GetMsgHash().Fixed()].(*messages.Ack)

	if ack != nil {
		m.SetLeaderChainID(ack.GetLeaderChainID())
		m.SetMinute(ack.Minute)

		pl := s.ProcessLists.Get(ack.DBHeight)
		pl.AddToProcessList(ack, m)
	}
}

// Messages that will go into the Process List must match an Acknowledgement.
// The code for this is the same for all such messages, so we put it here.
//
// Returns true if it finds a match, puts the message in holding, or invalidates the message
func (s *State) FollowerExecuteEOM(m interfaces.IMsg) {

	if m.IsLocal() {
		return // This is an internal EOM message.  We are not a leader so ignore.
	}

	FollowerEOMExecutions.Inc()
	TotalHoldingQueueInputs.Inc()
	s.Holding[m.GetMsgHash().Fixed()] = m

	ack, _ := s.Acks[m.GetMsgHash().Fixed()].(*messages.Ack)
	if ack != nil {
		pl := s.ProcessLists.Get(ack.DBHeight)
		pl.AddToProcessList(ack, m)
	}
}

// Ack messages always match some message in the Process List.   That is
// done here, though the only msg that should call this routine is the Ack
// message.
func (s *State) FollowerExecuteAck(msg interfaces.IMsg) {
	ack := msg.(*messages.Ack)

	if ack.DBHeight > s.HighestKnown {
		s.HighestKnown = ack.DBHeight
	}

	pl := s.ProcessLists.Get(ack.DBHeight)
	if pl == nil {
		return
	}
	list := pl.VMs[ack.VMIndex].List
	if len(list) > int(ack.Height) && list[ack.Height] != nil {
		return
	}

	TotalAcksInputs.Inc()
	s.Acks[ack.GetHash().Fixed()] = ack
	m, _ := s.Holding[ack.GetHash().Fixed()]
	if m != nil {
		m.FollowerExecute(s)
	}
}

func (s *State) ExecuteEntriesInDBState(dbmsg *messages.DBStateMsg) {
	height := dbmsg.DirectoryBlock.GetDatabaseHeight()

	if s.EntryDBHeightComplete > height {
		return
	}
	// If no Eblocks, leave
	if len(dbmsg.EBlocks) == 0 {
		return
	}

	// All DBStates that got here are valid, so just checking the DBlock hash works
	dblock, err := s.DB.FetchDBlockByHeight(height)
	if err != nil || dblock == nil {
		consenLogger.WithFields(log.Fields{"func": "ExecuteEntriesInDBState", "height": height}).Warnf("Dblock fetched is nil")
		return // This is a werid case
	}

	if !dbmsg.DirectoryBlock.GetHash().IsSameAs(dblock.GetHash()) {
		consenLogger.WithFields(log.Fields{"func": "ExecuteEntriesInDBState", "height": height}).Errorf("Bad DBState. DBlock does not match found")
		return // Bad DBlock
	}

	s.DB.StartMultiBatch()
	for _, e := range dbmsg.Entries {
		if exists, _ := s.DB.DoesKeyExist(databaseOverlay.ENTRY, e.GetHash().Bytes()); !exists {
			s.DB.InsertEntryMultiBatch(e)
		}
	}
	err = s.DB.ExecuteMultiBatch()
	if err != nil {
		consenLogger.WithFields(log.Fields{"func": "ExecuteEntriesInDBState", "height": height}).Errorf("Was unable to execute multibatch")
		return
	}
}

func (s *State) FollowerExecuteDBState(msg interfaces.IMsg) {
	dbstatemsg, _ := msg.(*messages.DBStateMsg)

	cntFail := func() {
		if !dbstatemsg.IsInDB {
			s.DBStateIgnoreCnt++
		}
	}

	// saved := s.GetHighestSavedBlk()

	dbheight := dbstatemsg.DirectoryBlock.GetHeader().GetDBHeight()

	// ignore if too old. If its under EntryDBHeightComplete
	if dbheight > 0 && dbheight <= s.GetHighestSavedBlk() && dbheight < s.EntryDBHeightComplete {
		return
	}

	//s.AddStatus(fmt.Sprintf("FollowerExecuteDBState(): Saved %d dbht: %d", saved, dbheight))

	pdbstate := s.DBStates.Get(int(dbheight - 1))

	switch pdbstate.ValidNext(s, dbstatemsg) {
	case 0:
		//s.AddStatus(fmt.Sprintf("FollowerExecuteDBState(): DBState might be valid %d", dbheight))

		// Don't add duplicate dbstate messages.
		if s.DBStatesReceivedBase < int(s.GetHighestSavedBlk()) {
			cut := int(s.GetHighestSavedBlk()) - s.DBStatesReceivedBase
			if len(s.DBStatesReceived) > cut {
				s.DBStatesReceived = append(make([]*messages.DBStateMsg, 0), s.DBStatesReceived[cut:]...)
			}
			s.DBStatesReceivedBase += cut
		}
		ix := int(dbheight) - s.DBStatesReceivedBase
		if ix < 0 {
			// If we are missing entries at this DBState, we can apply the entries only
			s.ExecuteEntriesInDBState(dbstatemsg)
			return
		}
		for len(s.DBStatesReceived) <= ix {
			s.DBStatesReceived = append(s.DBStatesReceived, nil)
		}
		s.DBStatesReceived[ix] = dbstatemsg
		return
	case -1:
		//s.AddStatus(fmt.Sprintf("FollowerExecuteDBState(): DBState is invalid at ht %d", dbheight))
		// Do nothing because this dbstate looks to be invalid
		cntFail()
		return
	}

	/**************************
	for int(s.ProcessLists.DBHeightBase)+len(s.ProcessLists.Lists) > int(dbheight+1) {
		s.ProcessLists.Lists[len(s.ProcessLists.Lists)-1].Clear()
		s.ProcessLists.Lists = s.ProcessLists.Lists[:len(s.ProcessLists.Lists)-1]
	}
	***************************/
	if dbheight > 1 && dbheight >= s.ProcessLists.DBHeightBase {
		dbs := s.DBStates.Get(int(dbheight))
		if pdbstate.SaveStruct != nil {
			//s.AddStatus(fmt.Sprintf("FollowerExecuteDBState(): Reset to previous state before applying at ht %d", dbheight))
			pdbstate.SaveStruct.TrimBack(s, dbs)
		}
	}

	dbstate := s.AddDBState(false,
		dbstatemsg.DirectoryBlock,
		dbstatemsg.AdminBlock,
		dbstatemsg.FactoidBlock,
		dbstatemsg.EntryCreditBlock,
		dbstatemsg.EBlocks,
		dbstatemsg.Entries)
	if dbstate == nil {
		//s.AddStatus(fmt.Sprintf("FollowerExecuteDBState(): dbstate fail at ht %d", dbheight))
		cntFail()
		return
	}

	// Check all the transaction IDs (do not include signatures).  Only check, don't set flags.
	for i, fct := range dbstatemsg.FactoidBlock.GetTransactions() {
		// Check the prior blocks for a replay.
		_, valid := s.FReplay.Valid(
			constants.BLOCK_REPLAY,
			fct.GetSigHash().Fixed(),
			fct.GetTimestamp(),
			dbstatemsg.DirectoryBlock.GetHeader().GetTimestamp())
		// If not the coinbase TX, and we are past 100,000, and the TX is not valid,then we don't accept this block.
		if i > 0 && // Don't test the coinbase TX
			((dbheight > 0 && dbheight < 2000) || dbheight > 100000) && // Test the first 2000 blks, so we can unit test, then after
			!valid { // 100K for the running system.  If a TX isn't valid, ignore.
			return //Totally ignore the block if it has a double spend.
		}
	}

	// Only set the flag if we know the whole block is valid.  We know it is because we checked them all in the loop
	// above
	for _, fct := range dbstatemsg.FactoidBlock.GetTransactions() {
		s.FReplay.IsTSValid_(
			constants.BLOCK_REPLAY,
			fct.GetSigHash().Fixed(),
			fct.GetTimestamp(),
			dbstatemsg.DirectoryBlock.GetHeader().GetTimestamp())
	}

	if dbstatemsg.IsInDB == false {
		//s.AddStatus(fmt.Sprintf("FollowerExecuteDBState(): dbstate added from network at ht %d", dbheight))
		dbstate.ReadyToSave = true
		dbstate.Locked = false
		dbstate.Signed = true
		s.DBStateAppliedCnt++
		s.DBStates.UpdateState()
	} else {
		//s.AddStatus(fmt.Sprintf("FollowerExecuteDBState(): dbstate added from local db at ht %d", dbheight))
		dbstate.Saved = true
		dbstate.IsNew = false
		dbstate.Locked = false
	}

	//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s Clear EOM follower execute DBState:  !s.EOM(%v)", s.FactomNodeName, s.EOM))
	s.EOM = false
	s.EOMDone = false
	s.EOMSys = false
	s.DBSig = false
	s.DBSigDone = false
	s.DBSigSys = false
	s.Saving = true
	s.Syncing = false

	// Hurry up our next ask.  When we get to where we have the data we aksed for, then go ahead and ask for the next set.
	if s.DBStates.LastEnd < int(dbheight) {
		s.DBStates.Catchup(true)
	}
	if s.DBStates.LastBegin < int(dbheight)+1 {
		s.DBStates.LastBegin = int(dbheight)
	}
	s.DBStates.TimeToAsk = nil

	if dbstatemsg.IsLocal() {
		if s.StateSaverStruct.FastBoot {
			dbstate.SaveStruct = SaveFactomdState(s, dbstate)

			err := s.StateSaverStruct.SaveDBStateList(s.DBStates, s.Network)
			if err != nil {
				panic(err)
			}
		}
	}
}

func (s *State) FollowerExecuteMMR(m interfaces.IMsg) {

	// Just ignore missing messages for a period after going off line or starting up.
	if s.IgnoreMissing || s.inMsgQueue.Length() > constants.INMSGQUEUE_HIGH {
		return
	}

	mmr, _ := m.(*messages.MissingMsgResponse)

	fullFault, ok := mmr.MsgResponse.(*messages.FullServerFault)
	if ok && fullFault != nil {
		switch fullFault.Validate(s) {
		case 1:
			pl := s.ProcessLists.Get(fullFault.DBHeight)
			if pl != nil && fullFault.HasEnoughSigs(s) && s.pledgedByAudit(fullFault) {
				_, okff := s.Replay.Valid(constants.INTERNAL_REPLAY, fullFault.GetRepeatHash().Fixed(), fullFault.GetTimestamp(), s.GetTimestamp())

				if okff {
					TotalXReviewQueueInputs.Inc()
					s.XReview = append(s.XReview, fullFault)
				} else {
					pl.AddToSystemList(fullFault)
				}
				s.MissingResponseAppliedCnt++
			} else if pl != nil && int(fullFault.Height) >= pl.System.Height {
				TotalXReviewQueueInputs.Inc()
				s.XReview = append(s.XReview, fullFault)
				s.MissingResponseAppliedCnt++
			}

		default:
			// Ignore if 0 or -1 or anything. If 0, I can ask for it again if I need it.
		}
		return
	}

	ack, ok := mmr.AckResponse.(*messages.Ack)

	// If we don't need this message, we don't have to do everything else.
	if !ok || ack.Validate(s) == -1 {
		return
	}

	ack.Response = true
	msg := mmr.MsgResponse

	if msg == nil {
		return
	}

	pl := s.ProcessLists.Get(ack.DBHeight)
	_, okr := s.Replay.Valid(constants.INTERNAL_REPLAY, ack.GetRepeatHash().Fixed(), ack.GetTimestamp(), s.GetTimestamp())
	_, okm := s.Replay.Valid(constants.INTERNAL_REPLAY, msg.GetRepeatHash().Fixed(), msg.GetTimestamp(), s.GetTimestamp())

	if pl == nil {
		return
	}

	TotalAcksInputs.Inc()
	s.Acks[ack.GetHash().Fixed()] = ack

	// Put these messages and ackowledgements that I have not seen yet back into the queues to process.
	if okr {
		TotalXReviewQueueInputs.Inc()
		s.XReview = append(s.XReview, ack)
	}
	if okm {
		TotalXReviewQueueInputs.Inc()
		s.XReview = append(s.XReview, msg)
	}

	// If I've seen both, put them in the process list.
	if !okr && !okm {
		pl.AddToProcessList(ack, msg)
	}

	s.MissingResponseAppliedCnt++

}

func (s *State) FollowerExecuteDataResponse(m interfaces.IMsg) {
	msg, ok := m.(*messages.DataResponse)
	if !ok {
		return
	}

	switch msg.DataType {
	case 1: // Data is an entryBlock
		eblock, ok := msg.DataObject.(interfaces.IEntryBlock)
		if !ok {
			return
		}

		ebKeyMR, _ := eblock.KeyMR()
		if ebKeyMR == nil {
			return
		}

		for i, missing := range s.MissingEntryBlocks {
			eb := missing.EBHash
			if !eb.IsSameAs(ebKeyMR) {
				continue
			}

			db, err := s.DB.FetchDBlockByHeight(eblock.GetHeader().GetDBHeight())
			if err != nil || db == nil {
				return
			}

			var missing []MissingEntryBlock
			missing = append(missing, s.MissingEntryBlocks[:i]...)
			missing = append(missing, s.MissingEntryBlocks[i+1:]...)
			s.MissingEntryBlocks = missing

			s.DB.ProcessEBlockBatch(eblock, true)

			break
		}

	case 0: // Data is an entry
		entry, ok := msg.DataObject.(interfaces.IEBEntry)
		if !ok {
			return
		}
		if len(s.WriteEntry) < cap(s.WriteEntry) {
			s.WriteEntry <- entry
		}
	}
}

func (s *State) FollowerExecuteMissingMsg(msg interfaces.IMsg) {
	// Don't respond to missing messages if we are behind.
	if s.inMsgQueue.Length() > constants.INMSGQUEUE_LOW {
		return
	}

	m := msg.(*messages.MissingMsg)

	pl := s.ProcessLists.Get(m.DBHeight)

	if pl == nil {
		s.MissingRequestIgnoreCnt++
		return
	}
	FollowerMissingMsgExecutions.Inc()
	sent := false
	if len(pl.System.List) > int(m.SystemHeight) && pl.System.List[m.SystemHeight] != nil {
		msgResponse := messages.NewMissingMsgResponse(s, pl.System.List[m.SystemHeight], nil)
		msgResponse.SetOrigin(m.GetOrigin())
		msgResponse.SetNetworkOrigin(m.GetNetworkOrigin())
		s.NetworkOutMsgQueue().Enqueue(msgResponse)
		s.MissingRequestReplyCnt++
		sent = true
	}

	for _, h := range m.ProcessListHeight {
		missingmsg, ackMsg, err := s.LoadSpecificMsgAndAck(m.DBHeight, m.VMIndex, h)

		if missingmsg != nil && ackMsg != nil && err == nil {
			// If I don't have this message, ignore.
			msgResponse := messages.NewMissingMsgResponse(s, missingmsg, ackMsg)
			msgResponse.SetOrigin(m.GetOrigin())
			msgResponse.SetNetworkOrigin(m.GetNetworkOrigin())
			s.NetworkOutMsgQueue().Enqueue(msgResponse)
			s.MissingRequestReplyCnt++
			sent = true
		}
	}

	if !sent {
		s.MissingRequestIgnoreCnt++
	}
	return
}

func (s *State) FollowerExecuteCommitChain(m interfaces.IMsg) {
	FollowerExecutions.Inc()
	s.FollowerExecuteMsg(m)
	cc := m.(*messages.CommitChainMsg)
	re := s.Holding[cc.CommitChain.EntryHash.Fixed()]
	if re != nil {
		TotalXReviewQueueInputs.Inc()
		s.XReview = append(s.XReview, re)
		re.SendOut(s, re)
	}
}

func (s *State) FollowerExecuteCommitEntry(m interfaces.IMsg) {
	FollowerExecutions.Inc()
	s.FollowerExecuteMsg(m)
	ce := m.(*messages.CommitEntryMsg)
	re := s.Holding[ce.CommitEntry.EntryHash.Fixed()]
	if re != nil {
		s.XReview = append(s.XReview, re)
		re.SendOut(s, re)
	}
}

func (s *State) FollowerExecuteRevealEntry(m interfaces.IMsg) {
	FollowerExecutions.Inc()
	TotalHoldingQueueInputs.Inc()
	s.Holding[m.GetMsgHash().Fixed()] = m
	ack, _ := s.Acks[m.GetMsgHash().Fixed()].(*messages.Ack)

	if ack != nil {
		m.SendOut(s, m)
		ack.SendOut(s, ack)
		m.SetLeaderChainID(ack.GetLeaderChainID())
		m.SetMinute(ack.Minute)

		pl := s.ProcessLists.Get(ack.DBHeight)
		pl.AddToProcessList(ack, m)

		msg := m.(*messages.RevealEntryMsg)
		TotalCommitsOutputs.Inc()
		s.Commits.Delete(msg.Entry.GetHash().Fixed()) // 	delete(s.Commits, msg.Entry.GetHash().Fixed())

		// This is so the api can determine if a chainhead is about to be updated. It fixes a race condition
		// on the api. MUST BE BEFORE THE REPLAY FILTER ADD
		pl.PendingChainHeads.Put(msg.Entry.GetChainID().Fixed(), msg)
		// Okay the Reveal has been recorded.  Record this as an entry that cannot be duplicated.
		s.Replay.IsTSValid_(constants.REVEAL_REPLAY, msg.Entry.GetHash().Fixed(), msg.Timestamp, s.GetTimestamp())

	}

}

func (s *State) LeaderExecute(m interfaces.IMsg) {
	LeaderExecutions.Inc()
	_, ok := s.Replay.Valid(constants.INTERNAL_REPLAY, m.GetRepeatHash().Fixed(), m.GetTimestamp(), s.GetTimestamp())
	if !ok {
		TotalHoldingQueueOutputs.Inc()
		delete(s.Holding, m.GetMsgHash().Fixed())
		return
	}

	ack := s.NewAck(m, nil).(*messages.Ack)
	m.SetLeaderChainID(ack.GetLeaderChainID())
	m.SetMinute(ack.Minute)

	s.ProcessLists.Get(ack.DBHeight).AddToProcessList(ack, m)
}

func (s *State) LeaderExecuteEOM(m interfaces.IMsg) {
	LeaderEOMExecutions.Inc()
	if !m.IsLocal() {
		s.FollowerExecuteEOM(m)
		return
	}

	// The zero based minute for the message is equal to
	// the one based "LastMinute".  This way we know we are
	// generating minutes in order.

	eom := m.(*messages.EOM)
	pl := s.ProcessLists.Get(s.LLeaderHeight)
	vm := pl.VMs[s.LeaderVMIndex]

	// Put the System Height and Serial Hash into the EOM
	eom.SysHeight = uint32(pl.System.Height)
	if pl.System.Height > 1 {
		ff, ok := pl.System.List[pl.System.Height-1].(*messages.FullServerFault)
		if ok {
			eom.SysHash = ff.GetSerialHash()
		}
	}

	if s.Syncing && vm.Synced {
		return
	} else if !s.Syncing {
		s.Syncing = true
		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s LeaderExecuteEOM: !s.EOM(%v)", s.FactomNodeName, s.EOM))
		s.EOM = true
		s.EOMsyncing = true
		s.EOMProcessed = 0
		for _, vm := range pl.VMs {
			vm.Synced = false
		}
		s.EOMLimit = len(pl.FedServers)
		s.EOMMinute = int(s.CurrentMinute)
	}

	if vm.EomMinuteIssued >= s.CurrentMinute+1 {
		//os.Stderr.WriteString(fmt.Sprintf("Bump detected %s minute %2d\n", s.FactomNodeName, s.CurrentMinute))
		return
	}

	//_, vmindex := pl.GetVirtualServers(s.EOMMinute, s.IdentityChainID)

	eom.DBHeight = s.LLeaderHeight
	eom.VMIndex = s.LeaderVMIndex
	// eom.Minute is zerobased, while LeaderMinute is 1 based.  So
	// a simple assignment works.
	eom.Minute = byte(s.CurrentMinute)
	vm.EomMinuteIssued = s.CurrentMinute + 1
	eom.Sign(s)
	eom.MsgHash = nil
	ack := s.NewAck(m, nil).(*messages.Ack)

	TotalAcksInputs.Inc()
	s.Acks[eom.GetMsgHash().Fixed()] = ack
	m.SetLocal(false)
	s.FollowerExecuteEOM(m)
	s.UpdateState()
	delete(s.Acks, ack.GetMsgHash().Fixed())
	delete(s.Holding, m.GetMsgHash().Fixed())
}

func (s *State) LeaderExecuteDBSig(m interfaces.IMsg) {
	LeaderExecutions.Inc()
	dbs := m.(*messages.DirectoryBlockSignature)
	pl := s.ProcessLists.Get(dbs.DBHeight)

	if dbs.DBHeight != s.LLeaderHeight {
		m.FollowerExecute(s)
		return
	}
	if len(pl.VMs[dbs.VMIndex].List) > 0 {
		return
	}

	// Put the System Height and Serial Hash into the EOM
	dbs.SysHeight = uint32(pl.System.Height)
	if pl.System.Height > 1 {
		ff, ok := pl.System.List[pl.System.Height-1].(*messages.FullServerFault)
		if ok {
			dbs.SysHash = ff.GetSerialHash()
		}
	}

	_, ok := s.Replay.Valid(constants.INTERNAL_REPLAY, m.GetRepeatHash().Fixed(), m.GetTimestamp(), s.GetTimestamp())
	if !ok {
		TotalHoldingQueueOutputs.Inc()
		HoldingQueueDBSigOutputs.Inc()
		delete(s.Holding, m.GetMsgHash().Fixed())
		return
	}

	ack := s.NewAck(m, s.Balancehash).(*messages.Ack)

	m.SetLeaderChainID(ack.GetLeaderChainID())
	m.SetMinute(ack.Minute)

	s.ProcessLists.Get(ack.DBHeight).AddToProcessList(ack, m)
}

func (s *State) LeaderExecuteCommitChain(m interfaces.IMsg) {
	cc := m.(*messages.CommitChainMsg)
	// Check if this commit has more entry credits than any previous that we have.
	if !s.IsHighestCommit(cc.CommitChain.EntryHash, m) {
		// This commit is not higher than any previous, so we can discard it and prevent a double spend
		return
	}

	s.LeaderExecute(m)
	re := s.Holding[cc.CommitChain.EntryHash.Fixed()]
	if re != nil {
		TotalXReviewQueueInputs.Inc()
		s.XReview = append(s.XReview, re)
		re.SendOut(s, re)
	}
}

func (s *State) LeaderExecuteCommitEntry(m interfaces.IMsg) {
	s.LeaderExecute(m)
	ce := m.(*messages.CommitEntryMsg)
	re := s.Holding[ce.CommitEntry.EntryHash.Fixed()]
	if re != nil {
		s.XReview = append(s.XReview, re)
		re.SendOut(s, re)
	}
}

func (s *State) LeaderExecuteRevealEntry(m interfaces.IMsg) {
	LeaderExecutions.Inc()
	re := m.(*messages.RevealEntryMsg)
	eh := re.Entry.GetHash()

	rtn := re.Validate(s)

	switch rtn {
	case 0:
		m.FollowerExecute(s)
		return
	case -1:
		return
	}

	commit := s.NextCommit(eh)

	now := s.GetTimestamp()
	// If we have already recorded a Reveal Entry with this hash in this period, just ignore.
	if _, v := s.Replay.Valid(constants.REVEAL_REPLAY, eh.Fixed(), s.GetLeaderTimestamp(), now); !v {
		return
	}

	ack := s.NewAck(m, nil).(*messages.Ack)

	m.SetLeaderChainID(ack.GetLeaderChainID())
	m.SetMinute(ack.Minute)

	// Put the acknowledgement in the Acks so we can tell if AddToProcessList() adds it.
	s.Acks[m.GetMsgHash().Fixed()] = ack
	TotalAcksInputs.Inc()
	s.ProcessLists.Get(ack.DBHeight).AddToProcessList(ack, m)
	// If it was added, then get rid of the matching Commit.
	if s.Acks[m.GetMsgHash().Fixed()] != nil {
		s.PutCommit(eh, commit)
		m.FollowerExecute(s)
	} else {
		// Okay the Reveal has been recorded.  Record this as an entry that cannot be duplicated.
		s.Replay.IsTSValid_(constants.REVEAL_REPLAY, eh.Fixed(), m.GetTimestamp(), now)
		TotalCommitsOutputs.Inc()
		s.Commits.Delete(eh.Fixed()) // delete(s.Commits, eh.Fixed())
	}
}

func (s *State) ProcessAddServer(dbheight uint32, addServerMsg interfaces.IMsg) bool {
	as, ok := addServerMsg.(*messages.AddServerMsg)
	if ok && !ProcessIdentityToAdminBlock(s, as.ServerChainID, as.ServerType) {
		//s.AddStatus(fmt.Sprintf("Failed to add %x as server type %d", as.ServerChainID.Bytes()[2:5], as.ServerType))
		return false
	}
	return true
}

func (s *State) ProcessRemoveServer(dbheight uint32, removeServerMsg interfaces.IMsg) bool {
	rs, ok := removeServerMsg.(*messages.RemoveServerMsg)
	if !ok {
		return true
	}

	if !s.VerifyIsAuthority(rs.ServerChainID) {
		return true
	}

	if s.GetAuthorityServerType(rs.ServerChainID) != rs.ServerType {
		return true
	}

	if len(s.LeaderPL.FedServers) < 2 && rs.ServerType == 0 {
		return true
	}
	s.LeaderPL.AdminBlock.RemoveFederatedServer(rs.ServerChainID)

	return true
}

func (s *State) ProcessChangeServerKey(dbheight uint32, changeServerKeyMsg interfaces.IMsg) bool {
	ask, ok := changeServerKeyMsg.(*messages.ChangeServerKeyMsg)
	if !ok {
		return true
	}

	if !s.VerifyIsAuthority(ask.IdentityChainID) {
		return true
	}

	switch ask.AdminBlockChange {
	case constants.TYPE_ADD_BTC_ANCHOR_KEY:
		var btcKey [20]byte
		copy(btcKey[:], ask.Key.Bytes()[:20])
		s.LeaderPL.AdminBlock.AddFederatedServerBitcoinAnchorKey(ask.IdentityChainID, ask.KeyPriority, ask.KeyType, btcKey)
	case constants.TYPE_ADD_FED_SERVER_KEY:
		pub := ask.Key.Fixed()
		s.LeaderPL.AdminBlock.AddFederatedServerSigningKey(ask.IdentityChainID, pub)
	case constants.TYPE_ADD_MATRYOSHKA:
		s.LeaderPL.AdminBlock.AddMatryoshkaHash(ask.IdentityChainID, ask.Key)
	}
	return true
}

func (s *State) ProcessCommitChain(dbheight uint32, commitChain interfaces.IMsg) bool {
	c, _ := commitChain.(*messages.CommitChainMsg)

	pl := s.ProcessLists.Get(dbheight)
	pl.EntryCreditBlock.GetBody().AddEntry(c.CommitChain)
	if e := s.GetFactoidState().UpdateECTransaction(true, c.CommitChain); e == nil {
		// save the Commit to match agains the Reveal later
		h := c.CommitChain.EntryHash
		s.PutCommit(h, c)
		entry := s.Holding[h.Fixed()]
		if entry != nil {
			entry.SendOut(s, entry)
			TotalXReviewQueueInputs.Inc()
			s.XReview = append(s.XReview, entry)
			TotalHoldingQueueOutputs.Inc()
			delete(s.Holding, h.Fixed())
		}
		return true
	}
	//s.AddStatus("Cannot process Commit Chain")

	return false
}

func (s *State) ProcessCommitEntry(dbheight uint32, commitEntry interfaces.IMsg) bool {
	c, _ := commitEntry.(*messages.CommitEntryMsg)

	pl := s.ProcessLists.Get(dbheight)
	pl.EntryCreditBlock.GetBody().AddEntry(c.CommitEntry)
	if e := s.GetFactoidState().UpdateECTransaction(true, c.CommitEntry); e == nil {
		// save the Commit to match agains the Reveal later
		h := c.CommitEntry.EntryHash
		s.PutCommit(h, c)
		entry := s.Holding[h.Fixed()]
		if entry != nil {
			entry.SendOut(s, entry)
			TotalXReviewQueueInputs.Inc()
			s.XReview = append(s.XReview, entry)
			TotalHoldingQueueOutputs.Inc()
			delete(s.Holding, h.Fixed())
		}
		return true
	}
	//s.AddStatus("Cannot Process Commit Entry")

	return false
}

func (s *State) ProcessRevealEntry(dbheight uint32, m interfaces.IMsg) bool {
	TotalProcessListProcesses.Inc()
	msg := m.(*messages.RevealEntryMsg)
	myhash := msg.Entry.GetHash()

	chainID := msg.Entry.GetChainID()

	TotalCommitsOutputs.Inc()
	s.Commits.Delete(msg.Entry.GetHash().Fixed()) // delete(s.Commits, msg.Entry.GetHash().Fixed())

	eb := s.GetNewEBlocks(dbheight, chainID)
	eb_db := s.GetNewEBlocks(dbheight-1, chainID)
	if eb_db == nil {
		eb_db, _ = s.DB.FetchEBlockHead(chainID)
	}
	// Handle the case that this is a Entry Chain create
	// Must be built with CommitChain (i.e. !msg.IsEntry).  Also
	// cannot have an existing chaing (eb and eb_db == nil)
	if !msg.IsEntry && eb == nil && eb_db == nil {
		// Create a new Entry Block for a new Entry Block Chain
		eb = entryBlock.NewEBlock()
		// Set the Chain ID
		eb.GetHeader().SetChainID(chainID)
		// Set the Directory Block Height for this Entry Block
		eb.GetHeader().SetDBHeight(dbheight)
		// Add our new entry
		eb.AddEBEntry(msg.Entry)
		// Put it in our list of new Entry Blocks for this Directory Block
		s.PutNewEBlocks(dbheight, chainID, eb)
		s.PutNewEntries(dbheight, myhash, msg.Entry)

		s.IncEntryChains()
		s.IncEntries()
		return true
	}

	// Create an entry (even if they used commitChain).  Means there must
	// be a chain somewhere.  If not, we return false.
	if eb == nil {
		if eb_db == nil {
			//s.AddStatus("Failed to add to process Reveal Entry because no Entry Block found")
			return false
		}
		eb = entryBlock.NewEBlock()
		eb.GetHeader().SetEBSequence(eb_db.GetHeader().GetEBSequence() + 1)
		eb.GetHeader().SetPrevFullHash(eb_db.GetHash())
		// Set the Chain ID
		eb.GetHeader().SetChainID(chainID)
		// Set the Directory Block Height for this Entry Block
		eb.GetHeader().SetDBHeight(dbheight)
		// Set the PrevKeyMR
		key, _ := eb_db.KeyMR()
		eb.GetHeader().SetPrevKeyMR(key)
	}
	// Add our new entry
	eb.AddEBEntry(msg.Entry)
	// Put it in our list of new Entry Blocks for this Directory Block
	s.PutNewEBlocks(dbheight, chainID, eb)
	s.PutNewEntries(dbheight, myhash, msg.Entry)

	// Monitor key changes for fed/audit servers
	LoadIdentityByEntry(msg.Entry, s, dbheight, false)

	s.IncEntries()
	return true
}

// dbheight is the height of the process list, and vmIndex is the vm
// that is missing the DBSig.  If the DBSig isn't our responsiblity, then
// this call will do nothing.  Assumes the state for the leader is set properly
func (s *State) SendDBSig(dbheight uint32, vmIndex int) {
	dbslog := consenLogger.WithFields(log.Fields{"func": "SendDBSig"})

	ht := s.GetHighestSavedBlk()
	if dbheight <= ht || s.EOM {
		return
	}
	pl := s.ProcessLists.Get(dbheight)
	vm := pl.VMs[vmIndex]
	if vm.Height > 0 {
		return
	}
	leader, lvm := pl.GetVirtualServers(vm.LeaderMinute, s.IdentityChainID)
	if !leader || lvm != vmIndex {
		return
	}

	if !vm.Signed {
		dbstate := s.DBStates.Get(int(dbheight - 1))
		if dbstate == nil && dbheight > 0 {
			s.SendDBSig(dbheight-1, vmIndex)
			return
		}
		if lvm == vmIndex {
			if !pl.DBSigAlreadySent {
				dbs := new(messages.DirectoryBlockSignature)
				dbs.DirectoryBlockHeader = dbstate.DirectoryBlock.GetHeader()
				//dbs.DirectoryBlockKeyMR = dbstate.DirectoryBlock.GetKeyMR()
				dbs.ServerIdentityChainID = s.GetIdentityChainID()
				dbs.DBHeight = dbheight
				dbs.Timestamp = s.GetTimestamp()
				dbs.SetVMHash(nil)
				dbs.SetVMIndex(vmIndex)
				dbs.SetLocal(true)
				dbs.Sign(s)
				err := dbs.Sign(s)
				if err != nil {
					panic(err)
				}

				dbslog.WithFields(dbs.LogFields()).WithFields(log.Fields{"lheight": s.GetLeaderHeight(), "node-name": s.GetFactomNodeName()}).Infof("Generate DBSig")
				dbs.LeaderExecute(s)
				vm.Signed = true
				pl.DBSigAlreadySent = true
			} else {
				pl.Ask(vmIndex, 0, 0, 5)
			}
		}
	}
}

// TODO: Should fault the server if we don't have the proper sequence of EOM messages.
func (s *State) ProcessEOM(dbheight uint32, msg interfaces.IMsg) bool {
	TotalProcessEOMs.Inc()
	e := msg.(*messages.EOM)
	// plog := consenLogger.WithFields(log.Fields{"func": "ProcessEOM", "msgheight": e.DBHeight, "lheight": s.GetLeaderHeight(), "min", e.Minute})

	if s.Syncing && !s.EOM {
		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm %2d Will Not Process: return on s.Syncing(%v) && !s.EOM(%v)", s.FactomNodeName, e.VMIndex, s.Syncing, s.EOM))
		return false
	}

	if s.EOM && e.DBHeight != dbheight {
		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm %2d Invalid EOM s.EOM(%v) && e.DBHeight(%v) != dbheight(%v)", s.FactomNodeName, e.VMIndex, s.EOM, e.DBHeight, dbheight))
	}

	if s.EOM && int(e.Minute) > s.EOMMinute {
		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm %2d Will Not Process: return on s.EOM(%v) && int(e.Minute(%v)) > s.EOMMinute(%v)", s.FactomNodeName, e.VMIndex, s.EOM, e.Minute, s.EOMMinute))
		return false
	}

	pl := s.ProcessLists.Get(dbheight)
	vm := s.ProcessLists.Get(dbheight).VMs[msg.GetVMIndex()]

	if uint32(pl.System.Height) >= e.SysHeight {
		s.EOMSys = true
	}

	// If I have done everything for all EOMs for all VMs, then and only then do I
	// let processing continue.
	if s.EOMDone && s.EOMSys {
		dbstate := s.GetDBState(dbheight - 1)
		if dbstate == nil {
			//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm %2d DBState == nil: return on s.EOM(%v) && int(e.Minute(%v)) > s.EOMMinute(%v)", s.FactomNodeName, e.VMIndex, s.EOM, e.Minute, s.EOMMinute))
			return false
		}
		if !dbstate.Saved {
			//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm %2d DBState not saved: return on s.EOM(%v) && int(e.Minute(%v)) > s.EOMMinute(%v)", s.FactomNodeName, e.VMIndex, s.EOM, e.Minute, s.EOMMinute))
			return false
		}

		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm %2d Done! s.EOMDone(%v) && s.EOMSys(%v)", s.FactomNodeName, e.VMIndex, s.EOMDone, s.EOMSys))
		s.EOMProcessed--
		if s.EOMProcessed <= 0 {
			s.EOM = false
			s.EOMDone = false
			s.Syncing = false
			s.EOMProcessed = 0
			s.TempBalanceHash = s.FactoidState.GetBalanceHash(true)
		}
		s.SendHeartBeat()

		return true
	}

	// What I do once  for all VMs at the beginning of processing a particular EOM
	if !s.EOM {
		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm %2d Start EOM Processing: !s.EOM(%v) EOM: %s", s.FactomNodeName, e.VMIndex, s.EOM, e.String()))
		s.EOMSys = false
		s.Syncing = true
		s.EOM = true
		s.EOMLimit = len(s.LeaderPL.FedServers)
		s.EOMMinute = int(e.Minute)
		s.EOMsyncing = true
		s.EOMProcessed = 0

		for _, vm := range pl.VMs {
			vm.Synced = false
		}
		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm  %2d First EOM processed: return on s.EOM(%v) && int(e.Minute(%v)) > s.EOMMinute(%v)", s.FactomNodeName, e.VMIndex, s.EOM, e.Minute, s.EOMMinute))
		return false
	}

	// What I do for each EOM
	if !vm.Synced {

		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm %2d Process Once: !e.Processed(%v) EOM: %s", s.FactomNodeName, e.VMIndex, e.Processed, e.String()))
		vm.LeaderMinute++
		s.EOMProcessed++
		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm %2d EOMProcessed++ (%2d)", s.FactomNodeName, e.VMIndex, s.EOMProcessed))
		vm.Synced = true
		markNoFault(pl, msg.GetVMIndex())
		if s.LeaderPL.SysHighest < int(e.SysHeight) {
			s.LeaderPL.SysHighest = int(e.SysHeight)
		}
		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm %2d Process this EOM: return on s.EOM(%v) && int(e.Minute(%v)) > s.EOMMinute(%v)", s.FactomNodeName, e.VMIndex, s.EOM, e.Minute, s.EOMMinute))
		return false
	}

	allfaults := s.LeaderPL.System.Height >= s.LeaderPL.SysHighest

	//if !allfaults {
	//	os.Stderr.WriteString(fmt.Sprintf("%s dbht %d min %d Don't have all faults\n", s.FactomNodeName, e.DBHeight, e.Minute))
	//}
	// After all EOM markers are processed, Claim we are done.  Now we can unwind
	if allfaults && s.EOMProcessed == s.EOMLimit && !s.EOMDone {

		//fmt.Println(fmt.Sprintf("EOM PROCESS: EOM Complete: %10s vm %2d allfaults(%v) && s.EOMProcessed(%v) == s.EOMLimit(%v) && !s.EOMDone(%v)",
		//	s.FactomNodeName,
		//	e.VMIndex, allfaults, s.EOMProcessed, s.EOMLimit, s.EOMDone))

		s.EOMDone = true
		for _, eb := range pl.NewEBlocks {
			eb.AddEndOfMinuteMarker(byte(e.Minute + 1))
		}

		s.FactoidState.EndOfPeriod(int(e.Minute))

		ecblk := pl.EntryCreditBlock
		ecbody := ecblk.GetBody()
		mn := entryCreditBlock.NewMinuteNumber(e.Minute + 1)
		ecbody.AddEntry(mn)

		if !s.Leader {
			s.CurrentMinute = int(e.Minute)
		}

		s.CurrentMinute++
		s.CurrentMinuteStartTime = time.Now().UnixNano()

		switch {
		case s.CurrentMinute < 10:
			if s.CurrentMinute == 1 {
				dbstate := s.GetDBState(dbheight - 1)
				if !dbstate.Saved {
					dbstate.ReadyToSave = true
				}
			}
			s.LeaderPL = s.ProcessLists.Get(s.LLeaderHeight)
			s.Leader, s.LeaderVMIndex = s.LeaderPL.GetVirtualServers(s.CurrentMinute, s.IdentityChainID)
		case s.CurrentMinute == 10:
			eBlocks := []interfaces.IEntryBlock{}
			entries := []interfaces.IEBEntry{}
			for _, v := range pl.NewEBlocks {
				eBlocks = append(eBlocks, v)
			}
			for _, v := range pl.NewEntries {
				entries = append(entries, v)
			}

			dbstate := s.AddDBState(true, s.LeaderPL.DirectoryBlock, s.LeaderPL.AdminBlock, s.GetFactoidState().GetCurrentBlock(), s.LeaderPL.EntryCreditBlock, eBlocks, entries)
			if dbstate == nil {
				dbstate = s.DBStates.Get(int(s.LeaderPL.DirectoryBlock.GetHeader().GetDBHeight()))
			}
			dbht := int(dbstate.DirectoryBlock.GetHeader().GetDBHeight())
			if dbht > 0 {
				prev := s.DBStates.Get(dbht - 1)
				s.DBStates.FixupLinks(prev, dbstate)
			}
			s.DBStates.ProcessBlocks(dbstate)

			s.CurrentMinute = 0
			s.LLeaderHeight++

			s.GetAckChange()
			s.CheckForIDChange()

			s.LeaderPL = s.ProcessLists.Get(s.LLeaderHeight)
			s.Leader, s.LeaderVMIndex = s.LeaderPL.GetVirtualServers(0, s.IdentityChainID)

			s.DBSigProcessed = 0

			// Note about dbsigs.... If we processed the previous minute, then we generate the DBSig for the next block.
			// But if we didn't process the preivious block, like we start from scratch, or we had to reset the entire
			// network, then no dbsig exists.  This code doesn't execute, and so we have no dbsig.  In that case, on
			// the next EOM, we see the block hasn't been signed, and we sign the block (Thats the call to SendDBSig()
			// above).
			pldbs := s.ProcessLists.Get(s.LLeaderHeight)
			if s.Leader && !pldbs.DBSigAlreadySent {
				// dbstate is already set.
				dbs := new(messages.DirectoryBlockSignature)
				db := dbstate.DirectoryBlock
				dbs.DirectoryBlockHeader = db.GetHeader()
				//dbs.DirectoryBlockKeyMR = dbstate.DirectoryBlock.GetKeyMR()
				dbs.ServerIdentityChainID = s.GetIdentityChainID()
				dbs.DBHeight = s.LLeaderHeight
				dbs.Timestamp = s.GetTimestamp()
				dbs.SetVMHash(nil)
				dbs.SetVMIndex(s.LeaderVMIndex)
				dbs.SetLocal(true)
				dbs.Sign(s)
				err := dbs.Sign(s)
				if err != nil {
					panic(err)
				}
				pldbs.DBSigAlreadySent = true

				dbslog := consenLogger.WithFields(log.Fields{"func": "SendDBSig", "lheight": s.GetLeaderHeight(), "node-name": s.GetFactomNodeName()}).WithFields(dbs.LogFields())
				dbslog.Infof("Generate DBSig")

				dbs.LeaderExecute(s)
			}
			s.Saving = true
		}

		s.Commits.RemoveExpired(s)
		// for k, v := range s.Commits {
		// 	if v != nil {
		// 		_, ok := s.Replay.Valid(constants.TIME_TEST, v.GetRepeatHash().Fixed(), v.GetTimestamp(), s.GetTimestamp())
		// 		if !ok {
		// 			delete(s.Commits, k)
		// 		}
		// 	}
		// }

		for k := range s.Acks {
			v := s.Acks[k].(*messages.Ack)
			if v.DBHeight < s.LLeaderHeight {
				TotalAcksOutputs.Inc()
				delete(s.Acks, k)
			}
		}
		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm %2d Saving: return on s.EOM(%v) && int(e.Minute(%v)) > s.EOMMinute(%v)", s.FactomNodeName, e.VMIndex, s.EOM, e.Minute, s.EOMMinute))
	} else {
		//fmt.Println(fmt.Sprintf("EOM PROCESS: %10s vm %2d Do nothing: return on s.EOM(%v) && int(e.Minute(%v)) > s.EOMMinute(%v)", s.FactomNodeName, e.VMIndex, s.EOM, e.Minute, s.EOMMinute))
	}

	return false
}

func (s *State) CheckForIDChange() {
	var reloadIdentity bool = false
	if s.AckChange > 0 {
		if s.LLeaderHeight >= s.AckChange {
			reloadIdentity = true
		}
	}
	if reloadIdentity {
		config := util.ReadConfig(s.filename)
		var err error
		s.IdentityChainID, err = primitives.NewShaHashFromStr(config.App.IdentityChainID)
		if err != nil {
			panic(err)
		}
		s.LocalServerPrivKey = config.App.LocalServerPrivKey
		s.initServerKeys()
	}
}

// When we process the directory Signature, and we are the leader for said signature, it
// is then that we push it out to the rest of the network.  Otherwise, if we are not the
// leader for the signature, it marks the sig complete for that list
func (s *State) ProcessDBSig(dbheight uint32, msg interfaces.IMsg) bool {
	//fmt.Println(fmt.Sprintf("ProcessDBSig: %10s %s ", s.FactomNodeName, msg.String()))

	dbs := msg.(*messages.DirectoryBlockSignature)
	//plog makes logging anything in ProcessDBSig() easier
	//		The instantiation as a function makes it almost no overhead if you do not use it
	plog := func(format string, args ...interface{}) {
		consenLogger.WithFields(log.Fields{"func": "ProcessDBSig", "msgheight": dbs.DBHeight, "lheight": s.GetLeaderHeight(), "msg": msg.String()}).Errorf(format, args...)
	}
	// Don't process if syncing an EOM
	if s.Syncing && !s.DBSig {
		//fmt.Println(fmt.Sprintf("ProcessDBSig(): %10s Will Not Process: dbht: %d return on s.Syncing(%v) && !s.DBSig(%v)", s.FactomNodeName,
		//	dbs.DBHeight, s.Syncing, s.DBSig))
		return false
	}

	pl := s.ProcessLists.Get(dbheight)
	vm := s.ProcessLists.Get(dbheight).VMs[msg.GetVMIndex()]

	if uint32(pl.System.Height) >= dbs.SysHeight {
		s.DBSigSys = true
	}

	// If we are done with DBSigs, and this message is processed, then we are done.  Let everything go!
	if s.DBSigSys && s.DBSig && s.DBSigDone {
		//fmt.Println(fmt.Sprintf("ProcessDBSig(): %10s Finished with DBSig: s.DBSigSys(%v) && s.DBSig(%v) && s.DBSigDone(%v)", s.FactomNodeName, s.DBSigSys, s.DBSig, s.DBSigDone))
		s.DBSigProcessed--
		if s.DBSigProcessed <= 0 {
			s.EOMDone = false
			s.EOMSys = false
			s.EOM = false
			s.DBSig = false
			s.Syncing = false
		}
		vm.Signed = true
		//s.LeaderPL.AdminBlock
		return true
	}

	// Put the stuff that only executes once at the start of DBSignatures here
	if !s.DBSig {

		//fmt.Printf("ProcessDBSig(): %s Start DBSig %s\n", s.FactomNodeName, dbs.String())
		s.DBSigLimit = len(pl.FedServers)
		s.DBSigProcessed = 0
		s.DBSig = true
		s.Syncing = true
		s.DBSigDone = false
		for _, vm := range pl.VMs {
			vm.Synced = false
		}
		pl.ResetDiffSigTally()
	}

	// Put the stuff that executes per DBSignature here
	if !vm.Synced {

		if s.LLeaderHeight > 0 && s.GetHighestCompletedBlk()+1 < s.LLeaderHeight {

			pl := s.ProcessLists.Get(dbs.DBHeight - 1)
			if !pl.Complete() {
				dbstate := s.DBStates.Get(int(dbs.DBHeight - 1))
				if dbstate == nil || (!dbstate.Locked && !dbstate.Saved) {
					db, _ := s.DB.FetchDBlockByHeight(dbs.DBHeight - 1)
					if db == nil {
						//fmt.Printf("ProcessDBSig(): %10s Previous Process List isn't complete. %s\n", s.FactomNodeName, dbs.String())
						return false
					}
				}
			}
		}

		//fmt.Println(fmt.Sprintf("ProcessDBSig(): %10s Process the %d DBSig: %v", s.FactomNodeName, s.DBSigProcessed, dbs.String()))
		if dbs.VMIndex == 0 {
			//fmt.Println(fmt.Sprintf("ProcessDBSig(): %10s Set Leader Timestamp to: %v %d", s.FactomNodeName, dbs.GetTimestamp().String(), dbs.GetTimestamp().GetTimeMilli()))
			s.SetLeaderTimestamp(dbs.GetTimestamp())
		}

		dblk, err := s.DB.FetchDBlockByHeight(dbheight - 1)
		if err != nil || dblk == nil {
			dbstate := s.GetDBState(dbheight - 1)
			if dbstate == nil || !(!dbstate.IsNew || dbstate.Locked || dbstate.Saved) {
				//fmt.Println(fmt.Sprintf("ProcessingDBSig(): %10s The prior dbsig %d is nil", s.FactomNodeName, dbheight-1))
				return false
			}
			dblk = dbstate.DirectoryBlock
		}

		if dbs.DirectoryBlockHeader.GetBodyMR().Fixed() != dblk.GetHeader().GetBodyMR().Fixed() {
			pl.IncrementDiffSigTally()
			plog("Failed. DBlocks do not match Expected-Body-Mr: %x, Got: %x",
				dblk.GetHeader().GetBodyMR().Fixed(), dbs.DirectoryBlockHeader.GetBodyMR().Fixed())
			return false
		}

		// Adds DB Sig to be added to Admin block if passes sig checks
		data, err := dbs.DirectoryBlockHeader.MarshalBinary()
		if err != nil {
			return false
		}
		if !dbs.DBSignature.Verify(data) {
			return false
		}

		valid, err := s.VerifyAuthoritySignature(data, dbs.DBSignature.GetSignature(), dbs.DBHeight)
		if err != nil || valid != 1 {
			plog("Failed. Invalid Auth Sig: Pubkey: %x", dbs.Signature.GetKey())
			return false
		}

		dbs.Matches = true
		s.AddDBSig(dbheight, dbs.ServerIdentityChainID, dbs.DBSignature)

		s.DBSigProcessed++
		//fmt.Println(fmt.Sprintf("Process DBSig %10s vm %2v DBSigProcessed++ (%2d)", s.FactomNodeName, dbs.VMIndex, s.DBSigProcessed))
		vm.Synced = true
	}

	allfaults := s.LeaderPL.System.Height >= s.LeaderPL.SysHighest

	// Put the stuff that executes once for set of DBSignatures (after I have them all) here
	if allfaults && !s.DBSigDone && s.DBSigProcessed >= s.DBSigLimit {
		//fmt.Println(fmt.Sprintf("All DBSigs are processed: allfaults(%v), && !s.DBSigDone(%v) && s.DBSigProcessed(%v)>= s.DBSigLimit(%v)",
		//	allfaults, s.DBSigDone, s.DBSigProcessed, s.DBSigLimit))
		fails := 0
		for i := range pl.FedServers {
			vm := pl.VMs[i]
			if len(vm.List) > 0 {
				tdbsig, ok := vm.List[0].(*messages.DirectoryBlockSignature)
				if !ok || !tdbsig.Matches {
					s.DBSigProcessed--
					return false
				}
			}
		}
		if fails > 0 {
			//fmt.Println("DBSig Fails Detected")
			return false
		}

		// TODO: check signatures here.  Count what match and what don't.  Then if a majority
		// disagree with us, null our entry out.  Otherwise toss our DBState and ask for one from
		// our neighbors.
		if !s.KeepMismatch && !pl.CheckDiffSigTally() {
			return false
		}

		s.ReviewHolding()
		s.Saving = false
		s.DBSigDone = true
	}
	return false
	/*
		err := s.LeaderPL.AdminBlock.AddDBSig(dbs.ServerIdentityChainID, dbs.DBSignature)
		if err != nil {
			fmt.Printf("Error in adding DB sig to admin block, %s\n", err.Error())
		}
	*/
}

func (s *State) ProcessFullServerFault(dbheight uint32, msg interfaces.IMsg) bool {
	// If we are here, this means that the FullFault message is complete
	// and we can execute it as such (replacing the faulted Leader with
	// the nominated Audit server)

	fullFault, ok := msg.(*messages.FullServerFault)
	if !ok {
		return false
	}
	if fullFault.GetAlreadyProcessed() {
		return false
	}

	pl := s.ProcessLists.Get(fullFault.DBHeight)
	if pl == nil {
		return false
	}

	// First we will update our status to include our fault process attempt
	//s.AddStatus(fmt.Sprintf("PROCESS Full Fault: %s",
	//	fullFault.StringWithSigCnt(s)))

	// If we're not caught up in our SystemList enough to process the fault,
	// processing must fail
	if pl.System.Height < int(fullFault.SystemHeight) {
		//s.AddStatus(fmt.Sprintf("PROCESS Full Fault Not at right system height: %s", fullFault.StringWithSigCnt(s)))
		return false
	}

	vm := pl.VMs[int(fullFault.VMIndex)]

	// Do not process the fault until the VM height is caught up to it
	if fullFault.Height > uint32(vm.Height) {
		//s.AddStatus(fmt.Sprintf("PROCESS Full Fault Not at right vm height: (FF:%d vm:%d) %s",
		//	fullFault.Height,
		//	uint32(vm.Height),
		//	fullFault.StringWithSigCnt(s)))
		return false
	}

	// Double-check that the fault's SystemHeight is proper
	if int(fullFault.SystemHeight) != pl.System.Height {
		//s.AddStatus(fmt.Sprintf("PROCESS Full Fault Not at right system height (FF:%d sys:%d) : %s",
		//	int(fullFault.SystemHeight),
		//	pl.System.Height,
		//	fullFault.StringWithSigCnt(s)))

		return false
	}

	// If "ClearFault" is set to true, that means the leader came back online so we can "forget" about
	// the fault (leave it in the SystemList, but consider it processed without having promoted/demoted anyone)
	if fullFault.ClearFault {
		if fullFault.GetVMIndex() < len(pl.VMs) && pl.VMs[fullFault.GetVMIndex()].WhenFaulted == 0 {
			// If we agree that the server doesn't need to be faulted, we will clear our currentFault
			// but otherwise do nothing (we do not execute the actual demotion/promotion)
			//s.AddStatus(fmt.Sprintf("PROCESS Full Fault CLEARING: %s", fullFault.StringWithSigCnt(s)))
			fullFault.SetAlreadyProcessed()
			faultLogger.WithField("func", "ClearFault").WithFields(fullFault.LogFields()).Warn("Cleared")
			return true
		}
	}

	auditServerList := s.GetAuditServers(fullFault.DBHeight)
	var theAuditReplacement interfaces.IServer

	for _, auditServer := range auditServerList {
		if auditServer.GetChainID().IsSameAs(fullFault.AuditServerID) {
			theAuditReplacement = auditServer
		}
	}
	if theAuditReplacement == nil {
		for _, fedServer := range s.GetFedServers(fullFault.DBHeight) {
			if fedServer.GetChainID().IsSameAs(fullFault.AuditServerID) {
				//s.AddStatus(fmt.Sprintf("PROCESS Full Fault Nothing to do, Already a Fed Server! %s", fullFault.StringWithSigCnt(s)))
				return false
			}
		}
		// If we don't have any Audit Servers in our Authority set
		// that match the nominated Audit Server in the FullFault,
		// we can't really do anything useful with it
		//s.AddStatus(fmt.Sprintf("PROCESS Full Fault Audit Server not an audit server. %s", fullFault.StringWithSigCnt(s)))
		return false
	}

	if fullFault.HasEnoughSigs(s) && s.pledgedByAudit(fullFault) {
		// If we are here, this means that the FullFault message is complete
		// and we can execute it as such (replacing the faulted Leader with
		// the nominated Audit server)

		rHt := vm.Height
		ffHt := int(fullFault.Height)
		if rHt > ffHt {
			//s.AddStatus(fmt.Sprintf("PROCESS Full Fault: FAIL but reset vm... %s", fullFault.StringWithSigCnt(s)))
			vm.Height = ffHt
			return false
		} else if rHt < ffHt {
			//s.AddStatus(fmt.Sprintf("PROCESS Full Fault: FAIL, vm not there yet. %s", fullFault.StringWithSigCnt(s)))
			return false
		}

		// Here is where we actually swap out the Leader with the Audit server
		// being promoted
		for listIdx, fedServ := range pl.FedServers {
			if fedServ.GetChainID().IsSameAs(fullFault.ServerID) {

				pl.FedServers[listIdx] = theAuditReplacement
				pl.FedServers[listIdx].SetOnline(true)
				audIdx := pl.AddAuditServer(fedServ.GetChainID())
				pl.AuditServers[audIdx].SetOnline(false)

				s.RemoveAuditServer(fullFault.DBHeight, theAuditReplacement.GetChainID())
				// After executing the FullFault successfully, we want to reset
				// to the default state (No One At Fault)
				s.Leader, s.LeaderVMIndex = s.LeaderPL.GetVirtualServers(s.CurrentMinute, s.IdentityChainID)

				authoritiesString := ""
				for _, str := range s.ConstructAuthoritySetString() {
					if len(authoritiesString) > 0 {
						authoritiesString += "\n"
					}
					authoritiesString += str
				}
				// Any updates required to the state as established by the AdminBlock are applied here.
				pl.State.SetAuthoritySetString(authoritiesString)
				authorityDeltaString := fmt.Sprintf("FULL FAULT SUCCESSFULLY PROCESSED DBHt: %d SysHt: %d ServerID %s AuditServerID %s",
					fullFault.DBHeight,
					fullFault.SystemHeight,
					fullFault.ServerID.String()[4:12],
					fullFault.AuditServerID.String()[4:12])
				pl.State.AddAuthorityDelta(authorityDeltaString)

				consenLogger.WithFields(log.Fields{"dbht": fullFault.DBHeight, "sysht": fullFault.SystemHeight,
					"server": fullFault.ServerID.String()[4:12], "audit": fullFault.AuditServerID.String()[4:12]}).Info("Full fault success")
				//s.AddStatus(authorityDeltaString)

				pl.State.LastFaultAction = time.Now().Unix()
				markNoFault(pl, fullFault.GetVMIndex())
				nextIndex := (int(fullFault.VMIndex) + 1) % len(pl.FedServers)
				if pl.VMs[nextIndex].FaultFlag > 0 {
					markNoFault(pl, nextIndex)
				}

				s.LeaderPL = s.ProcessLists.Get(s.LLeaderHeight)
				s.Leader, s.LeaderVMIndex = s.LeaderPL.GetVirtualServers(s.CurrentMinute, s.IdentityChainID)

				fullFault.SetAlreadyProcessed()
				faultLogger.WithField("func", "ProcessFault").WithFields(fullFault.LogFields()).Warn("Fault Processed (Leader Replaced)")
				return true
			}
		}
	} else {
		// If we are here, this means that the FullFault message is incomplete
		// and we cannot execute it (it might not have enough signatures, or it
		// might lack the pledge from the audit server being promoted)

		// We need to see whether our signature is included, and match the fault if not
		// (assuming we agree with the basic premise of the fault)

		for _, signature := range fullFault.SignatureList.List {
			var issuerID [32]byte
			rawIssuerID := signature.GetKey()
			for i := 0; i < 32; i++ {
				if i < len(rawIssuerID) {
					issuerID[i] = rawIssuerID[i]
				}
			}

			lbytes := fullFault.GetCoreHash().Bytes()

			isPledge := false
			auth, _ := s.GetAuthority(fullFault.AuditServerID)
			if auth == nil {
				isPledge = false
			} else {
				valid, err := auth.VerifySignature(lbytes, signature.GetSignature())
				if err == nil && valid {
					isPledge = true
					fullFault.SetPledgeDone(true)
				}
			}

			sfSigned, err := s.FastVerifyAuthoritySignature(lbytes, signature, fullFault.DBHeight)

			if err == nil && (sfSigned > 0 || (sfSigned == 0 && isPledge)) {
				fullFault.AddFaultVote(issuerID, fullFault.GetSignature())
			}

			if s.Leader || s.IdentityChainID.IsSameAs(fullFault.AuditServerID) {
				if !fullFault.GetMyVoteTallied() {
					nsf := messages.NewServerFault(fullFault.ServerID, fullFault.AuditServerID, int(fullFault.VMIndex), fullFault.DBHeight,
						fullFault.Height, int(fullFault.SystemHeight), fullFault.Timestamp)
					sfbytes, err := nsf.MarshalForSignature()
					myAuth, _ := s.GetAuthority(s.IdentityChainID)
					if myAuth == nil || err != nil {
						continue
					}
					valid, err := myAuth.VerifySignature(sfbytes, signature.GetSignature())
					if err == nil && valid {
						fullFault.SetMyVoteTallied(true)
					}
				}
			}
		}

		if s.Leader || s.IdentityChainID.IsSameAs(fullFault.AuditServerID) {
			if !fullFault.GetMyVoteTallied() {
				now := time.Now().Unix()
				if now-fullFault.LastMatch > 5 && int(now-s.LastTiebreak) > s.FaultTimeout/2 {
					if fullFault.SigTally(s) >= len(pl.FedServers)-1 {
						s.LastTiebreak = now
					}

					nsf := messages.NewServerFault(fullFault.ServerID, fullFault.AuditServerID, int(fullFault.VMIndex),
						fullFault.DBHeight, fullFault.Height, int(fullFault.SystemHeight), fullFault.Timestamp)
					//s.AddStatus(fmt.Sprintf("Match FullFault: %s", nsf.String()))

					s.matchFault(nsf)
				}
			}
		}
	}

	return false
}

func (s *State) GetMsg(vmIndex int, dbheight int, height int) (interfaces.IMsg, error) {

	pl := s.ProcessLists.Get(uint32(dbheight))
	if pl == nil {
		return nil, errors.New("No Process List")
	}
	vms := pl.VMs
	if len(vms) <= vmIndex {
		return nil, errors.New("Bad VM Index")
	}
	vm := vms[vmIndex]
	if vm.Height > height {
		return vm.List[height], nil
	}
	return nil, nil
}

func (s *State) SendHeartBeat() {
	dbstate := s.DBStates.Get(int(s.LLeaderHeight - 1))
	if dbstate == nil {
		return
	}
	for _, auditServer := range s.GetAuditServers(s.LLeaderHeight) {
		if auditServer.GetChainID().IsSameAs(s.IdentityChainID) {
			hb := new(messages.Heartbeat)
			hb.DBHeight = s.LLeaderHeight
			hb.Timestamp = primitives.NewTimestampNow()
			hb.SecretNumber = s.GetSalt(hb.Timestamp)
			hb.DBlockHash = dbstate.DBHash
			hb.IdentityChainID = s.IdentityChainID
			hb.Sign(s.GetServerPrivateKey())
			hb.SendOut(s, hb)
		}
	}
}

func (s *State) UpdateECs(ec interfaces.IEntryCreditBlock) {
	now := s.GetTimestamp()
	for _, entry := range ec.GetEntries() {
		cc, ok := entry.(*entryCreditBlock.CommitChain)
		if ok && s.Replay.IsTSValid_(constants.INTERNAL_REPLAY, cc.GetSigHash().Fixed(), cc.GetTimestamp(), now) {
			if s.NoEntryYet(cc.EntryHash, cc.GetTimestamp()) {
				cmsg := new(messages.CommitChainMsg)
				cmsg.CommitChain = cc
				s.PutCommit(cc.EntryHash, cmsg)
			}
			continue
		}
		ce, ok := entry.(*entryCreditBlock.CommitEntry)
		if ok && s.Replay.IsTSValid_(constants.INTERNAL_REPLAY, ce.GetSigHash().Fixed(), ce.GetTimestamp(), now) {
			if s.NoEntryYet(ce.EntryHash, ce.GetTimestamp()) {
				emsg := new(messages.CommitEntryMsg)
				emsg.CommitEntry = ce
				s.PutCommit(ce.EntryHash, emsg)
			}
			continue
		}
	}
}

func (s *State) GetNewEBlocks(dbheight uint32, hash interfaces.IHash) interfaces.IEntryBlock {
	if dbheight <= s.GetHighestSavedBlk()+2 {
		pl := s.ProcessLists.Get(dbheight)
		if pl == nil {
			return nil
		}
		return pl.GetNewEBlocks(hash)
	}
	return nil
}

func (s *State) IsNewOrPendingEBlocks(dbheight uint32, hash interfaces.IHash) bool {
	if dbheight <= s.GetHighestSavedBlk()+2 {
		pl := s.ProcessLists.Get(dbheight)
		if pl == nil {
			return false
		}
		eblk := pl.GetNewEBlocks(hash)
		if eblk != nil {
			return true
		}

		return pl.IsPendingChainHead(hash)
	}
	return false
}

func (s *State) PutNewEBlocks(dbheight uint32, hash interfaces.IHash, eb interfaces.IEntryBlock) {
	pl := s.ProcessLists.Get(dbheight)
	pl.AddNewEBlocks(hash, eb)
	// We no longer need them in this map, as they are in the other
	pl.PendingChainHeads.Delete(hash.Fixed())
}

func (s *State) PutNewEntries(dbheight uint32, hash interfaces.IHash, e interfaces.IEntry) {
	pl := s.ProcessLists.Get(dbheight)
	pl.AddNewEntry(hash, e)
}

// Returns the oldest, not processed, Commit received
func (s *State) NextCommit(hash interfaces.IHash) interfaces.IMsg {
	c := s.Commits.Get(hash.Fixed()) //  s.Commits[hash.Fixed()]
	return c
}

// IsHighestCommit will determine if the commit given has more entry credits than the current
// commit in the commit hashmap. If there is no prior commit, this will also return true.
func (s *State) IsHighestCommit(hash interfaces.IHash, msg interfaces.IMsg) bool {
	e, ok1 := s.Commits.Get(hash.Fixed()).(*messages.CommitEntryMsg)
	m, ok1b := msg.(*messages.CommitEntryMsg)
	ec, ok2 := s.Commits.Get(hash.Fixed()).(*messages.CommitChainMsg)
	mc, ok2b := msg.(*messages.CommitChainMsg)

	// Keep the most entry credits. If the current (e,ec) is >=, then the message is not
	// the highest.
	switch {
	case ok1 && ok1b && e.CommitEntry.Credits >= m.CommitEntry.Credits:
	case ok2 && ok2b && ec.CommitChain.Credits >= mc.CommitChain.Credits:
	default:
		// Returns true when new commit is greater than old, or if old does not exist
		return true
	}
	return false
}

func (s *State) PutCommit(hash interfaces.IHash, msg interfaces.IMsg) {
	if s.IsHighestCommit(hash, msg) {
		s.Commits.Put(hash.Fixed(), msg)
	}
}

func (s *State) GetHighestAck() uint32 {
	return s.HighestAck
}

func (s *State) SetHighestAck(dbht uint32) {
	if dbht > s.HighestAck {
		s.HighestAck = dbht
	}
}

// This is the highest block signed off and recorded in the Database.
func (s *State) GetHighestSavedBlk() uint32 {
	v := s.DBStates.GetHighestSavedBlk()
	HighestSaved.Set(float64(v))
	return v
}

// This is the highest block signed off, but not necessarily validted.
func (s *State) GetHighestCompletedBlk() uint32 {
	v := s.DBStates.GetHighestCompletedBlk()
	HighestCompleted.Set(float64(v))
	return v
}

// This is lowest block currently under construction under the "leader".
func (s *State) GetLeaderHeight() uint32 {
	return s.LLeaderHeight
}

// The highest block for which we have received a message.  Sometimes the same as
// BuildingBlock(), but can be different depending or the order messages are recieved.
func (s *State) GetHighestKnownBlock() uint32 {
	if s.ProcessLists == nil {
		return 0
	}
	HighestKnown.Set(float64(s.HighestKnown))
	return s.HighestKnown
}

func (s *State) GetF(rt bool, adr [32]byte) (v int64) {
	ok := false
	if rt {
		pl := s.ProcessLists.Get(s.LLeaderHeight)
		if pl != nil {
			pl.FactoidBalancesTMutex.Lock()
			defer pl.FactoidBalancesTMutex.Unlock()
			v, ok = pl.FactoidBalancesT[adr]
		}
	}
	if !ok {
		s.FactoidBalancesPMutex.Lock()
		defer s.FactoidBalancesPMutex.Unlock()
		v = s.FactoidBalancesP[adr]
	}

	return v

}

// If rt == true, update the Temp balances.  Otherwise update the Permenent balances.
func (s *State) PutF(rt bool, adr [32]byte, v int64) {
	if rt {
		pl := s.ProcessLists.Get(s.LLeaderHeight)
		if pl != nil {
			pl.FactoidBalancesTMutex.Lock()
			defer pl.FactoidBalancesTMutex.Unlock()

			pl.FactoidBalancesT[adr] = v
		}
	} else {
		s.FactoidBalancesPMutex.Lock()
		defer s.FactoidBalancesPMutex.Unlock()
		s.FactoidBalancesP[adr] = v
	}
}

func (s *State) GetE(rt bool, adr [32]byte) (v int64) {
	ok := false
	if rt {
		pl := s.ProcessLists.Get(s.LLeaderHeight)
		if pl != nil {
			pl.ECBalancesTMutex.Lock()
			defer pl.ECBalancesTMutex.Unlock()
			v, ok = pl.ECBalancesT[adr]
		}
	}
	if !ok {
		s.ECBalancesPMutex.Lock()
		defer s.ECBalancesPMutex.Unlock()
		v = s.ECBalancesP[adr]
	}
	return v

}

// If rt == true, update the Temp balances.  Otherwise update the Permenent balances.
func (s *State) PutE(rt bool, adr [32]byte, v int64) {
	if rt {
		pl := s.ProcessLists.Get(s.LLeaderHeight)
		if pl != nil {
			pl.ECBalancesTMutex.Lock()
			defer pl.ECBalancesTMutex.Unlock()
			pl.ECBalancesT[adr] = v
		}
	} else {
		s.ECBalancesPMutex.Lock()
		defer s.ECBalancesPMutex.Unlock()
		s.ECBalancesP[adr] = v
	}
}

// Returns the Virtual Server Index for this hash if this server is the leader;
// returns -1 if we are not the leader for this hash
func (s *State) ComputeVMIndex(hash []byte) int {
	return s.LeaderPL.VMIndexFor(hash)
}

func (s *State) GetDBHeightComplete() uint32 {
	db := s.GetDirectoryBlock()
	if db == nil {
		return 0
	}
	return db.GetHeader().GetDBHeight()
}

func (s *State) GetDirectoryBlock() interfaces.IDirectoryBlock {
	if s.DBStates.Last() == nil {
		return nil
	}
	return s.DBStates.Last().DirectoryBlock
}

func (s *State) GetNewHash() interfaces.IHash {
	return new(primitives.Hash)
}

// Create a new Acknowledgement.  Must be called by a leader.  This
// call assumes all the pieces are in place to create a new acknowledgement
func (s *State) NewAck(msg interfaces.IMsg, balanceHash interfaces.IHash) interfaces.IMsg {

	vmIndex := msg.GetVMIndex()

	msg.SetLeaderChainID(s.IdentityChainID)
	ack := new(messages.Ack)
	ack.DBHeight = s.LLeaderHeight
	ack.VMIndex = vmIndex
	ack.Minute = byte(s.ProcessLists.Get(s.LLeaderHeight).VMs[vmIndex].LeaderMinute)
	ack.Timestamp = s.GetTimestamp()
	ack.SaltNumber = s.GetSalt(ack.Timestamp)
	copy(ack.Salt[:8], s.Salt.Bytes()[:8])
	ack.MessageHash = msg.GetMsgHash()
	ack.LeaderChainID = s.IdentityChainID
	ack.BalanceHash = balanceHash
	listlen := len(s.LeaderPL.VMs[vmIndex].List)
	if listlen == 0 {
		ack.Height = 0
		ack.SerialHash = ack.MessageHash
	} else {
		last := s.LeaderPL.GetAckAt(vmIndex, listlen-1)
		ack.Height = last.Height + 1
		ack.SerialHash, _ = primitives.CreateHash(last.MessageHash, ack.MessageHash)
	}

	ack.Sign(s)

	return ack
}
