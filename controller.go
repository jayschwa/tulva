// Copyright 2013 Jari Takkala and Brian Dignan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"log"
	"launchpad.net/tomb"
	"sort"
)

/*
const (
	maxSimultaneousDownloadsPerPeer = 5
)*/

type RarityMap struct {
	data map[int][]int
}

func NewRarityMap() *RarityMap {
	r := new(RarityMap)
	r.data = make(map[int][]int)
	return r
}

// Add a new rarity -> pieceNum mapping. 
func (r *RarityMap) put(rarity int, pieceNum int) {
	if _, exists := r.data[rarity]; !exists {
		r.data[rarity] = make([]int, 0)
	}

	r.data[rarity] = append(r.data[rarity], pieceNum)
}

// Flatten out the map into a slice that's sorted by rarity. 
func (r *RarityMap) getPiecesByRarity() []int {
	pieces := make([]int, 0)

	// put all rarity map keys into a temporary unsorted slice
	keys := make([]int, 0)
	for rarity, _ := range r.data {
		keys = append(keys, rarity)
	}

	// sort the slice of keys (rarity) in ascending order
	sort.Ints(keys)

	// Get the map value for each key (starting with the lowest) and 
	// concatenate that slice of pieces (for that rarity) to the result
	for _, rarity := range keys {
		pieceNums := r.data[rarity]
		pieces = append(pieces, pieceNums...)
	}

	return pieces
}

type Controller struct {
	finishedPieces []bool
	pieceHashes []string
	activeRequestsTotals []int 
	peers map[string]*PeerInfo
	maxSimultaneousDownloadsPerPeer int
	rxChans *ControllerRxChans
	t tomb.Tomb
}

type ControllerPeerChans struct {
	requestPiece chan RequestPiece   // Other end is Peer. Used to tell the peer to request a particular piece.
	cancelPiece  chan CancelPiece    // Other end is Peer. Used to tell the peer to cancel a particular piece.
	havePiece    chan chan HavePiece // Other end is Peer. Used to give the peer the initial bitfield and new pieces.
}

func NewControllerPeerChans() *ControllerPeerChans {
	cpc := new(ControllerPeerChans)
	cpc.requestPiece = make(chan RequestPiece)
	cpc.cancelPiece = make(chan CancelPiece)
	cpc.havePiece = make(chan chan HavePiece)
	return cpc
}

type ControllerDiskIOChans struct {
	receivedPiece chan ReceivedPiece // Other end is IO 
}

func NewControllerDiskIOChans() *ControllerDiskIOChans {
	return &ControllerDiskIOChans{ receivedPiece: make(chan ReceivedPiece) }
}

type ControllerPeerManagerChans struct {
	newPeer chan PeerComms  // Other end is the PeerManager
	deadPeer chan string  // Other end is the PeerManager
}

func NewControllerPeerManagerChans() *ControllerPeerManagerChans {
	return &ControllerPeerManagerChans{ newPeer: make(chan PeerComms)}
}

type PeerControllerChans struct {
	chokeStatus 	chan PeerChokeStatus  // Other end is Peer. Used when the peer is becomes choked or unchoked
	havePiece 		chan chan HavePiece  // Other end is Peer. used When the peer receives a HAVE message
}

func NewPeerControllerChans() *PeerControllerChans {
	return &PeerControllerChans{ chokeStatus: make(chan PeerChokeStatus), havePiece: make(chan chan HavePiece)}
}

type ControllerRxChans struct {
	diskIO 		ControllerDiskIOChans
	peerManager ControllerPeerManagerChans
	peer 		PeerControllerChans
}

func NewControllerRxChans( diskIO *ControllerDiskIOChans, peerManager *ControllerPeerManagerChans, peer *PeerControllerChans) *ControllerRxChans {
	return &ControllerRxChans{ *diskIO, *peerManager, *peer }
}

