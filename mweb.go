package neutrino

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/bits"

	"github.com/ltcsuite/ltcd/chaincfg/chainhash"
	"github.com/ltcsuite/ltcd/ltcutil/bloom"
	"github.com/ltcsuite/ltcd/txscript"
	"github.com/ltcsuite/ltcd/wire"
	"github.com/ltcsuite/ltcwallet/wtxmgr"
	"github.com/ltcsuite/neutrino/banman"
	"github.com/ltcsuite/neutrino/query"
	"lukechampine.com/blake3"
)

func verifyMwebHeader(
	mwebHeader *wire.MsgMwebHeader, mwebLeafset *wire.MsgMwebLeafset,
	lastHeight uint32, lastHash *chainhash.Hash) bool {

	if mwebHeader == nil || mwebLeafset == nil {
		return false
	}
	log.Infof("Got mwebheader and mwebleafset at (block_height=%v, block_hash=%v)",
		lastHeight, *lastHash)

	if mwebHeader.Merkle.Header.BlockHash() != *lastHash {
		log.Infof("Block hash mismatch, merkle header hash=%v, block hash=%v",
			mwebHeader.Merkle.Header.BlockHash(), *lastHash)
		return false
	}

	extractResult := bloom.VerifyMerkleBlock(&mwebHeader.Merkle)
	if !extractResult.Root.IsEqual(&mwebHeader.Merkle.Header.MerkleRoot) {
		log.Info("mwebheader merkle block is bad")
		return false
	}

	if !mwebHeader.Hogex.IsHogEx {
		log.Info("mwebheader hogex is not hogex")
		return false
	}

	// Validate that the hash of the HogEx transaction in the tx message
	// matches the hash in the merkleblock message, and that it’s the last
	// transaction committed to by the merkle root of the block.
	finalTx := extractResult.Match[len(extractResult.Match)-1]
	if mwebHeader.Hogex.TxHash() != *finalTx {
		log.Infof("Tx hash mismatch, hogex=%v, last merkle tx=%v",
			mwebHeader.Hogex.TxHash(), *finalTx)
		return false
	}
	finalTxPos := extractResult.Index[len(extractResult.Index)-1]
	if finalTxPos != mwebHeader.Merkle.Transactions-1 {
		log.Infof("Tx index mismatch, got=%v, expected=%v",
			finalTxPos, mwebHeader.Merkle.Transactions-1)
		return false
	}

	// Validate that the pubkey script of the first output contains the HogAddr,
	// which shall consist of <OP_8><0x20> followed by the 32-byte hash of the
	// MWEB header.
	mwebHeaderHash := mwebHeader.MwebHeader.Hash()
	script := append([]byte{txscript.OP_8, 0x20}, mwebHeaderHash[:]...)
	if !bytes.Equal(mwebHeader.Hogex.TxOut[0].PkScript, script) {
		log.Infof("HogAddr mismatch, hogex=%v, expected=%v",
			mwebHeader.Hogex.TxOut[0].PkScript, script)
		return false
	}

	// Verify that the hash of the leafset bitmap matches the
	// leafset_root value in the MWEB header.
	leafsetRoot := chainhash.Hash(blake3.Sum256(mwebLeafset.Leafset))
	if leafsetRoot != mwebHeader.MwebHeader.LeafsetRoot {
		log.Infof("Leafset root mismatch, leafset=%v, in header=%v",
			leafsetRoot, mwebHeader.MwebHeader.LeafsetRoot)
		return false
	}

	log.Infof("Verified mwebheader and mwebleafset at (block_height=%v, block_hash=%v)",
		lastHeight, *lastHash)
	return true
}

type (
	leafset []byte
	leafIdx uint64
	nodeIdx uint64
)

func (l leafset) contains(i leafIdx) bool {
	if int(i/8) >= len(l) {
		return false
	}
	return l[i/8]&(0x80>>(i%8)) > 0
}

func (l leafset) nextUnspent(i leafIdx) leafIdx {
	for {
		i++
		if l.contains(i) || int(i/8) >= len(l) {
			return i
		}
	}
}

func (i leafIdx) nodeIdx() nodeIdx {
	return nodeIdx(2*i) - nodeIdx(bits.OnesCount64(uint64(i)))
}

func (i nodeIdx) height() uint64 {
	height := uint64(i)
	h := 64 - bits.LeadingZeros64(uint64(i))
	for peakSize := uint64(1<<h - 1); peakSize > 0; peakSize >>= 1 {
		if height >= peakSize {
			height -= peakSize
		}
	}
	return height
}

