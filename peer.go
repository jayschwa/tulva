// Copyright 2013 Jari Takkala and Brian Dignan. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/binary"
	//"errors"
	"fmt"
	"io"
	"launchpad.net/tomb"
	"log"
	"net"
	"sort"
	//"strconv"
	"syscall"
	"time"
)

const pstr = "BitTorrent protocol"

// Message ID values 
const (
	MsgChoke int = iota
	MsgUnchoke
	MsgInterested
	MsgNotInterested
	MsgHave
	MsgBitfield
	MsgRequest
	MsgPiece
	MsgCancel
	MsgPort
)

// PeerTuple represents a single IP+port pair of a peer
type PeerTuple struct {
	IP   net.IP
	Port uint16
}

type Peer struct {
	conn           *net.TCPConn
	amChoking      bool
	amInterested   bool
	peerChoking    bool
	peerInterested bool
	ourBitfield    []bool
	peerBitfield   []bool
	initiator      bool
	peerID         []byte
	keepalive      <-chan time.Time // channel for sending keepalives
	lastTxKeepalive  time.Time
	lastRxKeepalive  time.Time
	read           chan []byte
	infoHash       []byte
	diskIOChans    diskIOPeerChans
	peerManagerChans peerManagerChans
	stats          PeerStats
	t              tomb.Tomb
}

type PeerStats struct {
	read int
	write int
	errors int
}

type PeerManager struct {
	peers        map[string]*Peer
	infoHash     []byte
	peerChans    peerManagerChans
	serverChans  serverPeerChans
	trackerChans trackerPeerChans
	diskIOChans  diskIOPeerChans
	t            tomb.Tomb
}

type peerManagerChans struct {
	deadPeer chan string
}

type PeerComms struct {
	peerName     string
	chans 		 ControllerPeerChans	
}

func NewPeerComms(peerName string, cpc ControllerPeerChans) *PeerComms {
	pc := new(PeerComms)
	pc.peerName = peerName
	pc.chans = cpc
	return pc
}

type PeerInfo struct {
	peerName        string
	isChoked        bool // The peer is connected but choked. Defaults to TRUE (choked)
	availablePieces []bool
	activeRequests  map[int]struct{}
	qtyPiecesNeeded int                   // The quantity of pieces that this peer has that we haven't yet downloaded.
	chans 			ControllerPeerChans
}

func NewPeerInfo(quantityOfPieces int, peerComms PeerComms) *PeerInfo {
	pi := new(PeerInfo)

	pi.peerName = peerComms.peerName
	pi.chans = peerComms.chans

	pi.isChoked = true // By default, a peer starts as being choked by the other side.
	pi.availablePieces = make([]bool, quantityOfPieces)
	pi.activeRequests = make(map[int]struct{})

	return pi
}

// Sent by the peer to controller indicating a 'choke' state change. It either went from unchoked to choked,
// or from choked to unchoked.
type PeerChokeStatus struct {
	peerName   string
	isChoked bool
}

type SortedPeers []*PeerInfo

func (sp SortedPeers) Less(i, j int) bool {
	return sp[i].qtyPiecesNeeded <= sp[j].qtyPiecesNeeded
}

func (sp SortedPeers) Swap(i, j int) {
	tmp := sp[i]
	sp[i] = sp[j]
	sp[j] = tmp
}

func (sp SortedPeers) Len() int {
	return len(sp)
}

func sortedPeersByQtyPiecesNeeded(peers map[string]*PeerInfo) SortedPeers {
	peerInfoSlice := make(SortedPeers, 0)

	for _, peerInfo := range peers {
		peerInfoSlice = append(peerInfoSlice, peerInfo)
	}
	sort.Sort(peerInfoSlice)

	return peerInfoSlice
}

func NewPeerManager(infoHash []byte, diskIOChans diskIOPeerChans, serverChans serverPeerChans, trackerChans trackerPeerChans) *PeerManager {
	pm := new(PeerManager)
	pm.infoHash = infoHash
	pm.diskIOChans = diskIOChans
	pm.serverChans = serverChans
	pm.trackerChans = trackerChans
	pm.peerChans.deadPeer = make(chan string)
	pm.peers = make(map[string]*Peer)
	return pm
}

func ConnectToPeer(peerTuple PeerTuple, connCh chan *net.TCPConn) {
	raddr := net.TCPAddr{peerTuple.IP, int(peerTuple.Port), ""}
	log.Println("Connecting to", raddr)
	conn, err := net.DialTCP("tcp4", nil, &raddr)
	if err != nil {
		if e, ok := err.(*net.OpError); ok {
			if e.Err == syscall.ECONNREFUSED {
				log.Println("ConnectToPeer : Connection Refused:", raddr)
				return
			}
		}
		log.Fatal(err)
	}
	log.Println("ConnectToPeer : Connected:", raddr)
	connCh <- conn
}

func NewPeer(infoHash []byte, initiator bool, diskIOChans diskIOPeerChans) *Peer {
	p := &Peer{infoHash: infoHash, amChoking: true, amInterested: false, peerChoking: true, peerInterested: false, initiator: initiator, diskIOChans: diskIOChans}
	p.read = make(chan []byte)
	return p
}

func constructMessage(id int, payload []byte) (msg []byte, err error) {
	msg = make([]byte, 4)

	// Store the length of payload + id in network byte order
	binary.BigEndian.PutUint32(msg, uint32(len(payload) + 1))
	msg = append(msg, byte(id))
	msg = append(msg, payload...)

	return
}