func NewController(finishedPieces []bool, 
					pieceHashes []string, 
					rxChans *ControllerRxChans) *Controller {

	cont := new(Controller)
	cont.finishedPieces = finishedPieces
	cont.pieceHashes = pieceHashes
	cont.rxChans = rxChans
	cont.peers = make(map[string]*PeerInfo)
	cont.activeRequestsTotals = make([]int, len(finishedPieces))
	cont.maxSimultaneousDownloadsPerPeer = 5  // suggested default 
	return cont
}

func (cont *Controller) Stop() error {
	log.Println("Controller : Stop : Stopping")
	cont.t.Kill(nil)
	return cont.t.Wait()
}

func (cont *Controller) sendHaveToPeersWhoNeedPiece(pieceNum int) {
	for _, peerInfo := range cont.peers {
		if !peerInfo.availablePieces[pieceNum] {

			// This peer doesn't have the piece that we just finished. Send them a HAVE message. 
			log.Printf("Controller : sendHaveToPeersWhoNeedPiece : Sending HAVE to %s for piece %d", peerInfo.peerName, pieceNum)

			haveMessage := new(HavePiece)

			// When sent from the controller to the peer, the only relevant field is the piece number
			// field. The peerName field doesn't need to be set, because the message is being sent
			// directly to the peer 
			haveMessage.pieceNum = pieceNum

			go func() { 
				// Create a temporary channel that's sent through the main HavePiece channel
				innerChan := make(chan HavePiece)
				peerInfo.chans.havePiece <- innerChan

				// Now that the other side has the innerChan (and is blocking on it), send the
				// HAVE message.
				innerChan <- *haveMessage

				// Close the inner channel indicating to the other side that there are no more pieces 
				close(innerChan) 
			}()

		}
	}
}

func (cont *Controller) removePieceFromActiveRequests(piece ReceivedPiece) {
	finishingPeer := cont.peers[piece.peerName]
	if _, exists := finishingPeer.activeRequests[piece.pieceNum]; exists {
		// Remove this piece from the peer's activeRequests set
		delete(finishingPeer.activeRequests, piece.pieceNum)

		// Decrement activeRequestsTotals for this piece by one (one less peer is downloading it)
		cont.activeRequestsTotals[piece.pieceNum]--

		// Check every peer to see if they're also downloading this piece.  
		for peerName, peerInfo := range cont.peers {
			if _, exists := peerInfo.activeRequests[piece.pieceNum]; exists {
				// This peer was also working on the same piece
				log.Printf("Controller : removePieceFromActiveRequests : %s was also working on piece %d which is finished. Sending a CANCEL", peerName, piece.pieceNum)
				
				// Remove this piece from the peer's activeRequests set
				delete(peerInfo.activeRequests, piece.pieceNum)
				
				// Decrement activeRequestsTotals for this piece by one (one less peer is downloading it)
				cont.activeRequestsTotals[piece.pieceNum]--

				cancelMessage := new(CancelPiece)
				cancelMessage.pieceNum = piece.pieceNum

				// Tell this peer to stop downloading this piece because it's already finished. 
				go func() { peerInfo.chans.cancelPiece <- *cancelMessage }()
			}
		}


		stuckRequests := cont.activeRequestsTotals[piece.pieceNum]
		if stuckRequests != 0 {
			log.Fatalf("Controller : removePieceFromActiveRequests : Somehow there are %d stuck requests for piece number %d", stuckRequests, piece.pieceNum)
		}

	} else {
		// The peer just finished this piece, but it wasn't in its active request list
		log.Printf("Controller : removePieceFromActiveRequests : %s finished piece %d, but that piece wasn't in its active request list", piece.peerName, piece.pieceNum)
	}
}

func (cont *Controller) createPeerPieceTotals() []int {
	peerPieceTotals := make([]int, len(cont.finishedPieces))

	for _, peerInfo := range cont.peers {
		// Only factor the peer into the 'rarity' equation if it's active. 
		if !peerInfo.isChoked {
			for pieceNum, hasPiece := range peerInfo.availablePieces {
				if hasPiece {
					// The peer has this piece. Increment the peer count for this piece. 
					peerPieceTotals[pieceNum]++
				}
			}
		}
	}

	return peerPieceTotals
}