func (i nodeIdx) leafIdx() leafIdx {
	leafIndex := uint64(0)
	numLeft := uint64(i)
	h := 64 - bits.LeadingZeros64(uint64(i))
	for peakSize := uint64(1<<h - 1); peakSize > 0; peakSize >>= 1 {
		if numLeft >= peakSize {
			leafIndex += (peakSize + 1) / 2
			numLeft -= peakSize
		}
	}
	return leafIdx(leafIndex)
}

func (i nodeIdx) left(height uint64) nodeIdx {
	return i - (1 << height)
}

func (i nodeIdx) right() nodeIdx {
	return i - 1
}

func (i nodeIdx) hash(data []byte) *chainhash.Hash {
	h := blake3.New(32, nil)
	binary.Write(h, binary.LittleEndian, uint64(i))
	wire.WriteVarBytes(h, 0, data)
	return (*chainhash.Hash)(h.Sum(nil))
}

func (i nodeIdx) parentHash(left, right []byte) *chainhash.Hash {
	h := blake3.New(32, nil)
	binary.Write(h, binary.LittleEndian, uint64(i))
	h.Write(left)
	h.Write(right)
	return (*chainhash.Hash)(h.Sum(nil))
}

func calcPeaks(nodes uint64) (peaks []nodeIdx) {
	sumPrevPeaks := uint64(0)
	h := 64 - bits.LeadingZeros64(nodes)
	for peakSize := uint64(1<<h - 1); peakSize > 0; peakSize >>= 1 {
		if nodes >= peakSize {
			peaks = append(peaks, nodeIdx(sumPrevPeaks+peakSize-1))
			sumPrevPeaks += peakSize
			nodes -= peakSize
		}
	}
	return
}

type verifyMwebUtxosVars struct {
	mwebUtxos                 *wire.MsgMwebUtxos
	leafset                   leafset
	firstLeafIdx, lastLeafIdx leafIdx
	leavesUsed, hashesUsed    int
	isProofHash               map[nodeIdx]bool
}

func (v *verifyMwebUtxosVars) nextLeaf() (leafIndex leafIdx, hash *chainhash.Hash) {
	if v.leavesUsed == len(v.mwebUtxos.Utxos) {
		return
	}
	utxo := v.mwebUtxos.Utxos[v.leavesUsed]
	leafIndex = leafIdx(utxo.LeafIndex)
	hash = utxo.OutputId
	v.leavesUsed++
	return
}

func (v *verifyMwebUtxosVars) nextHash(nodeIdx nodeIdx) (hash *chainhash.Hash) {
	if v.hashesUsed == len(v.mwebUtxos.ProofHashes) {
		return
	}
	hash = v.mwebUtxos.ProofHashes[v.hashesUsed]
	v.hashesUsed++
	v.isProofHash[nodeIdx] = true
	return
}

func (v *verifyMwebUtxosVars) calcNodeHash(nodeIdx nodeIdx, height uint64) *chainhash.Hash {
	if nodeIdx < v.firstLeafIdx.nodeIdx() || v.isProofHash[nodeIdx] {
		return v.nextHash(nodeIdx)
	}
	if height == 0 {
		leafIdx := nodeIdx.leafIdx()
		if !v.leafset.contains(leafIdx) {
			return nil
		}
		leafIdx2, outputId := v.nextLeaf()
		if leafIdx != leafIdx2 || outputId == nil {
			return nil
		}
		return nodeIdx.hash(outputId[:])
	}
	left := v.calcNodeHash(nodeIdx.left(height), height-1)
	var right *chainhash.Hash
	if v.lastLeafIdx.nodeIdx() <= nodeIdx.left(height) {
		right = v.nextHash(nodeIdx.right())
	} else {
		right = v.calcNodeHash(nodeIdx.right(), height-1)
	}
	switch {
	case left == nil && right == nil:
		return nil
	case left == nil:
		if left = v.nextHash(nodeIdx.left(height)); left == nil {
			return nil
		}
	case right == nil:
		if right = v.nextHash(nodeIdx.right()); right == nil {
			return nil
		}
	}
	return nodeIdx.parentHash(left[:], right[:])
}