func (p *Peer) Reader() {
	log.Println("Peer : Reader : Started")

	buf := make([]byte, 1024)

	for {
		n, err := p.conn.Read(buf)
		if err != nil {
			if err == io.EOF {
				log.Println("Reader : EOF:", p.conn.RemoteAddr().String())
				return
			} else {
				if e, ok := err.(*net.OpError); ok {
					if e.Err == syscall.ECONNRESET {
						log.Println("Reader : Connection Reset:", p.conn.RemoteAddr().String())
						return
					}
				}
				log.Fatal(err)
			}
		}
		fmt.Printf("Read %d bytes\n", n)
		p.read <- buf
	}
}

func (p *Peer) sendHandshake() {
	log.Println("Peer : sendHandshake : Started")
	defer log.Println("Peer : sendHandshake : Completed")

	reserved := make([]byte, 8)
	buf := make([]byte, 0)
	buf = append(buf, byte(len(pstr)))
	buf = append(buf, []byte(pstr)...)
	buf = append(buf, reserved...)
	buf = append(buf, p.infoHash...)
	buf = append(buf, PeerID...)
	n, err := p.conn.Write(buf)
	if err != nil {
		log.Fatal(err)
	}
	p.stats.write += n
}

func (p *Peer) receiveHandshake() (error) {
	log.Println("Peer : receiveHandshake : Started")
	defer log.Println("Peer : receiveHandshake : Completed")

	buf := make([]byte, 1024)
	//p.conn.SetReadDeadline(time.Now().Add(time.Second * 1))
	n, err := p.conn.Read(buf)
	if err != nil {
		if err == io.EOF {
			log.Println("Reader : EOF:", p.conn.RemoteAddr().String())
			return err
		} else {
			if e, ok := err.(*net.OpError); ok {
				if e.Err == syscall.ECONNRESET {
					log.Println("Reader : Connection Reset:", p.conn.RemoteAddr().String())
					return err
				}
			}
			log.Fatal(err)
		}
	}
	p.stats.read += n

	pstrlen := len(pstr)
	if (buf[0] != byte(pstrlen)) {
		pstrerr := fmt.Sprintf("Unexpected length for pstrlen (wanted %d, got %d)", pstrlen, buf[0])
		log.Fatal(pstrerr)
	}
	offset := 1
	if !bytes.Equal(buf[offset:offset + pstrlen], []byte(pstr)) {
		pstrerr := fmt.Sprintf("Protocol mismtach: got %s, expected %s", buf[offset:offset + pstrlen], pstr)
		log.Fatal(pstrerr)
	}
	offset += pstrlen
	// ignore reserved bits for now
	offset += 8
	if !bytes.Equal(buf[offset:offset + 20], p.infoHash) {
		pstrerr := fmt.Sprintf("Invalid infoHash: got %x, expected %x", buf[offset:offset + 20], p.infoHash)
		log.Fatal(pstrerr)
	}
	offset += 20
	p.peerID = make([]byte, 20)
	copy(p.peerID, buf[offset:offset + 20])
	fmt.Printf("Handshake success with peer %s, ID %q\n", p.conn.RemoteAddr().String(), p.peerID)

	return nil
}

func (p *Peer) doHandshake() {
	if p.initiator {
		p.sendHandshake()
		p.receiveHandshake()
	} else {
		p.receiveHandshake()
		p.sendHandshake()
	}
}

func (p *Peer) Stop() error {
	log.Println("Peer : Stop : Stopping")
	p.t.Kill(nil)
	return p.t.Wait()
}

func (p *Peer) Run() {
	log.Println("Peer : Run : Started")
	defer log.Println("Peer : Run : Completed")

	p.doHandshake()
	go p.Reader()

	for {
		select {
		case <-p.keepalive:
		case <-p.read:
			fmt.Println("p.read")
		//case buf := <-p.read:
			//fmt.Println("Read from peer:", buf)
		case <-p.t.Dying():
			p.peerManagerChans.deadPeer <- p.conn.RemoteAddr().String()
			return
		}
	}
}

func (pm *PeerManager) Stop() error {
	log.Println("PeerManager : Stop : Stopping")
	pm.t.Kill(nil)
	return pm.t.Wait()
}

func (pm *PeerManager) Run() {
	log.Println("PeerManager : Run : Started")
	defer pm.t.Done()
	defer log.Println("PeerManager : Run : Completed")

	for {
		select {
		case peer := <-pm.trackerChans.peers:
			peerID := fmt.Sprintf("%s:%d", peer.IP.String(), peer.Port)
			_, ok := pm.peers[peerID]
			if !ok {
				// Construct the Peer object
				pm.peers[peerID] = NewPeer(pm.infoHash, true, pm.diskIOChans)
				go ConnectToPeer(peer, pm.serverChans.conns)
			}
		case conn := <-pm.serverChans.conns:
			_, ok := pm.peers[conn.RemoteAddr().String()]
			if !ok {
				// Construct the Peer object
				pm.peers[conn.RemoteAddr().String()] = NewPeer(pm.infoHash, false, pm.diskIOChans)
			}
			// Associate the connection with the peer object and start the peer
			pm.peers[conn.RemoteAddr().String()].conn = conn
			go pm.peers[conn.RemoteAddr().String()].Run()
		case peer := <-pm.peerChans.deadPeer:
			log.Printf("PeerManager : Deleting peer %s\n", peer)
			delete(pm.peers, peer)
		case <-pm.t.Dying():
			for _, peer := range pm.peers {
				peer.Stop()
			}
			return
		}
	}
}