func (cont *Controller) createRaritySlice() []int {
	rarityMap := NewRarityMap()

	peerPieceTotals := cont.createPeerPieceTotals()

	for pieceNum, total := range peerPieceTotals {
		if cont.finishedPieces[pieceNum] {
			continue
		} 

		rarityMap.put(total, pieceNum)
	}
	return rarityMap.getPiecesByRarity()
}

func (cont *Controller) updateQuantityNeededForPeer(peerInfo *PeerInfo) {
	qtyPiecesNeeded := 0
	for pieceNum, pieceFinished := range cont.finishedPieces {
		if !pieceFinished && peerInfo.availablePieces[pieceNum] {
			qtyPiecesNeeded++
		}
	}
	// Overwrite the old value with the new value just computed
	peerInfo.qtyPiecesNeeded = qtyPiecesNeeded
}

type PiecePriority struct {
	pieceNum int
	activeRequestsTotal int
	rarityIndex int
}

type PiecePrioritySlice []PiecePriority

func (pps PiecePrioritySlice) Less(i, j int) bool {
	if pps[i].activeRequestsTotal != pps[j].activeRequestsTotal {
		// Determine which is less ONLY by activeRequestsTotal, and not by rarityIndex
		// because activeRequestsTotals influences sorting order more than rarityIndex. 
		return pps[i].activeRequestsTotal <= pps[j].activeRequestsTotal

	} else {
		// Since activeRequestsTotal is the same for both, use rarityIndex as a tie
		// breaker
		return pps[i].rarityIndex <= pps[j].rarityIndex
	} 
}

func (pps PiecePrioritySlice) Swap(i, j int) {
	pps[i], pps[j] = pps[j], pps[i]
}

func (pps PiecePrioritySlice) Len() int {
	return len(pps)
}

// Convert the PiecePrioritySlice to a simple slice of integers (pieces) sorted by priority
func (pps PiecePrioritySlice) toSortedPieceSlice() []int {

	pieceSlice := make([]int, 0)

	// Sort the PiecePrioritySlice before iterating over it. 
	sort.Sort(pps)
	
	for _, pp := range pps {
		pieceSlice = append(pieceSlice, pp.pieceNum)
	}

	return pieceSlice
}

func (cont *Controller) createDownloadPriorityForPeer(peerInfo *PeerInfo, raritySlice []int) []int {
	// Create an unsorted PiecePrioritySlice object for each available piece on this peer that we need. 
	piecePrioritySlice := make(PiecePrioritySlice, 0)

	for rarityIndex, pieceNum := range raritySlice {
		if peerInfo.availablePieces[pieceNum] {
			if _, exists := peerInfo.activeRequests[pieceNum]; !exists {				
				// 1) The peer has this piece available
				// 2) We need this piece, because it's in the raritySlice
				// 3) The peer is not already working on this piece (not in activeRequests)

				pp := new(PiecePriority)
				pp.pieceNum = pieceNum
				pp.activeRequestsTotal = cont.activeRequestsTotals[pieceNum]
				pp.rarityIndex = rarityIndex

				piecePrioritySlice = append(piecePrioritySlice, *pp)
			}
		}
	}

	return piecePrioritySlice.toSortedPieceSlice()
}


func (cont *Controller) sendRequestsToPeer(peerInfo *PeerInfo, raritySlice []int) {

	// Create the slice of pieces that this peer should work on next. It will not 
	// include pieces that have already been written to disk, or pieces that the 
	// peer is already working on. 
	downloadPriority := cont.createDownloadPriorityForPeer(peerInfo, raritySlice)

	log.Printf("Controller : SendRequestsToPeer : Built downloadPriority with %d pieces for peer %s", len(downloadPriority), peerInfo.peerName)

	for _, pieceNum := range downloadPriority {
		if len(peerInfo.activeRequests) >= cont.maxSimultaneousDownloadsPerPeer {
			// We've sent enough requests
			break
		}

		// Create a new RequestPiece message and send it to the peer
		requestMessage := new(RequestPiece)
		requestMessage.pieceNum = pieceNum
		requestMessage.expectedHash = cont.pieceHashes[pieceNum]
		log.Printf("Controller : SendRequestsToPeer : Requesting %s to get pieceNum %d", peerInfo.peerName, pieceNum)
		go func() { peerInfo.chans.requestPiece <- *requestMessage }()

		// Add this pieceNum to the set of pieces that this peer is working on
		peerInfo.activeRequests[pieceNum] = struct{}{}

		// Increment the number of peers that are working on this piece. 
		cont.activeRequestsTotals[pieceNum]++

	}
}