func verifyMwebUtxos(mwebHeader *wire.MwebHeader,
	mwebLeafset leafset, mwebUtxos *wire.MsgMwebUtxos) bool {

	if mwebUtxos.StartIndex == 0 &&
		len(mwebUtxos.Utxos) == 0 &&
		len(mwebUtxos.ProofHashes) == 0 &&
		mwebHeader.OutputRoot.IsEqual(&chainhash.Hash{}) &&
		mwebHeader.OutputMMRSize == 0 {
		return true
	} else if len(mwebUtxos.Utxos) == 0 ||
		mwebHeader.OutputMMRSize == 0 {
		return false
	}

	v := &verifyMwebUtxosVars{
		mwebUtxos:    mwebUtxos,
		leafset:      mwebLeafset,
		firstLeafIdx: leafIdx(mwebUtxos.StartIndex),
		lastLeafIdx:  leafIdx(mwebUtxos.StartIndex),
		isProofHash:  make(map[nodeIdx]bool),
	}

	for i := 0; ; i++ {
		if !v.leafset.contains(v.lastLeafIdx) {
			return false
		}
		if leafIdx(mwebUtxos.Utxos[i].LeafIndex) != v.lastLeafIdx {
			return false
		}
		if i == len(mwebUtxos.Utxos)-1 {
			break
		}
		v.lastLeafIdx = v.leafset.nextUnspent(v.lastLeafIdx)
	}

	var (
		nextNodeIdx = leafIdx(mwebHeader.OutputMMRSize).nodeIdx()
		peaks       = calcPeaks(uint64(nextNodeIdx))
		peakHashes  []*chainhash.Hash
	)
	for i := 0; i < 2; i++ {
		peakHashes = nil
		v.leavesUsed = 0
		v.hashesUsed = 0

		for _, peakNodeIdx := range peaks {
			peakHash := v.calcNodeHash(peakNodeIdx, peakNodeIdx.height())
			if peakHash == nil {
				peakHash = v.nextHash(peakNodeIdx)
				if peakHash == nil {
					return false
				}
			}
			peakHashes = append(peakHashes, peakHash)
			if v.lastLeafIdx.nodeIdx() <= peakNodeIdx {
				if peakNodeIdx != peaks[len(peaks)-1] {
					baggedPeak := v.nextHash(nextNodeIdx)
					if baggedPeak == nil {
						return false
					}
					peakHashes = append(peakHashes, baggedPeak)
				}
				break
			}
		}
		if v.leavesUsed != len(v.mwebUtxos.Utxos) ||
			v.hashesUsed != len(v.mwebUtxos.ProofHashes) {
			return false
		}
	}

	baggedPeak := peakHashes[len(peakHashes)-1]
	for i := len(peakHashes) - 2; i >= 0; i-- {
		baggedPeak = nextNodeIdx.parentHash(peakHashes[i][:], baggedPeak[:])
	}
	return baggedPeak.IsEqual(&mwebHeader.OutputRoot)
}

// mwebUtxosQuery holds all information necessary to perform and
// handle a query for mweb utxos.
type mwebUtxosQuery struct {
	blockMgr   *blockManager
	mwebHeader *wire.MwebHeader
	leafset    leafset
	msgs       []wire.Message
	utxosChan  chan *wire.MsgMwebUtxos
}