func sendBitfieldOverChannel(outerChan chan<- chan HavePiece, peerName string, bitfield []bool) {

	bitfieldCopy := make([]bool, len(bitfield))
	copy(bitfieldCopy, bitfield)

	go func() {

		innerChan := make(chan HavePiece)
		outerChan <- innerChan

		for pieceNum, havePiece := range bitfield {
			if havePiece {
				haveMessage := new(HavePiece)
				haveMessage.peerName = peerName
				haveMessage.pieceNum = pieceNum
				innerChan <- *haveMessage
			}
		}
		close(innerChan)
	}()
}

func (cont *Controller) removeUnfinishedWorkForPeer(peerInfo *PeerInfo) {
	// First decrement activeRequestsTotals for each piece that this peer was working on
	for pieceNum, _ := range peerInfo.activeRequests {
		cont.activeRequestsTotals[pieceNum]--
	}

	// Next empty out the set
	peerInfo.activeRequests = make(map[int]struct{})
}


func (cont *Controller) Run() {
	log.Println("Controller : Run : Started")
	defer cont.t.Done()
	defer log.Println("Controller : Run : Completed")

	for {
		select {

		// === START OF MESSAGES FROM DISK_IO === 
		case piece := <- cont.rxChans.diskIO.receivedPiece:

			_, exists := cont.peers[piece.peerName]

			if !exists {
				log.Printf("Controller : Run (Received Piece) : WARNING. Was notified that %s finished downloading %d, but it doesn't currently exist in the peers mapping.", piece.peerName, piece.pieceNum)
			} else if !cont.peers[piece.peerName].isChoked {
				log.Printf("Controller : Run (Received Piece) : %s finished downloading piece number %d", piece.peerName, piece.pieceNum)
			} else {
				log.Printf("Controller : Run (Received Piece) : WARNING. Was notified that %s finished downloading %d but it's currently choked.", piece.peerName, piece.pieceNum)
			}

			// Update our bitfield to show that we now have that piece
			cont.finishedPieces[piece.pieceNum] = true

			// For every peer that doesn't already have this piece, send them a HAVE message
			cont.sendHaveToPeersWhoNeedPiece(piece.pieceNum)

			// Remove this piece from the active request list for the peer that 
			// finished the download, along with all other peers who were downloading
			// it. 
			cont.removePieceFromActiveRequests(piece)

			// Create a slice of pieces sorted by rarity
			raritySlice := cont.createRaritySlice()

			// Given the updated finishedPieces slice, update the quantity of pieces
			// that are needed from each peer. This step is required to later sort 
			// peerInfo slices by the quantity of needed pieces. 
			for _, peerInfo := range cont.peers {
				cont.updateQuantityNeededForPeer(peerInfo)
			}
			
			// Create a PeerInfo slice sorted by qtyPiecesNeeded
			sortedPeers := sortedPeersByQtyPiecesNeeded(cont.peers)

			// Iterate through the sorted peerInfo slice. For each Peer that isn't 
			// currently requesting the max amount of pieces, send more piece requests. 
			for _, peerInfo := range sortedPeers {
				// Confirm that this peer is still connected and is available to take requests
				// and also that the peer needs more requests
				if !peerInfo.isChoked && len(peerInfo.activeRequests) < cont.maxSimultaneousDownloadsPerPeer {
					cont.sendRequestsToPeer(peerInfo, raritySlice)
				}
			}
		// === END OF MESSAGES FROM DISK_IO === 


		// === START OF MESSAGES FROM PEER_MANAGER === 
		case peerComms := <- cont.rxChans.peerManager.newPeer:

			peerInfo := NewPeerInfo(len(cont.finishedPieces), peerComms)

			// Throw an error if the peer is duplicate (same IP/Port. should never happen)
			if _, exists := cont.peers[peerInfo.peerName]; exists {
				log.Fatalf("Controller : Run (New Peer) : Received pre-existing peer with ID of %s over newPeer channel", peerInfo.peerName)
			} else {
				log.Printf("Controller : Run (New Peer) : Received a new PeerComms with peerName of %s", peerInfo.peerName)
			}

			// Add PeerInfo to the peers map using IP:Port as the key
			cont.peers[peerInfo.peerName] = peerInfo

			sendBitfieldOverChannel(peerInfo.chans.havePiece, peerInfo.peerName, cont.finishedPieces)

			// We're not going to send requests to this peer yet. Once we receive a full bitfield from the peer
			// through HAVE messages, we'll then send requests. 


		case peerName := <- cont.rxChans.peerManager.deadPeer:

			_, exists := cont.peers[peerName]

			if !exists {
				log.Fatalf("Controller : Run (Dead Peer) : Was told that %s is dead, but that peer doesn't exist in the mapping", peerName)
			}

			delete(cont.peers, peerName)

		// === END OF MESSAGES FROM PEER_MANAGER === 



		// === START OF MESSAGES FROM PEER === 
		case chokeStatus := <- cont.rxChans.peer.chokeStatus:
			// The peer is tell us that it can no longer work on a particular piece. 
			log.Printf("Controller : Run (Choke Status) : Received a PeerChokeStatus from %s with value %t", chokeStatus.peerName, chokeStatus.isChoked)

			peerInfo, exists := cont.peers[chokeStatus.peerName]

			if !exists {
				log.Fatalf("Controller : Run (Choke Status) : Unable to process PeerChokeStatus from %s because it doesn't exist in the peers mapping")
			}

			peerInfo.isChoked = chokeStatus.isChoked
			
			if chokeStatus.isChoked {
				// The peer (presumably) transitioned from unchoked to choked

				// If the peer was working on any pieces, remove them from its activeRequests set
				// Also decrement activeRequestsTotals for any pieces that the peer was told to download
				// but didn't finished. 
				cont.removeUnfinishedWorkForPeer(peerInfo)

			} else {
				// The peer (presumably) transitioned from choked to unchoked

				// Create a slice of pieces sorted by rarity
				raritySlice := cont.createRaritySlice()

				// Send requests to the peer
				cont.sendRequestsToPeer(peerInfo, raritySlice)

			}



		case innerChan := <- cont.rxChans.peer.havePiece:

			var peerInfo *PeerInfo
			var exists bool

			// Receive one or more pieces over inner channel
			for piece := range innerChan {

				log.Printf("Controller : Run (Have Piece) : Received a HavePiece from %s for pieceNum %d", piece.peerName, piece.pieceNum)
				
				// Update the peers availability slice. 
				peerInfo, exists = cont.peers[piece.peerName]; 
				if !exists {
					log.Fatalf("Controller : Run (Have Piece) : Unable to process HavePiece for %s because it doesn't exist in the peers mapping", piece.peerName)
				} 

				if peerInfo.availablePieces[piece.pieceNum] {
					log.Fatalf("Controller : Run (Have Piece) : Received duplicate HavePiece from %s for piece number %d", peerInfo.peerName, piece.pieceNum)
				} 

				// Mark this peer as having this piece
				peerInfo.availablePieces[piece.pieceNum] = true

			}

			// This is either one or more HAVE messages sent for the initial peer bitfield, or it's
			// a single HAVE message sent because the peer has a new piece. In either case, we should 
			// attempt to download more pieces. 
			if !peerInfo.isChoked && len(peerInfo.activeRequests) < cont.maxSimultaneousDownloadsPerPeer {

				// Create a slice of pieces sorted by rarity
				raritySlice := cont.createRaritySlice()

				// Send requests to the peer

				cont.sendRequestsToPeer(peerInfo, raritySlice)

			}
		// === END OF MESSAGES FROM PEER === 




		case <- cont.t.Dying():
			return
		}
	}

}