func (b *blockManager) getMwebUtxos(mwebHeader *wire.MwebHeader,
	newLeafset leafset, lastHeight uint32, lastHeader *wire.BlockHeader) {

	log.Infof("Fetching set of mweb utxos from "+
		"height=%v, hash=%v", lastHeight, lastHeader.BlockHash())

	newNumLeaves := mwebHeader.OutputMMRSize
	dbLeafset, oldNumLeaves, err := b.cfg.MwebCoins.GetLeafSet()
	if err != nil {
		panic(fmt.Sprintf("couldn't read mweb coins db: %v", err))
	}
	oldLeafset := leafset(dbLeafset)

	// Skip over common prefix
	var index uint64
	for index < uint64(len(oldLeafset)) &&
		index < uint64(len(newLeafset)) &&
		oldLeafset[index] == newLeafset[index] {
		index++
	}

	type span struct {
		start uint64
		count uint16
	}
	var addLeaf span
	var addedLeaves []span
	var removedLeaves []uint64
	addLeafSpan := func() {
		if addLeaf.count > 0 {
			addedLeaves = append(addedLeaves, addLeaf)
			addLeaf = span{}
		}
	}
	for index *= 8; index < oldNumLeaves || index < newNumLeaves; index++ {
		if oldLeafset.contains(leafIdx(index)) {
			addLeafSpan()
			if !newLeafset.contains(leafIdx(index)) {
				removedLeaves = append(removedLeaves, index)
			}
		} else if newLeafset.contains(leafIdx(index)) {
			if addLeaf.count == 0 {
				addLeaf.start = index
			}
			addLeaf.count++
			if addLeaf.count == wire.MaxMwebUtxosPerQuery {
				addLeafSpan()
			}
		}
	}
	addLeafSpan()

	var queryMsgs []wire.Message
	for _, addLeaf := range addedLeaves {
		queryMsgs = append(queryMsgs,
			wire.NewMsgGetMwebUtxos(lastHeader.BlockHash(),
				addLeaf.start, addLeaf.count, wire.MwebNetUtxoCompact))
	}

	// We'll also create an additional map that we'll use to
	// re-order the responses as we get them in.
	queryResponses := make(map[uint64]*wire.MsgMwebUtxos, len(queryMsgs))

	batchesCount := len(queryMsgs)
	if batchesCount == 0 {
		b.purgeSpentMwebTxos(newLeafset, newNumLeaves, removedLeaves)
		return
	}

	log.Infof("Starting to query for mweb utxos from index=%v", addedLeaves[0].start)
	log.Infof("Attempting to query for %v mwebutxos batches", batchesCount)

	// With the set of messages constructed, we'll now request the batch
	// all at once. This message will distribute the mwebutxos requests
	// amongst all active peers, effectively sharding each query
	// dynamically.
	utxosChan := make(chan *wire.MsgMwebUtxos, len(queryMsgs))
	q := mwebUtxosQuery{
		blockMgr:   b,
		mwebHeader: mwebHeader,
		leafset:    newLeafset,
		msgs:       queryMsgs,
		utxosChan:  utxosChan,
	}

	// Hand the queries to the work manager, and consume the verified
	// responses as they come back.
	errChan := b.cfg.QueryDispatcher.Query(
		q.requests(), query.Cancel(b.quit),
	)

	b.mwebUtxosCallbacksMtx.Lock()
	defer b.mwebUtxosCallbacksMtx.Unlock()

	// Keep waiting for more mwebutxos as long as we haven't received an
	// answer for our last getmwebutxos, and no error is encountered.
	totalUtxos := 0
	for i := 0; i < len(addedLeaves); {
		var r *wire.MsgMwebUtxos
		select {
		case r = <-utxosChan:
		case err := <-errChan:
			switch {
			case err == query.ErrWorkManagerShuttingDown:
				return
			case err != nil:
				log.Errorf("Query finished with error before "+
					"all responses received: %v", err)
				return
			}

			// The query did finish successfully, but continue to
			// allow picking up the last mwebutxos sent on the
			// utxosChan.
			continue

		case <-b.quit:
			return
		}

		// Find the first and last indices for the mweb utxos
		// represented by this message.
		startIndex := r.Utxos[0].LeafIndex
		lastIndex := r.Utxos[len(r.Utxos)-1].LeafIndex
		curIndex := addedLeaves[i].start

		log.Debugf("Got mwebutxos from index=%v to "+
			"index=%v, block hash=%v", startIndex,
			lastIndex, r.BlockHash)

		// If this is out of order but not yet written, we can
		// store them for later.
		if startIndex > curIndex {
			log.Debugf("Got response for mwebutxos at "+
				"index=%v, only at index=%v, stashing",
				startIndex, curIndex)
		}

		// If this is out of order stuff that's already been
		// written, we can ignore it.
		if lastIndex < curIndex {
			log.Debugf("Received out of order reply "+
				"lastIndex=%v, already written", lastIndex)
			continue
		}

		// Add the verified response to our cache.
		queryResponses[startIndex] = r

		// Then, we cycle through any cached messages, adding
		// them to the batch and deleting them from the cache.
		for i < len(addedLeaves) {
			// If we don't yet have the next response, then
			// we'll break out so we can wait for the peers
			// to respond with this message.
			curIndex = addedLeaves[i].start
			r, ok := queryResponses[curIndex]
			if !ok {
				break
			}

			// We have another response to write, so delete
			// it from the cache and write it.
			delete(queryResponses, curIndex)

			log.Debugf("Writing mwebutxos at index=%v", curIndex)

			err := b.cfg.MwebCoins.PutCoins(r.Utxos)
			if err != nil {
				panic(fmt.Sprintf("couldn't write mweb coins: %v", err))
			}

			block := &wtxmgr.BlockMeta{
				Block: wtxmgr.Block{
					Hash:   lastHeader.BlockHash(),
					Height: int32(lastHeight),
				},
				Time: lastHeader.Timestamp,
			}
			for _, cb := range b.mwebUtxosCallbacks {
				cb(newLeafset, r.Utxos, block)
			}

			totalUtxos += len(r.Utxos)

			// Update the next index to write.
			i++
		}
	}

	log.Infof("Successfully got %v mweb utxos", totalUtxos)

	b.purgeSpentMwebTxos(newLeafset, newNumLeaves, removedLeaves)
}

func (b *blockManager) purgeSpentMwebTxos(newLeafset leafset,
	newNumLeaves uint64, removedLeaves []uint64) {

	if len(removedLeaves) > 0 {
		log.Infof("Purging %v spent mweb txos from db", len(removedLeaves))
	}

	err := b.cfg.MwebCoins.PutLeafSetAndPurge(
		newLeafset, newNumLeaves, removedLeaves)
	if err != nil {
		panic(fmt.Sprintf("couldn't purge mweb txos: %v", err))
	}
}

// requests creates the query.Requests for this mwebutxos query.
func (m *mwebUtxosQuery) requests() []*query.Request {
	reqs := make([]*query.Request, len(m.msgs))
	for idx, msg := range m.msgs {
		reqs[idx] = &query.Request{
			Req:        msg,
			HandleResp: m.handleResponse,
		}
	}
	return reqs
}

// handleResponse is the internal response handler used for requests for this
// mwebutxos query.
func (m *mwebUtxosQuery) handleResponse(req, resp wire.Message,
	peerAddr string) query.Progress {

	r, ok := resp.(*wire.MsgMwebUtxos)
	if !ok {
		// We are only looking for mwebutxos messages.
		return query.Progress{
			Finished:   false,
			Progressed: false,
		}
	}

	q, ok := req.(*wire.MsgGetMwebUtxos)
	if !ok {
		// We sent a getmwebutxos message, so that's what we should be
		// comparing against.
		return query.Progress{
			Finished:   false,
			Progressed: false,
		}
	}

	// The response doesn't match the query.
	if !q.BlockHash.IsEqual(&r.BlockHash) ||
		q.StartIndex != r.StartIndex ||
		q.OutputFormat != r.OutputFormat ||
		q.NumRequested != uint16(len(r.Utxos)) {
		return query.Progress{
			Finished:   false,
			Progressed: false,
		}
	}

	if !verifyMwebUtxos(m.mwebHeader, m.leafset, r) {
		log.Warnf("Failed to verify mweb utxos at index %v!!!",
			r.StartIndex)

		// If the peer gives us a bad mwebutxos message,
		// then we'll ban the peer so we can re-allocate
		// the query elsewhere.
		err := m.blockMgr.cfg.BanPeer(
			peerAddr, banman.InvalidMwebUtxos,
		)
		if err != nil {
			log.Errorf("Unable to ban peer %v: %v", peerAddr, err)
		}

		return query.Progress{
			Finished:   false,
			Progressed: false,
		}
	}

	// At this point, the response matches the query,
	// so we'll deliver the verified utxos on the utxosChan.
	// We'll also return a Progress indicating the query
	// finished, that the peer looking for the answer to this
	// query can move on to the next query.
	select {
	case m.utxosChan <- r:
	case <-m.blockMgr.quit:
		return query.Progress{
			Finished:   false,
			Progressed: false,
		}
	}

	return query.Progress{
		Finished:   true,
		Progressed: true,
	}
}

func (b *blockManager) notifyAddedMwebUtxos(leafSet []byte) error {
	b.mwebUtxosCallbacksMtx.Lock()
	defer b.mwebUtxosCallbacksMtx.Unlock()

	dbLeafset, newNumLeaves, err := b.cfg.MwebCoins.GetLeafSet()
	if err != nil {
		return err
	}
	oldLeafset := leafset(leafSet)
	newLeafset := leafset(dbLeafset)

	// Skip over common prefix
	var index uint64
	for index < uint64(len(oldLeafset)) &&
		index < uint64(len(newLeafset)) &&
		oldLeafset[index] == newLeafset[index] {
		index++
	}

	var addedLeaves []uint64
	for index *= 8; index < newNumLeaves; index++ {
		if !oldLeafset.contains(leafIdx(index)) &&
			newLeafset.contains(leafIdx(index)) {
			addedLeaves = append(addedLeaves, index)
		}
	}

	utxos, err := b.cfg.MwebCoins.FetchLeaves(addedLeaves)
	if err != nil {
		return err
	}

	header, height, err := b.cfg.BlockHeaders.ChainTip()
	if err != nil {
		return err
	}

	block := &wtxmgr.BlockMeta{
		Block: wtxmgr.Block{
			Hash:   header.BlockHash(),
			Height: int32(height),
		},
		Time: header.Timestamp,
	}
	for _, cb := range b.mwebUtxosCallbacks {
		cb(newLeafset, utxos, block)
	}

	return nil
}
